/*
 * Copyright (C) 2016-2018. ActionTech.
 * Based on: github.com/actiontech/kafkas, github.com/github/gh-ost .
 * License: MPL version 2: https://www.mozilla.org/en-US/MPL/2.0 .
 */

package mysql

import (
	gosql "database/sql"
	"encoding/json"
	"fmt"
	dcommon "github.com/actiontech/dtle/drivers/mysql/common"

	"github.com/actiontech/dtle/drivers/mysql/mysql/common"

	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"

	//"math"
	"bytes"
	"encoding/gob"

	//"encoding/base64"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/snappy"
	gonats "github.com/nats-io/go-nats"
	gomysql "github.com/siddontang/go-mysql/mysql"

	"container/heap"
	"context"
	"encoding/hex"
	"os"

	//models "github.com/actiontech/dtle/drivers/mysql/mysql"
	utils "github.com/actiontech/dtle/drivers/mysql/mysql/util"
	"github.com/actiontech/dtle/drivers/mysql/mysql/g"
	"github.com/actiontech/dtle/drivers/mysql/mysql/base"
	"github.com/actiontech/dtle/drivers/mysql/mysql/binlog"
	config "github.com/actiontech/dtle/drivers/mysql/mysql/config"
	umconf "github.com/actiontech/dtle/drivers/mysql/mysql/config"
	"github.com/actiontech/dtle/drivers/mysql/mysql/sql"
	hclog "github.com/hashicorp/go-hclog"
	not "github.com/nats-io/not.go"
	uuid "github.com/satori/go.uuid"
)

const (
	cleanupGtidExecutedLimit = 4096
	pingInterval             = 10 * time.Second
)
const (
	TaskStateComplete int = iota
	TaskStateRestart
	TaskStateDead
)

// from container/heap/example_intheap_test.go
type Int64PriQueue []int64

func (q Int64PriQueue) Len() int           { return len(q) }
func (q Int64PriQueue) Less(i, j int) bool { return q[i] < q[j] }
func (q Int64PriQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *Int64PriQueue) Push(x interface{}) {
	*q = append(*q, x.(int64))
}
func (q *Int64PriQueue) Pop() interface{} {
	old := *q
	n := len(old)
	x := old[n-1]
	*q = old[0 : n-1]
	return x
}

type applierTableItem struct {
	columns  *umconf.ColumnList
	psInsert []*gosql.Stmt
	psDelete []*gosql.Stmt
	psUpdate []*gosql.Stmt
}

func newApplierTableItem(parallelWorkers int) *applierTableItem {
	return &applierTableItem{
		columns:  nil,
		psInsert: make([]*gosql.Stmt, parallelWorkers),
		psDelete: make([]*gosql.Stmt, parallelWorkers),
		psUpdate: make([]*gosql.Stmt, parallelWorkers),
	}
}
func (ait *applierTableItem) Reset() {
	// TODO handle err of `.Close()`?
	closeStmts := func(stmts []*gosql.Stmt) {
		for i := range stmts {
			if stmts[i] != nil {
				stmts[i].Close()
				stmts[i] = nil
			}
		}
	}
	closeStmts(ait.psInsert)
	closeStmts(ait.psDelete)
	closeStmts(ait.psUpdate)

	ait.columns = nil
}

type mapSchemaTableItems map[string](map[string](*applierTableItem))

// Applier connects and writes the the applier-server, which is the server where
// write row data and apply binlog events onto the dest table.

type MtsManager struct {
	// If you set lastCommit = N, all tx with seqNum <= N must have been executed
	lastCommitted int64
	lastEnqueue   int64
	updated       chan struct{}
	shutdownCh    chan struct{}
	// SeqNum executed but not added to LC
	m          Int64PriQueue
	chExecuted chan int64
}

//  shutdownCh: close to indicate a shutdown
func NewMtsManager(shutdownCh chan struct{}) *MtsManager {
	return &MtsManager{
		lastCommitted: 0,
		updated:       make(chan struct{}, 1), // 1-buffered, see #211-7.1
		shutdownCh:    shutdownCh,
		m:             nil,
		chExecuted:    make(chan int64),
	}
}

//  This function must be called sequentially.
func (mm *MtsManager) WaitForAllCommitted() bool {
	for {
		if mm.lastCommitted == mm.lastEnqueue {
			return true
		}

		select {
		case <-mm.updated:
			// continue
		case <-mm.shutdownCh:
			return false
		}
	}
}

// block for waiting. return true for can_execute, false for abortion.
//  This function must be called sequentially.
func (mm *MtsManager) WaitForExecution(binlogEntry *binlog.BinlogEntry) bool {
	mm.lastEnqueue = binlogEntry.Coordinates.SeqenceNumber

	for {
		currentLC := atomic.LoadInt64(&mm.lastCommitted)
		if currentLC >= binlogEntry.Coordinates.LastCommitted {
			return true
		}

		// block until lastCommitted updated
		select {
		case <-mm.updated:
			// continue
		case <-mm.shutdownCh:
			return false
		}
	}
}

func (mm *MtsManager) LcUpdater() {
	for {
		select {
		case seqNum := <-mm.chExecuted:
			if seqNum <= mm.lastCommitted {
				// ignore it
			} else {
				heap.Push(&mm.m, seqNum)

				for mm.m.Len() > 0 {
					least := mm.m[0]
					if least == mm.lastCommitted+1 {
						heap.Pop(&mm.m)
						atomic.AddInt64(&mm.lastCommitted, 1)
						select {
						case mm.updated <- struct{}{}:
						default: // non-blocking
						}
					} else {
						break
					}
				}
			}

		case <-mm.shutdownCh:
			return
		}
	}
}

func (mm *MtsManager) Executed(binlogEntry *binlog.BinlogEntry) {
	mm.chExecuted <- binlogEntry.Coordinates.SeqenceNumber
}

type Applier struct {
	logger             hclog.Logger
	subject            string
	subjectUUID        uuid.UUID
	mysqlContext       *config.MySQLDriverConfig
	dbs                []*sql.Conn
	db                 *gosql.DB
	gtidExecuted       base.GtidSet
	currentCoordinates *CurrentCoordinates
	tableItems         mapSchemaTableItems

	rowCopyComplete     chan bool
	rowCopyCompleteFlag int64
	// copyRowsQueue should not be buffered; if buffered some non-damaging but
	//  excessive work happens at the end of the iteration as new copy-jobs arrive befroe realizing the copy is complete
	copyRowsQueue           chan *DumpEntry
	applyDataEntryQueue     chan *binlog.BinlogEntry
	applyBinlogTxQueue      chan *binlog.BinlogTx
	applyBinlogGroupTxQueue chan []*binlog.BinlogTx
	// only TX can be executed should be put into this chan
	applyBinlogMtsTxQueue chan *binlog.BinlogEntry
	lastAppliedBinlogTx   *binlog.BinlogTx

	natsConn *gonats.Conn
	waitCh   chan WaitResult
	wg       sync.WaitGroup

	shutdown     bool
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex

	mtsManager     *MtsManager
	printTps       bool
	txLastNSeconds uint32
	nDumpEntry     int64

	stubFullApplyDelay time.Duration
	storeManager       *dcommon.StoreManager
}

func NewApplier(ctx *common.ExecContext, cfg *umconf.MySQLDriverConfig, logger hclog.Logger, storeManager *dcommon.StoreManager) (*Applier, error) {
	cfg = cfg.SetDefault()
	/*entry := logger.WithFields(logrus.Fields{
		"job": ctx.Subject,
	})*/
	subjectUUID, err := uuid.FromString(ctx.Subject)
	if err != nil {
		logger.Error("job id is not a valid UUID: %v", err.Error())
		return nil, err
	}

	logger.Info("NewApplier", "subject", ctx.Subject)

	a := &Applier{
		logger:                  logger,
		subject:                 ctx.Subject,
		subjectUUID:             subjectUUID,
		mysqlContext:            cfg,
		currentCoordinates:      &CurrentCoordinates{},
		tableItems:              make(mapSchemaTableItems),
		rowCopyComplete:         make(chan bool, 1),
		copyRowsQueue:           make(chan *DumpEntry, 24),
		applyDataEntryQueue:     make(chan *binlog.BinlogEntry, cfg.ReplChanBufferSize*2),
		applyBinlogMtsTxQueue:   make(chan *binlog.BinlogEntry, cfg.ReplChanBufferSize*2),
		applyBinlogTxQueue:      make(chan *binlog.BinlogTx, cfg.ReplChanBufferSize*2),
		applyBinlogGroupTxQueue: make(chan []*binlog.BinlogTx, cfg.ReplChanBufferSize*2),
		waitCh:                  make(chan WaitResult, 1),
		shutdownCh:              make(chan struct{}),
		printTps:                os.Getenv(g.ENV_PRINT_TPS) != "",
		storeManager:            storeManager,
	}
	stubFullApplyDelayStr := os.Getenv(g.ENV_FULL_APPLY_DELAY)
	if stubFullApplyDelayStr == "" {
		a.stubFullApplyDelay = 0
	} else {
		delay, parseIntErr := strconv.ParseInt(stubFullApplyDelayStr, 10, 0)
		if parseIntErr != nil { // backward compatibility
			delay = 20
		}
		a.stubFullApplyDelay = time.Duration(delay) * time.Second
	}

	a.mtsManager = NewMtsManager(a.shutdownCh)
	go a.mtsManager.LcUpdater()
	return a, nil
}

func (a *Applier) MtsWorker(workerIndex int) {
	keepLoop := true

	for keepLoop {
		timer := time.NewTimer(pingInterval)
		select {
		case tx := <-a.applyBinlogMtsTxQueue:
			a.logger.Debug("mysql.applier: a binlogEntry MTS dequeue, worker: %v. GNO: %v",
				workerIndex, tx.Coordinates.GNO)
			if err := a.ApplyBinlogEvent(nil, workerIndex, tx); err != nil {
				a.onError(TaskStateDead, err) // TODO coordinate with other goroutine
				keepLoop = false
			} else {
				// do nothing
			}
			a.logger.Debug("mysql.applier: worker: %v. after ApplyBinlogEvent. GNO: %v",
				workerIndex, tx.Coordinates.GNO)
		case <-a.shutdownCh:
			keepLoop = false
		case <-timer.C:
			err := a.dbs[workerIndex].Db.PingContext(context.Background())
			if err != nil {
				a.logger.Error("mysql.applier. bad connection for mts worker. workerIndex: %v, err: %v",
					workerIndex, err)
			}
		}
		timer.Stop()
	}
}

// Run executes the complete apply logic.
func (a *Applier) Run() {
	if a.printTps {
		go func() {
			for {
				time.Sleep(5 * time.Second)
				n := atomic.SwapUint32(&a.txLastNSeconds, 0)
				a.logger.Info("mysql.applier: txLastNSeconds: %v", n)
			}
		}()
	}
	//a.logger.Debug("the connectionconfi host is ",a.mysqlContext.ConnectionConfig.Host)
//	a.logger.Info("mysql.applier: Apply binlog events to %s.%d", a.mysqlContext.ConnectionConfig.Host, a.mysqlContext.ConnectionConfig.Port)
	a.mysqlContext.StartTime = time.Now()
	if err := a.initDBConnections(); err != nil {
		a.onError(TaskStateDead, err)
		return
	}
	a.logger.Debug("initNatSubClien ",a.mysqlContext.ConnectionConfig.Host)
	if err := a.initNatSubClient(); err != nil {
		a.onError(TaskStateDead, err)
		return
	}
	a.logger.Debug("initiateStreaming is ",a.mysqlContext.ConnectionConfig.Host)
	if err := a.initiateStreaming(); err != nil {
		a.onError(TaskStateDead, err)
		return
	}

	for i := 0; i < a.mysqlContext.ParallelWorkers; i++ {
		go a.MtsWorker(i)
	}

	go a.executeWriteFuncs()
}

func (a *Applier) onApplyTxStructWithSuper(dbApplier *sql.Conn, binlogTx *binlog.BinlogTx) error {
	dbApplier.DbMutex.Lock()
	defer func() {
		_, err := sql.ExecNoPrepare(dbApplier.Db, `commit;set gtid_next='automatic'`)
		if err != nil {
			a.onError(TaskStateDead, err)
		}
		dbApplier.DbMutex.Unlock()
	}()

	if binlogTx.Fde != "" && dbApplier.Fde != binlogTx.Fde {
		dbApplier.Fde = binlogTx.Fde // IMO it would comare the internal pointer first
		_, err := sql.ExecNoPrepare(dbApplier.Db, binlogTx.Fde)
		if err != nil {
			return err
		}
	}

	_, err := sql.ExecNoPrepare(dbApplier.Db, fmt.Sprintf(`set gtid_next='%s:%d'`, binlogTx.SID, binlogTx.GNO))
	if err != nil {
		return err
	}
	var ignoreError error
	if binlogTx.Query == "" {
		_, err = sql.ExecNoPrepare(dbApplier.Db, `begin;commit`)
		if err != nil {
			return err
		}
	} else {
		_, err := sql.ExecNoPrepare(dbApplier.Db, binlogTx.Query)
		if err != nil {
			if !sql.IgnoreError(err) {
				//SELECT FROM_BASE64('')
				a.logger.Error("mysql.applier: exec gtid:[%s:%d] error: %v", binlogTx.SID, binlogTx.GNO, err)
				return err
			}
			a.logger.Warn("mysql.applier: exec gtid:[%s:%d],ignore error: %v", binlogTx.SID, binlogTx.GNO, err)
			ignoreError = err
		}
	}

	if ignoreError != nil {
		_, err := sql.ExecNoPrepare(dbApplier.Db, fmt.Sprintf(`commit;set gtid_next='%s:%d'`, binlogTx.SID, binlogTx.GNO))
		if err != nil {
			return err
		}
		_, err = sql.ExecNoPrepare(dbApplier.Db, `begin;commit`)
		if err != nil {
			return err
		}
	}
	return nil
}

// executeWriteFuncs writes data via applier: both the rowcopy and the events backlog.
// This is where the ghost table gets the data. The function fills the data single-threaded.
// Both event backlog and rowcopy events are polled; the backlog events have precedence.
func (a *Applier) executeWriteFuncs() {
	if a.mysqlContext.Gtid == "" {
		go func() {
			var stopLoop = false
			for !stopLoop {
				select {
				case copyRows := <-a.copyRowsQueue:
					if nil != copyRows {
						//time.Sleep(20 * time.Second) // #348 stub
						if err := a.ApplyEventQueries(a.db, copyRows); err != nil {
							a.onError(TaskStateDead, err)
						}
					}
					if atomic.LoadInt64(&a.nDumpEntry) < 0 {
						a.onError(TaskStateDead, fmt.Errorf("DTLE_BUG"))
					} else {
						atomic.AddInt64(&a.nDumpEntry, -1)
					}
				case <-a.rowCopyComplete:
					stopLoop = true
				case <-a.shutdownCh:
					stopLoop = true
				case <-time.After(10 * time.Second):
					a.logger.Debug("mysql.applier: no copyRows for 10s.")
				}
			}
		}()
	}

	if a.mysqlContext.Gtid == "" {
		a.logger.Info("mysql.applier: Operating until row copy is complete")
		a.mysqlContext.Stage =  StageSlaveWaitingForWorkersToProcessQueue
		for {
			if atomic.LoadInt64(&a.rowCopyCompleteFlag) == 1 && a.mysqlContext.TotalRowsCopied == a.mysqlContext.TotalRowsReplay {
				a.rowCopyComplete <- true
				a.logger.Info("mysql.applier: Rows copy complete.number of rows:%d", a.mysqlContext.TotalRowsReplay)
				a.mysqlContext.Gtid = a.currentCoordinates.RetrievedGtidSet
				a.mysqlContext.BinlogFile = a.currentCoordinates.File
				a.mysqlContext.BinlogPos = a.currentCoordinates.Position
				break
			}
			if a.shutdown {
				break
			}
			time.Sleep(time.Second)
		}
	}

	var dbApplier *sql.Conn

	stopMTSIncrLoop := false
	for !stopMTSIncrLoop {
		select {
		case <-a.shutdownCh:
			stopMTSIncrLoop = true
		case groupTx := <-a.applyBinlogGroupTxQueue:
			// this chan is used for homogeneous
			if len(groupTx) == 0 {
				continue
			}
			for idx, binlogTx := range groupTx {
				dbApplier = a.dbs[idx%a.mysqlContext.ParallelWorkers]
				go func(tx *binlog.BinlogTx) {
					a.wg.Add(1)
					if err := a.onApplyTxStructWithSuper(dbApplier, tx); err != nil {
						a.onError(TaskStateDead, err)
					}
					a.wg.Done()
				}(binlogTx)
			}
			a.wg.Wait() // Waiting for all goroutines to finish

			if !a.shutdown {
				a.lastAppliedBinlogTx = groupTx[len(groupTx)-1]
				if a.mysqlContext.Gtid != "" {
					sp := strings.Split(a.mysqlContext.Gtid, ",")

					if strings.Split(sp[len(sp)-1], ":")[0] == a.lastAppliedBinlogTx.SID || strings.Split(sp[len(sp)-1], ":")[0] == "\n"+a.lastAppliedBinlogTx.SID {
						sp[len(sp)-1] = fmt.Sprintf("%s:1-%d", a.lastAppliedBinlogTx.SID, a.lastAppliedBinlogTx.GNO)
					}
					var rgtid string
					for _, gtid := range sp {
						rgtid = rgtid + "," + gtid
					}
					a.mysqlContext.Gtid = rgtid[1:]
				} else {
					a.mysqlContext.Gtid = fmt.Sprintf("%s:1-%d", a.lastAppliedBinlogTx.SID, a.lastAppliedBinlogTx.GNO)
				}
				// a.mysqlContext.BinlogPos = // homogeneous obsolete. not implementing.
			}
		case <-time.After(1 * time.Second):
			// do nothing
		}
	}
}

func (a *Applier) initNatSubClient() (err error) {
	natsAddr := fmt.Sprintf("nats://%s", a.mysqlContext.NatsAddr)
	sc, err := gonats.Connect(natsAddr)
	if err != nil {
		a.logger.Error("mysql.applier: Can't connect nats server %v. make sure a nats streaming server is running.%v", natsAddr, err)
		return err
	}
	a.logger.Debug("mysql.applier: Connect nats server %v", natsAddr)
	a.natsConn = sc
	return nil
}
func DecodeDumpEntry(data []byte) (entry *DumpEntry, err error) {
	msg, err := snappy.Decode(nil, data)
	if err != nil {
		return nil, err
	}

	entry = &DumpEntry{}
	n, err := entry.Unmarshal(msg)
	if err != nil {
		return nil, err
	}
	if n != uint64(len(msg)) {
		return nil, fmt.Errorf("DumpEntry.Unmarshal: not all consumed. data: %v, consumed: %v",
			len(msg), n)
	}
	return entry, nil
}

// Decode
func Decode(data []byte, vPtr interface{}) (err error) {
	msg, err := snappy.Decode(nil, data)
	if err != nil {
		return err
	}

	return gob.NewDecoder(bytes.NewBuffer(msg)).Decode(vPtr)
}

func (a *Applier) setTableItemForBinlogEntry(binlogEntry *binlog.BinlogEntry) error {
	var err error
	for i := range binlogEntry.Events {
		dmlEvent := &binlogEntry.Events[i]
		switch dmlEvent.DML {
		case binlog.NotDML:
			// do nothing
		default:
			tableItem := a.getTableItem(dmlEvent.DatabaseName, dmlEvent.TableName)
			if tableItem.columns == nil {
				a.logger.Debug("mysql.applier: get tableColumns %v.%v", dmlEvent.DatabaseName, dmlEvent.TableName)
				tableItem.columns, err = base.GetTableColumns(a.db, dmlEvent.DatabaseName, dmlEvent.TableName)
				if err != nil {
					a.logger.Error("mysql.applier. GetTableColumns error. err: %v", err)
					return err
				}
				err = base.ApplyColumnTypes(a.db, dmlEvent.DatabaseName, dmlEvent.TableName, tableItem.columns)
				if err != nil {
					a.logger.Error("mysql.applier. ApplyColumnTypes error. err: %v", err)
					return err
				}
			} else {
				a.logger.Debug("mysql.applier: reuse tableColumns %v.%v", dmlEvent.DatabaseName, dmlEvent.TableName)
			}
			dmlEvent.TableItem = tableItem
		}
	}
	return nil
}

func (a *Applier) cleanGtidExecuted(sid uuid.UUID, intervalStr string) error {
	a.logger.Debug("mysql.applier. incr. cleanup before WaitForExecution")
	if !a.mtsManager.WaitForAllCommitted() {
		return nil // shutdown
	}
	a.logger.Debug("mysql.applier. incr. cleanup after WaitForExecution")

	// The TX is unnecessary if we first insert and then delete.
	// However, consider `binlog_group_commit_sync_delay > 0`,
	// `begin; delete; insert; commit;` (1 TX) is faster than `insert; delete;` (2 TX)
	dbApplier := a.dbs[0]
	tx, err := dbApplier.Db.BeginTx(context.Background(), &gosql.TxOptions{})
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()

	_, err = dbApplier.PsDeleteExecutedGtid.Exec(sid.Bytes())
	if err != nil {
		return err
	}

	a.logger.Debug("mysql.applier: compactation gtid. new interval: %v", intervalStr)
	_, err = dbApplier.PsInsertExecutedGtid.Exec(sid.Bytes(), intervalStr)
	if err != nil {
		return err
	}
	return nil
}

func (a *Applier) heterogeneousReplay() {
	var err error
	stopSomeLoop := false
	prevDDL := false
	var ctx context.Context
	for !stopSomeLoop {
		select {
		case binlogEntry := <-a.applyDataEntryQueue:
			if nil == binlogEntry {
				continue
			}
			spanContext := binlogEntry.SpanContext
			span := opentracing.GlobalTracer().StartSpan("dest use binlogEntry  ", opentracing.FollowsFrom(spanContext))
			ctx = opentracing.ContextWithSpan(ctx, span)
			a.logger.Debug("mysql.applier: a binlogEntry. remaining: %v. gno: %v, lc: %v, seq: %v",
				len(a.applyDataEntryQueue), binlogEntry.Coordinates.GNO,
				binlogEntry.Coordinates.LastCommitted, binlogEntry.Coordinates.SeqenceNumber)

			if binlogEntry.Coordinates.OSID == a.mysqlContext.MySQLServerUuid {
				a.logger.Debug("mysql.applier: skipping a kafkas tx. osid: %v", binlogEntry.Coordinates.OSID)
				continue
			}
			// region TestIfExecuted
			if a.gtidExecuted == nil {
				// udup crash recovery or never executed
				a.gtidExecuted, err = base.SelectAllGtidExecuted(a.db, a.subjectUUID)
				if err != nil {
					a.onError(TaskStateDead, err)
					return
				}
			}
			txSid := binlogEntry.Coordinates.GetSid()

			gtidSetItem, hasSid := a.gtidExecuted[binlogEntry.Coordinates.SID]
			if !hasSid {
				gtidSetItem = &base.GtidExecutedItem{}
				a.gtidExecuted[binlogEntry.Coordinates.SID] = gtidSetItem
			}
			if base.IntervalSlicesContainOne(gtidSetItem.Intervals, binlogEntry.Coordinates.GNO) {
				// entry executed
				a.logger.Debug("mysql.applier: skip an executed tx: %v:%v", txSid, binlogEntry.Coordinates.GNO)
				continue
			}
			// endregion
			// this must be after duplication check
			var rotated bool
			if a.currentCoordinates.File == binlogEntry.Coordinates.LogFile {
				rotated = false
			} else {
				rotated = true
				a.currentCoordinates.File = binlogEntry.Coordinates.LogFile
			}

			a.logger.Debug("mysql.applier. gtidSetItem.NRow: %v", gtidSetItem.NRow)
			if gtidSetItem.NRow >= cleanupGtidExecutedLimit {
				err = a.cleanGtidExecuted(binlogEntry.Coordinates.SID, base.StringInterval(gtidSetItem.Intervals))
				if err != nil {
					a.onError(TaskStateDead, err)
					return
				}
				gtidSetItem.NRow = 1
			}

			thisInterval := gomysql.Interval{Start: binlogEntry.Coordinates.GNO, Stop: binlogEntry.Coordinates.GNO + 1}

			gtidSetItem.NRow += 1
			// TODO normalize may affect oringinal intervals
			newInterval := append(gtidSetItem.Intervals, thisInterval).Normalize()
			// TODO this is assigned before real execution
			gtidSetItem.Intervals = newInterval
			if binlogEntry.Coordinates.SeqenceNumber == 0 {
				// MySQL 5.6: non mts
				err := a.setTableItemForBinlogEntry(binlogEntry)
				if err != nil {
					a.onError(TaskStateDead, err)
					return
				}
				if err := a.ApplyBinlogEvent(ctx, 0, binlogEntry); err != nil {
					a.onError(TaskStateDead, err)
					return
				}
			} else {
				if rotated {
					a.logger.Debug("mysql.applier: binlog rotated to %v", a.currentCoordinates.File)
					if !a.mtsManager.WaitForAllCommitted() {
						return // shutdown
					}
					a.mtsManager.lastCommitted = 0
					a.mtsManager.lastEnqueue = 0
					if len(a.mtsManager.m) != 0 {
						a.logger.Warn("DTLE_BUG: len(a.mtsManager.m) should be 0")
					}
				}
				// If there are TXs skipped by udup source-side
				for a.mtsManager.lastEnqueue+1 < binlogEntry.Coordinates.SeqenceNumber {
					a.mtsManager.lastEnqueue += 1
					a.mtsManager.chExecuted <- a.mtsManager.lastEnqueue
				}
				hasDDL := func() bool {
					for i := range binlogEntry.Events {
						dmlEvent := &binlogEntry.Events[i]
						switch dmlEvent.DML {
						case binlog.NotDML:
							return true
						default:
						}
					}
					return false
				}()
				// DDL must be executed separatedly
				if hasDDL || prevDDL {
					a.logger.Debug("mysql.applier: gno: %v MTS found DDL(%v,%v). WaitForAllCommitted",
						binlogEntry.Coordinates.GNO, hasDDL, prevDDL)
					if !a.mtsManager.WaitForAllCommitted() {
						return // shutdown
					}
				}
				if hasDDL {
					prevDDL = true
				} else {
					prevDDL = false
				}

				if !a.mtsManager.WaitForExecution(binlogEntry) {
					return // shutdown
				}
				a.logger.Debug("mysql.applier: a binlogEntry MTS enqueue. gno: %v", binlogEntry.Coordinates.GNO)
				err = a.setTableItemForBinlogEntry(binlogEntry)
				if err != nil {
					a.onError(TaskStateDead, err)
					return
				}
				binlogEntry.SpanContext = span.Context()
				a.applyBinlogMtsTxQueue <- binlogEntry
			}
			span.Finish()
			if !a.shutdown {
				// TODO what is this used for?
				if a.mysqlContext.Gtid != "" {
					sp := strings.Split(a.mysqlContext.Gtid, ",")
					if strings.Split(sp[len(sp)-1], ":")[0] == txSid || strings.Split(sp[len(sp)-1], ":")[0] == "\n"+txSid {
						sp[len(sp)-1] = fmt.Sprintf("%s:1-%d", txSid, binlogEntry.Coordinates.GNO)
					}
					var rgtid string
					for _, gtid := range sp {
						rgtid = rgtid + "," + gtid
					}
					a.mysqlContext.Gtid = rgtid[1:]
				} else {
					a.mysqlContext.Gtid = fmt.Sprintf("%s:1-%d", txSid, binlogEntry.Coordinates.GNO)
				}
				if a.mysqlContext.BinlogFile != binlogEntry.Coordinates.LogFile {
					a.mysqlContext.BinlogFile = binlogEntry.Coordinates.LogFile
					a.publishProgress()
				}
				a.mysqlContext.BinlogPos = binlogEntry.Coordinates.LogPos
			}
		case <-time.After(10 * time.Second):
			a.logger.Debug("mysql.applier: no binlogEntry for 10s")
		case <-a.shutdownCh:
			stopSomeLoop = true
		}
	}
}
func (a *Applier) homogeneousReplay() {
	var lastCommitted int64
	var err error
	//timeout := time.After(100 * time.Millisecond)
	groupTx := []*binlog.BinlogTx{}
OUTER:
	for {
		select {
		case binlogTx := <-a.applyBinlogTxQueue:
			if nil == binlogTx {
				continue
			}
			if a.mysqlContext.MySQLServerUuid == binlogTx.SID {
				continue
			}
			if a.mysqlContext.ParallelWorkers <= 1 {
				if err = a.onApplyTxStructWithSuper(a.dbs[0], binlogTx); err != nil {
					a.onError(TaskStateDead, err)
					break OUTER
				}

				if !a.shutdown {
					a.lastAppliedBinlogTx = binlogTx
					if a.mysqlContext.Gtid != "" {
						sp := strings.Split(a.mysqlContext.Gtid, ",")
						if strings.Split(sp[len(sp)-1], ":")[0] == a.lastAppliedBinlogTx.SID || strings.Split(sp[len(sp)-1], ":")[0] == "\n"+a.lastAppliedBinlogTx.SID {
							sp[len(sp)-1] = fmt.Sprintf("%s:1-%d", a.lastAppliedBinlogTx.SID, a.lastAppliedBinlogTx.GNO)
						}
						var rgtid string
						for _, gtid := range sp {
							rgtid = rgtid + "," + gtid
						}
						a.mysqlContext.Gtid = rgtid[1:]
					} else {
						a.mysqlContext.Gtid = fmt.Sprintf("%s:1-%d", a.lastAppliedBinlogTx.SID, a.lastAppliedBinlogTx.GNO)
					}
					// a.mysqlContext.BinlogPos = // homogeneous obsolete. not implementing.
				}
			} else {
				if binlogTx.LastCommitted == lastCommitted {
					groupTx = append(groupTx, binlogTx)
				} else {
					if len(groupTx) != 0 {
						a.applyBinlogGroupTxQueue <- groupTx
						groupTx = []*binlog.BinlogTx{}
					}
					groupTx = append(groupTx, binlogTx)
				}
				lastCommitted = binlogTx.LastCommitted
			}
		case <-time.After(100 * time.Millisecond):
			if len(groupTx) != 0 {
				a.applyBinlogGroupTxQueue <- groupTx
				groupTx = []*binlog.BinlogTx{}
			}
		case <-a.shutdownCh:
			break OUTER
		}
	}
}

// initiateStreaming begins treaming of binary log events and registers listeners for such events
func (a *Applier) initiateStreaming() error {
	a.mysqlContext.MarkRowCopyStartTime()
	a.logger.Debug("mysql.applier: nats subscribe")
	tracer := opentracing.GlobalTracer()
	_, err := a.natsConn.Subscribe(fmt.Sprintf("%s_full", a.subject), func(m *gonats.Msg) {
		a.logger.Debug("mysql.applier: full. recv a msg. copyRowsQueue: %v", len(a.copyRowsQueue))
		t := not.NewTraceMsg(m)
		// Extract the span context from the request message.
		sc, err := tracer.Extract(opentracing.Binary, t)
		if err != nil {
			a.logger.Debug("applier:get data")
		}
		// Setup a span referring to the span context of the incoming NATS message.
		replySpan := tracer.StartSpan("Service Responder", ext.SpanKindRPCServer, ext.RPCServerOption(sc))
		ext.MessageBusDestination.Set(replySpan, m.Subject)
		defer replySpan.Finish()
		dumpData, err := DecodeDumpEntry(t.Bytes())
		if err != nil {
			a.onError(TaskStateDead, err)
			// TODO return?
		}

		timer := time.NewTimer(DefaultConnectWait / 2)
		atomic.AddInt64(&a.nDumpEntry, 1) // this must be increased before enqueuing
		select {
		case a.copyRowsQueue <- dumpData:
			a.logger.Debug("mysql.applier: full. enqueue")
			timer.Stop()
			a.mysqlContext.Stage =  StageSlaveWaitingForWorkersToProcessQueue
			if err := a.natsConn.Publish(m.Reply, nil); err != nil {
				a.onError(TaskStateDead, err)
			}
			a.logger.Debug("mysql.applier. full. after publish nats reply")
			atomic.AddInt64(&a.mysqlContext.RowsEstimate, dumpData.TotalCount)
		case <-timer.C:
			atomic.AddInt64(&a.nDumpEntry, -1)

			a.logger.Debug("mysql.applier. full. discarding entries")
			a.mysqlContext.Stage =  StageSlaveWaitingForWorkersToProcessQueue
		}
	})
	/*if err := sub.SetPendingLimits(a.mysqlContext.MsgsLimit, a.mysqlContext.BytesLimit); err != nil {
		return err
	}*/

	_, err = a.natsConn.Subscribe(fmt.Sprintf("%s_full_complete", a.subject), func(m *gonats.Msg) {
		dumpData := &dumpStatResult{}
		t := not.NewTraceMsg(m)
		// Extract the span context from the request message.
		sc, err := tracer.Extract(opentracing.Binary, t)
		if err != nil {
			a.logger.Debug("applier:get data")
		}
		// Setup a span referring to the span context of the incoming NATS message.
		replySpan := tracer.StartSpan("Service Responder", ext.SpanKindRPCServer, ext.RPCServerOption(sc))
		ext.MessageBusDestination.Set(replySpan, m.Subject)
		defer replySpan.Finish()
		if err := Decode(t.Bytes(), dumpData); err != nil {
			a.onError(TaskStateDead, err)
		}
		a.currentCoordinates.RetrievedGtidSet = dumpData.Gtid
		a.currentCoordinates.File = dumpData.LogFile
		a.currentCoordinates.Position = dumpData.LogPos

		a.mysqlContext.Stage =  StageSlaveWaitingForWorkersToProcessQueue

		for atomic.LoadInt64(&a.nDumpEntry) != 0 {
			a.logger.Debug("mysql.applier. nDumpEntry is not zero, waiting. %v", a.nDumpEntry)
			time.Sleep(1 * time.Second)
			if a.shutdown {
				return
			}
		}

		a.logger.Debug("mysql.applier. ack full_complete")
		if err := a.natsConn.Publish(m.Reply, nil); err != nil {
			a.onError(TaskStateDead, err)
		}
		atomic.AddInt64(&a.mysqlContext.TotalRowsCopied, dumpData.TotalCount)
		atomic.StoreInt64(&a.rowCopyCompleteFlag, 1)
	})
	if err != nil {
		return err
	}

	if a.mysqlContext.ApproveHeterogeneous {
		_, err := a.natsConn.Subscribe(fmt.Sprintf("%s_incr_hete", a.subject), func(m *gonats.Msg) {
			var binlogEntries binlog.BinlogEntries
			t := not.NewTraceMsg(m)
			// Extract the span context from the request message.
			spanContext, err := tracer.Extract(opentracing.Binary, t)
			if err != nil {
				a.logger.Debug("applier:get data")
			}
			// Setup a span referring to the span context of the incoming NATS message.
			replySpan := tracer.StartSpan("nast : dest to get data  ", ext.SpanKindRPCServer, ext.RPCServerOption(spanContext))
			ext.MessageBusDestination.Set(replySpan, m.Subject)
			defer replySpan.Finish()
			if err := Decode(t.Bytes(), &binlogEntries); err != nil {
				a.onError(TaskStateDead, err)
			}

			nEntries := len(binlogEntries.Entries)

			handled := false
			for i := 0; !handled && (i < DefaultConnectWaitSecond/2); i++ {
				vacancy := cap(a.applyDataEntryQueue) - len(a.applyDataEntryQueue)
				a.logger.Debug("applier. incr. nEntries: %v, vacancy: %v", nEntries, vacancy)
				if vacancy < nEntries {
					a.logger.Debug("applier. incr. wait 1s for applyDataEntryQueue")
					time.Sleep(1 * time.Second) // It will wait an second at the end, but seems no hurt.
				} else {
					a.logger.Debug("applier. incr. applyDataEntryQueue enqueue")
					for _, binlogEntry := range binlogEntries.Entries {
						binlogEntry.SpanContext = replySpan.Context()
						a.applyDataEntryQueue <- binlogEntry
						a.currentCoordinates.RetrievedGtidSet = binlogEntry.Coordinates.GetGtidForThisTx()
						atomic.AddInt64(&a.mysqlContext.DeltaEstimate, 1)
					}
					a.mysqlContext.Stage =  StageWaitingForMasterToSendEvent

					if err := a.natsConn.Publish(m.Reply, nil); err != nil {
						a.onError(TaskStateDead, err)
					}
					a.logger.Debug("applier. incr. ack-recv. nEntries: %v", nEntries)

					handled = true
				}
			}
			if !handled {
				// discard these entries
				a.logger.Debug("applier. incr. discarding entries")
				a.mysqlContext.Stage =  StageWaitingForMasterToSendEvent
			}
		})
		if err != nil {
			return err
		}

		go a.heterogeneousReplay()
	} else {
		_, err := a.natsConn.Subscribe(fmt.Sprintf("%s_incr", a.subject), func(m *gonats.Msg) {
			var binlogTx []*binlog.BinlogTx
			t := not.NewTraceMsg(m)
			// Extract the span context from the request message.
			sc, err := tracer.Extract(opentracing.Binary, t)
			if err != nil {
				a.logger.Debug("applier:get data")
			}
			// Setup a span referring to the span context of the incoming NATS message.
			replySpan := tracer.StartSpan(a.subject, ext.SpanKindRPCServer, ext.RPCServerOption(sc))
			ext.MessageBusDestination.Set(replySpan, m.Subject)
			defer replySpan.Finish()
			if err := Decode(t.Bytes(), &binlogTx); err != nil {
				a.onError(TaskStateDead, err)
			}
			for _, tx := range binlogTx {
				a.applyBinlogTxQueue <- tx
			}
			if err := a.natsConn.Publish(m.Reply, nil); err != nil {
				a.onError(TaskStateDead, err)
			}
		})
		if err != nil {
			return err
		}
		/*if err := sub.SetPendingLimits(a.mysqlContext.MsgsLimit, a.mysqlContext.BytesLimit); err != nil {
			return err
		}*/
		go a.homogeneousReplay()
	}

	return nil
}

func (a *Applier) publishProgress() {
	retry := 0
	keep := true
	for keep {
		a.logger.Debug("*** applier.publishProgress. retry %v, file %v", retry, a.mysqlContext.BinlogFile)
		_, err := a.natsConn.Request(fmt.Sprintf("%s_progress", a.subject), []byte(a.mysqlContext.BinlogFile), 10*time.Second)
		if err == nil {
			keep = false
		} else {
			if err == gonats.ErrTimeout {
				a.logger.Debug("retry: %v file: %v applier.publishProgress. timeout", retry, a.mysqlContext.BinlogFile)
				break
			} else {
				a.logger.Debug("retry: %v file: %v applier.publishProgress. unknown error", retry, a.mysqlContext.BinlogFile)

			}
			retry += 1
			if retry < 5 {
				time.Sleep(1 * time.Second)
			} else {
				keep = false
			}
		}
	}
}

func (a *Applier) initDBConnections() (err error) {
	applierUri := a.mysqlContext.ConnectionConfig.GetDBUri()
	if a.db, err = sql.CreateDB(applierUri); err != nil {
		return err
	}
	a.db.SetMaxOpenConns(10 + a.mysqlContext.ParallelWorkers)
	if a.dbs, err = sql.CreateConns(a.db, a.mysqlContext.ParallelWorkers); err != nil {
		a.logger.Debug("beging connetion mysql 2 create conns err")
		return err
	}

	if err := a.validateConnection(a.db); err != nil {
		return err
	}
	a.logger.Debug("beging connetion mysql 4 validate  serverid")
	if err := a.validateServerUUID(); err != nil {
		return err
	}
	a.logger.Debug("beging connetion mysql 5 validate  grants")
	if err := a.validateGrants(); err != nil {
		a.logger.Error("mysql.applier: Unexpected error on validateGrants, got %v", err)
		return err
	}
	a.logger.Debug("mysql.applier. after validateGrants")
	if err := a.validateAndReadTimeZone(); err != nil {
		return err
	}
	a.logger.Debug("mysql.applier. after validateAndReadTimeZone")

	if a.mysqlContext.ApproveHeterogeneous {
		if err := a.createTableGtidExecutedV3(); err != nil {
			return err
		}
		a.logger.Debug("mysql.applier. after createTableGtidExecutedV2")

		for i := range a.dbs {
			a.dbs[i].PsDeleteExecutedGtid, err = a.dbs[i].Db.PrepareContext(context.Background(), fmt.Sprintf("delete from %v.%v where job_uuid = unhex('%s') and source_uuid = ?",
				g.DtleSchemaName, g.GtidExecutedTableV3, hex.EncodeToString(a.subjectUUID.Bytes())))
			if err != nil {
				return err
			}
			a.dbs[i].PsInsertExecutedGtid, err = a.dbs[i].Db.PrepareContext(context.Background(), fmt.Sprintf("replace into %v.%v "+
				"(job_uuid,source_uuid,interval_gtid) "+
				"values (unhex('%s'), ?, ?)",
				g.DtleSchemaName, g.GtidExecutedTableV3,
				hex.EncodeToString(a.subjectUUID.Bytes())))
			if err != nil {
				return err
			}

		}
		a.logger.Debug("mysql.applier. after prepare stmt for gtid_executed table")
	}

	a.logger.Info("mysql.applier: Initiated on %s:%d, version %+v", a.mysqlContext.ConnectionConfig.Host, a.mysqlContext.ConnectionConfig.Port, a.mysqlContext.MySQLVersion)
	return nil
}

func (a *Applier) validateServerUUID() error {
	query := `SELECT @@SERVER_UUID`
	if err := a.db.QueryRow(query).Scan(&a.mysqlContext.MySQLServerUuid); err != nil {
		return err
	}
	return nil
}

// validateConnection issues a simple can-connect to MySQL
func (a *Applier) validateConnection(db *gosql.DB) error {
	query := `select @@global.version`
	if err := db.QueryRow(query).Scan(&a.mysqlContext.MySQLVersion); err != nil {
		return err
	}
	// Match the version string (from SELECT VERSION()).
	if strings.HasPrefix(a.mysqlContext.MySQLVersion, "5.6") {
		a.mysqlContext.ParallelWorkers = 1
	}
	a.logger.Debug("mysql.applier: Connection validated on %s:%d", a.mysqlContext.ConnectionConfig.Host, a.mysqlContext.ConnectionConfig.Port)
	return nil
}

// validateGrants verifies the user by which we're executing has necessary grants
// to do its thang.
func (a *Applier) validateGrants() error {
	if a.mysqlContext.SkipPrivilegeCheck {
		a.logger.Debug("mysql.applier: skipping priv check")
		return nil
	}
	query := `show grants for current_user()`
	foundAll := false
	foundSuper := false
	foundDBAll := false

	err := sql.QueryRowsMap(a.db, query, func(rowMap sql.RowMap) error {
		for _, grantData := range rowMap {
			grant := grantData.String
			if strings.Contains(grant, `GRANT ALL PRIVILEGES ON`) {
				foundAll = true
			}
			if strings.Contains(grant, `SUPER`) && strings.Contains(grant, ` ON *.*`) {
				foundSuper = true
			}
			if strings.Contains(grant, fmt.Sprintf("GRANT ALL PRIVILEGES ON `%v`.`%v`",
				g.DtleSchemaName, g.GtidExecutedTableV3)) {
				foundDBAll = true
			}
			if base.StringContainsAll(grant, `ALTER`, `CREATE`, `DELETE`, `DROP`, `INDEX`, `INSERT`, `SELECT`, `TRIGGER`, `UPDATE`, ` ON`) {
				foundDBAll = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.mysqlContext.HasSuperPrivilege = foundSuper

	if foundAll {
		a.logger.Info("mysql.applier: User has ALL privileges")
		return nil
	}
	if foundSuper {
		a.logger.Info("mysql.applier: User has SUPER privileges")
		return nil
	}
	if foundDBAll {
		a.logger.Info("User has ALL privileges on *.*")
		return nil
	}
	a.logger.Debug("mysql.applier: Privileges: super: %t, ALL on *.*: %t", foundSuper, foundAll)
	//return fmt.Error("user has insufficient privileges for applier. Needed: SUPER|ALL on *.*")
	return nil
}

// validateAndReadTimeZone potentially reads server time-zone
func (a *Applier) validateAndReadTimeZone() error {
	query := `select @@global.time_zone`
	if err := a.db.QueryRow(query).Scan(&a.mysqlContext.TimeZone); err != nil {
		return err
	}

	a.logger.Info("mysql.applier: Will use time_zone='%s' on applier", a.mysqlContext.TimeZone)
	return nil
}
func (a *Applier) migrateGtidExecutedV2toV3() error {
	a.logger.Info(`migrateGtidExecutedV2toV3 starting`)

	var err error
	var query string

	logErr := func(query string, err error) {
		a.logger.Error(`migrateGtidExecutedV2toV3 failed. manual intervention might be required. query: %v. err: %v`,
			query, err)
	}

	query = fmt.Sprintf("alter table %v.%v rename to %v.%v",
		g.DtleSchemaName, g.GtidExecutedTableV2, g.DtleSchemaName, g.GtidExecutedTempTable2To3)
	_, err = a.db.Exec(query)
	if err != nil {
		logErr(query, err)
		return err
	}

	query = fmt.Sprintf("alter table %v.%v modify column interval_gtid longtext",
		g.DtleSchemaName, g.GtidExecutedTempTable2To3)
	_, err = a.db.Exec(query)
	if err != nil {
		logErr(query, err)
		return err
	}

	query = fmt.Sprintf("alter table %v.%v rename to %v.%v",
		g.DtleSchemaName, g.GtidExecutedTempTable2To3, g.DtleSchemaName, g.GtidExecutedTableV3)
	_, err = a.db.Exec(query)
	if err != nil {
		logErr(query, err)
		return err
	}

	a.logger.Info(`migrateGtidExecutedV2toV3 done`)

	return nil
}
func (a *Applier) createTableGtidExecutedV3() error {
	if result, err := sql.QueryResultData(a.db, fmt.Sprintf("SHOW TABLES FROM %v LIKE '%v%%'",
		g.DtleSchemaName, g.GtidExecutedTempTablePrefix)); nil == err && len(result) > 0 {
		return fmt.Errorf("GtidExecutedTempTable exists. require manual intervention")
	}

	if result, err := sql.QueryResultData(a.db, fmt.Sprintf("SHOW TABLES FROM %v LIKE '%v%%'",
		g.DtleSchemaName, g.GtidExecutedTablePrefix)); nil == err && len(result) > 0 {
		if len(result) > 1 {
			return fmt.Errorf("multiple GtidExecutedTable exists, while at most one is allowed. require manual intervention")
		} else {
			if len(result[0]) < 1 {
				return fmt.Errorf("mysql error: expect 1 column for 'SHOW TABLES' query")
			}
			switch result[0][0].String {
			case g.GtidExecutedTableV2:
				err = a.migrateGtidExecutedV2toV3()
				if err != nil {
					return err
				}
			case g.GtidExecutedTableV3:
				return nil
			default:
				return fmt.Errorf("newer GtidExecutedTable exists, which is unrecognized by this verion. require manual intervention")
			}
		}
	}

	a.logger.Debug("mysql.applier. after show gtid_executed table")

	query := fmt.Sprintf(`
			CREATE DATABASE IF NOT EXISTS %v;
		`, g.DtleSchemaName)
	if _, err := a.db.Exec(query); err != nil {
		return err
	}
	a.logger.Debug("mysql.applier. after create kafkas schema")

	query = fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %v.%v (
				job_uuid binary(16) NOT NULL COMMENT 'unique identifier of job',
				source_uuid binary(16) NOT NULL COMMENT 'uuid of the source where the transaction was originally executed.',
				interval_gtid longtext NOT NULL COMMENT 'number of interval.'
			);
		`, g.DtleSchemaName, g.GtidExecutedTableV3)
	if _, err := a.db.Exec(query); err != nil {
		return err
	}
	a.logger.Debug("mysql.applier. after create gtid_executed table")

	return nil
}

func (a *Applier) getTableItem(schema string, table string) *applierTableItem {
	schemaItem, ok := a.tableItems[schema]
	if !ok {
		schemaItem = make(map[string]*applierTableItem)
		a.tableItems[schema] = schemaItem
	}

	tableItem, ok := schemaItem[table]
	if !ok {
		tableItem = newApplierTableItem(a.mysqlContext.ParallelWorkers)
		schemaItem[table] = tableItem
	}

	return tableItem
}

// buildDMLEventQuery creates a query to operate on the ghost table, based on an intercepted binlog
// event entry on the original table.
func (a *Applier) buildDMLEventQuery(dmlEvent binlog.DataEvent, workerIdx int, spanContext opentracing.SpanContext) (stmt *gosql.Stmt, query string, args []interface{}, rowsDelta int64, err error) {
	// Large piece of code deleted here. See git annotate.
	tableItem := dmlEvent.TableItem.(*applierTableItem)
	var tableColumns = tableItem.columns
	span := opentracing.GlobalTracer().StartSpan("desc  buildDMLEventQuery ", opentracing.FollowsFrom(spanContext))
	defer span.Finish()
	doPrepareIfNil := func(stmts []*gosql.Stmt, query string) (*gosql.Stmt, error) {
		var err error
		if stmts[workerIdx] == nil {
			a.logger.Debug("mysql.applier buildDMLEventQuery prepare query %v", query)
			stmts[workerIdx], err = a.dbs[workerIdx].Db.PrepareContext(context.Background(), query)
			if err != nil {
				a.logger.Error("mysql.applier buildDMLEventQuery prepare query %v err %v", query, err)
			}
		}
		return stmts[workerIdx], err
	}

	switch dmlEvent.DML {
	case binlog.DeleteDML:
		{
			query, uniqueKeyArgs, hasUK, err := sql.BuildDMLDeleteQuery(dmlEvent.DatabaseName, dmlEvent.TableName, tableColumns, dmlEvent.WhereColumnValues.GetAbstractValues())
			if err != nil {
				return nil, "", nil, -1, err
			}
			if hasUK {
				stmt, err := doPrepareIfNil(tableItem.psDelete, query)
				if err != nil {
					return nil, "", nil, -1, err
				}
				return stmt, "", uniqueKeyArgs, -1, nil
			} else {
				return nil, query, uniqueKeyArgs, -1, nil
			}
		}
	case binlog.InsertDML:
		{
			// TODO no need to generate query string every time
			query, sharedArgs, err := sql.BuildDMLInsertQuery(dmlEvent.DatabaseName, dmlEvent.TableName, tableColumns, tableColumns, tableColumns, dmlEvent.NewColumnValues.GetAbstractValues())
			if err != nil {
				return nil, "", nil, -1, err
			}
			stmt, err := doPrepareIfNil(tableItem.psInsert, query)
			if err != nil {
				return nil, "", nil, -1, err
			}
			return stmt, "", sharedArgs, 1, err
		}
	case binlog.UpdateDML:
		{
			query, sharedArgs, uniqueKeyArgs, hasUK, err := sql.BuildDMLUpdateQuery(dmlEvent.DatabaseName, dmlEvent.TableName, tableColumns, tableColumns, tableColumns, tableColumns, dmlEvent.NewColumnValues.GetAbstractValues(), dmlEvent.WhereColumnValues.GetAbstractValues())
			if err != nil {
				return nil, "", nil, -1, err
			}
			args = append(args, sharedArgs...)
			args = append(args, uniqueKeyArgs...)

			if hasUK {
				stmt, err := doPrepareIfNil(tableItem.psUpdate, query)
				if err != nil {
					return nil, "", nil, -1, err
				}

				return stmt, "", args, 0, err
			} else {
				return nil, query, args, 0, err
			}
		}
	}
	return nil, "", args, 0, fmt.Errorf("Unknown dml event type: %+v", dmlEvent.DML)
}

// ApplyEventQueries applies multiple DML queries onto the dest table
func (a *Applier) ApplyBinlogEvent(ctx context.Context, workerIdx int, binlogEntry *binlog.BinlogEntry) error {
	dbApplier := a.dbs[workerIdx]

	var totalDelta int64
	var err error
	var spanContext opentracing.SpanContext
	var span opentracing.Span
	if ctx != nil {
		spanContext = opentracing.SpanFromContext(ctx).Context()
		span = opentracing.GlobalTracer().StartSpan(" desc single binlogEvent transform to sql ", opentracing.ChildOf(spanContext))
		span.SetTag("start insert sql ", time.Now().UnixNano()/1e6)
		defer span.Finish()

	} else {
		spanContext = binlogEntry.SpanContext
		span = opentracing.GlobalTracer().StartSpan("desc mts binlogEvent transform to sql ", opentracing.ChildOf(spanContext))
		span.SetTag("start insert sql ", time.Now().UnixNano()/1e6)
		defer span.Finish()
		spanContext = span.Context()
	}
	txSid := binlogEntry.Coordinates.GetSid()

	dbApplier.DbMutex.Lock()
	tx, err := dbApplier.Db.BeginTx(context.Background(), &gosql.TxOptions{})
	if err != nil {
		return err
	}
	defer func() {
		span.SetTag("begin commit sql ", time.Now().UnixNano()/1e6)
		if err := tx.Commit(); err != nil {
			a.onError(TaskStateDead, err)
		} else {
			a.mtsManager.Executed(binlogEntry)
		}
		if a.printTps {
			atomic.AddUint32(&a.txLastNSeconds, 1)
		}
		span.SetTag("after  commit sql ", time.Now().UnixNano()/1e6)

		dbApplier.DbMutex.Unlock()
	}()
	span.SetTag("begin transform binlogEvent to sql time  ", time.Now().UnixNano()/1e6)
	for i, event := range binlogEntry.Events {
		a.logger.Debug("mysql.applier: ApplyBinlogEvent. gno: %v, event: %v",
			binlogEntry.Coordinates.GNO, i)
		switch event.DML {
		case binlog.NotDML:
			var err error
			a.logger.Debug("mysql.applier: ApplyBinlogEvent: not dml: %v", event.Query)

			if event.CurrentSchema != "" {
				query := fmt.Sprintf("USE %s", umconf.EscapeName(event.CurrentSchema))
				a.logger.Debug("mysql.applier: query: %v", query)
				_, err = tx.Exec(query)
				if err != nil {
					if !sql.IgnoreError(err) {
						a.logger.Error("mysql.applier: Exec sql error: %v", err)
						return err
					} else {
						a.logger.Warn("mysql.applier: Ignore error: %v", err)
					}
				}
			}

			if event.TableName != "" {
				var schema string
				if event.DatabaseName != "" {
					schema = event.DatabaseName
				} else {
					schema = event.CurrentSchema
				}
				a.logger.Debug("mysql.applier: reset tableItem %v.%v", schema, event.TableName)
				a.getTableItem(schema, event.TableName).Reset()
			} else { // TableName == ""
				if event.DatabaseName != "" {
					if schemaItem, ok := a.tableItems[event.DatabaseName]; ok {
						for tableName, v := range schemaItem {
							a.logger.Debug("mysql.applier: reset tableItem %v.%v", event.DatabaseName, tableName)
							v.Reset()
						}
					}
					delete(a.tableItems, event.DatabaseName)
				}
			}

			_, err = tx.Exec(event.Query)
			if err != nil {
				if !sql.IgnoreError(err) {
					a.logger.Error("mysql.applier: Exec sql error: %v", err)
					return err
				} else {
					a.logger.Warn("mysql.applier: Ignore error: %v", err)
				}
			}
			a.logger.Debug("mysql.applier: Exec [%s]", event.Query)
		default:
			a.logger.Debug("mysql.applier: ApplyBinlogEvent: a dml event")
			stmt, query, args, rowDelta, err := a.buildDMLEventQuery(event, workerIdx, spanContext)
			if err != nil {
				a.logger.Error("mysql.applier: Build dml query error: %v", err)
				return err
			}

			a.logger.Debug("ApplyBinlogEvent. args: %v", args)

			var r gosql.Result
			if stmt != nil {
				r, err = stmt.Exec(args...)
			} else {
				r, err = a.dbs[workerIdx].Db.ExecContext(context.Background(), query, args...)
			}

			if err != nil {
				a.logger.Error("mysql.applier: gtid: %s:%d, error: %v", txSid, binlogEntry.Coordinates.GNO, err)
				return err
			}
			nr, err := r.RowsAffected()
			if err != nil {
				a.logger.Debug("ApplyBinlogEvent executed gno %v event %v rows_affected_err %v schema", binlogEntry.Coordinates.GNO, i, err)
			} else {
				a.logger.Debug("ApplyBinlogEvent executed gno %v event %v rows_affected %v", binlogEntry.Coordinates.GNO, i, nr)
			}
			totalDelta += rowDelta
		}
	}
	span.SetTag("after  transform  binlogEvent to sql  ", time.Now().UnixNano()/1e6)
	a.logger.Debug("ApplyBinlogEvent. insert gno: %v", binlogEntry.Coordinates.GNO)
	_, err = dbApplier.PsInsertExecutedGtid.Exec(binlogEntry.Coordinates.SID.Bytes(), binlogEntry.Coordinates.GNO)
	if err != nil {
		return err
	}

	// no error
	a.mysqlContext.Stage =  StageWaitingForGtidToBeCommitted
	atomic.AddInt64(&a.mysqlContext.TotalDeltaCopied, 1)
	return nil
}

func (a *Applier) ApplyEventQueries(db *gosql.DB, entry *DumpEntry) error {
	if a.stubFullApplyDelay != 0 {
		a.logger.Debug("mysql.applier: stubFullApplyDelay start sleep")
		time.Sleep(a.stubFullApplyDelay)
		a.logger.Debug("mysql.applier: stubFullApplyDelay end sleep")
	}

	if entry.SystemVariablesStatement != "" {
		for i := range a.dbs {
			a.logger.Debug("mysql.applier: exec sysvar query: %v", entry.SystemVariablesStatement)
			_, err := a.dbs[i].Db.ExecContext(context.Background(), entry.SystemVariablesStatement)
			if err != nil {
				a.logger.Error("mysql.applier: err exec sysvar query. err: %v", err)
				return err
			}
		}
	}
	if entry.SqlMode != "" {
		for i := range a.dbs {
			a.logger.Debug("mysql.applier: exec sqlmode query: %v", entry.SqlMode)
			_, err := a.dbs[i].Db.ExecContext(context.Background(), entry.SqlMode)
			if err != nil {
				a.logger.Error("mysql.applier: err exec sysvar query. err: %v", err)
				return err
			}
		}
	}

	queries := []string{}
	queries = append(queries, entry.SystemVariablesStatement, entry.SqlMode, entry.DbSQL)
	queries = append(queries, entry.TbSQL...)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Commit(); err != nil {
			a.onError(TaskStateDead, err)
		}
		atomic.AddInt64(&a.mysqlContext.TotalRowsReplay, entry.RowsCount)
	}()
	sessionQuery := `SET @@session.foreign_key_checks = 0`
	if _, err := tx.Exec(sessionQuery); err != nil {
		return err
	}
	execQuery := func(query string) error {
		a.logger.Debug("mysql.applier: Exec [%s]", utils.StrLim(query, 256))
		_, err := tx.Exec(query)
		if err != nil {
			if !sql.IgnoreError(err) {
				a.logger.Error("mysql.applier: Exec [%s] error: %v", utils.StrLim(query, 10), err)
				return err
			}
			if !sql.IgnoreExistsError(err) {
				a.logger.Warn("mysql.applier: Ignore error: %v", err)
			}
		}
		return nil
	}

	for _, query := range queries {
		if query == "" {
			continue
		}
		err := execQuery(query)
		if err != nil {
			return err
		}
	}

	var buf bytes.Buffer
	BufSizeLimit := 1 * 1024 * 1024 // 1MB. TODO parameterize it
	BufSizeLimitDelta := 1024
	buf.Grow(BufSizeLimit + BufSizeLimitDelta)
	for i, _ := range entry.ValuesX {
		if buf.Len() == 0 {
			buf.WriteString(fmt.Sprintf(`replace into %s.%s values (`,
				umconf.EscapeName(entry.TableSchema), umconf.EscapeName(entry.TableName)))
		} else {
			buf.WriteString(",(")
		}

		firstCol := true
		for j := range entry.ValuesX[i] {
			if firstCol {
				firstCol = false
			} else {
				buf.WriteByte(',')
			}

			colData := entry.ValuesX[i][j]
			if colData != nil {
				buf.WriteByte('\'')
				buf.WriteString(sql.EscapeValue(string(*colData)))
				buf.WriteByte('\'')
			} else {
				buf.WriteString("NULL")
			}
		}
		buf.WriteByte(')')

		needInsert := (i == len(entry.ValuesX)-1) || (buf.Len() >= BufSizeLimit)
		// last rows or sql too large

		if needInsert {
			err := execQuery(buf.String())
			buf.Reset()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *Applier) Stats() (*TaskStatistics, error) {
	totalRowsReplay := a.mysqlContext.GetTotalRowsReplay()
	rowsEstimate := atomic.LoadInt64(&a.mysqlContext.RowsEstimate)
	totalDeltaCopied := a.mysqlContext.GetTotalDeltaCopied()
	deltaEstimate := atomic.LoadInt64(&a.mysqlContext.DeltaEstimate)

	var progressPct float64
	var backlog, eta string
	if rowsEstimate == 0 && deltaEstimate == 0 {
		progressPct = 0.0
	} else {
		progressPct = 100.0 * float64(totalDeltaCopied+totalRowsReplay) / float64(deltaEstimate+rowsEstimate)
		if a.mysqlContext.Gtid != "" {
			// Done copying rows. The totalRowsCopied value is the de-facto number of rows,
			// and there is no further need to keep updating the value.
			backlog = fmt.Sprintf("%d/%d", len(a.applyDataEntryQueue), cap(a.applyDataEntryQueue))
		} else {
			backlog = fmt.Sprintf("%d/%d", len(a.copyRowsQueue), cap(a.copyRowsQueue))
		}
	}

	var etaSeconds float64 = math.MaxFloat64
	eta = "N/A"
	if progressPct >= 100.0 {
		eta = "0s"
		a.mysqlContext.Stage =  StageSlaveHasReadAllRelayLog
	} else if progressPct >= 1.0 {
		elapsedRowCopySeconds := a.mysqlContext.ElapsedRowCopyTime().Seconds()
		totalExpectedSeconds := elapsedRowCopySeconds * float64(rowsEstimate) / float64(totalRowsReplay)
		if atomic.LoadInt64(&a.rowCopyCompleteFlag) == 1 {
			totalExpectedSeconds = elapsedRowCopySeconds * float64(deltaEstimate) / float64(totalDeltaCopied)
		}
		etaSeconds = totalExpectedSeconds - elapsedRowCopySeconds
		if etaSeconds >= 0 {
			etaDuration := time.Duration(etaSeconds) * time.Second
			eta = base.PrettifyDurationOutput(etaDuration)
		} else {
			eta = "0s"
		}
	}

	taskResUsage :=  TaskStatistics{
		ExecMasterRowCount: totalRowsReplay,
		ExecMasterTxCount:  totalDeltaCopied,
		ReadMasterRowCount: rowsEstimate,
		ReadMasterTxCount:  deltaEstimate,
		ProgressPct:        strconv.FormatFloat(progressPct, 'f', 1, 64),
		ETA:                eta,
		Backlog:            backlog,
		Stage:              a.mysqlContext.Stage,
		CurrentCoordinates: a.currentCoordinates,
		BufferStat:  BufferStat{
			ApplierTxQueueSize:      len(a.applyBinlogTxQueue),
			ApplierGroupTxQueueSize: len(a.applyBinlogGroupTxQueue),
		},
		Timestamp: time.Now().UTC().UnixNano(),
	}
	if a.natsConn != nil {
		taskResUsage.MsgStat = a.natsConn.Statistics
	}

	return &taskResUsage, nil
}

func (a *Applier) ID() string {
	id := config.DriverCtx{
		DriverConfig: &config.MySQLDriverConfig{
			ReplicateDoDb:     a.mysqlContext.ReplicateDoDb,
			ReplicateIgnoreDb: a.mysqlContext.ReplicateIgnoreDb,
			Gtid:              a.mysqlContext.Gtid,
			BinlogPos:         a.mysqlContext.BinlogPos,
			BinlogFile:        a.mysqlContext.BinlogFile,
			NatsAddr:          a.mysqlContext.NatsAddr,
			ParallelWorkers:   a.mysqlContext.ParallelWorkers,
			ConnectionConfig:  a.mysqlContext.ConnectionConfig,
		},
	}

	data, err := json.Marshal(id)
	if err != nil {
		a.logger.Error("mysql.applier: Failed to marshal ID to JSON: %s", err)
	}
	return string(data)
}

func (a *Applier) onError(state int, err error) {
	if a.shutdown {
		return
	}
	switch state {
	case TaskStateComplete:
		a.logger.Info("mysql.applier: Done migrating")
	case TaskStateRestart:
		if a.natsConn != nil {
			if err := a.natsConn.Publish(fmt.Sprintf("%s_restart", a.subject), []byte(a.mysqlContext.Gtid)); err != nil {
				a.logger.Error("mysql.applier: Trigger restart extractor : %v", err)
			}
		}
	default:
		if a.natsConn != nil {
			if err := a.natsConn.Publish(fmt.Sprintf("%s_error", a.subject), []byte(a.mysqlContext.Gtid)); err != nil {
				a.logger.Error("mysql.applier: Trigger extractor shutdown: %v", err)
			}
		}
	}

	a.waitCh <- *NewWaitResult(state, err)
	a.Shutdown()
}

func (a *Applier) WaitCh() chan WaitResult {
	return a.waitCh
}

func (a *Applier) Shutdown() error {
	a.shutdownLock.Lock()
	defer a.shutdownLock.Unlock()
	if a.shutdown {
		return nil
	}

	if a.natsConn != nil {
		a.natsConn.Close()
	}

	a.shutdown = true
	close(a.shutdownCh)

	if err := sql.CloseDB(a.db); err != nil {
		return err
	}
	if err := sql.CloseConns(a.dbs...); err != nil {
		return err
	}

	//close(a.applyBinlogTxQueue)
	//close(a.applyBinlogGroupTxQueue)
	a.logger.Info("mysql.applier: Shutting down")
	return nil
}