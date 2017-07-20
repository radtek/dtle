package client

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/armon/go-metrics"

	"encoding/json"
	"udup/internal/client/driver"
	"udup/internal/config"
	log "udup/internal/logger"
	"udup/internal/models"
)

const (
	// killBackoffBaseline is the baseline time for exponential backoff while
	// killing a task.
	killBackoffBaseline = 5 * time.Second

	// killBackoffLimit is the limit of the exponential backoff for killing
	// the task.
	killBackoffLimit = 2 * time.Minute

	// killFailureLimit is how many times we will attempt to kill a task before
	// giving up and potentially leaking resources.
	killFailureLimit = 5
)

// Worker is used to wrap a task within an allocation and provide the execution context.
type Worker struct {
	config         *config.ClientConfig
	updater        TaskStateUpdater
	logger         *log.Logger
	alloc          *models.Allocation
	restartTracker *RestartTracker

	// running marks whether the task is running
	running     bool
	runningLock sync.Mutex

	taskStats     *models.TaskStatistics
	taskStatsLock sync.RWMutex

	task *models.Task

	handle     driver.DriverHandle
	handleLock sync.Mutex

	// payloadRendered tracks whether the payload has been rendered to disk
	payloadRendered bool

	// startCh is used to trigger the start of the task
	startCh chan struct{}

	// unblockCh is used to unblock the starting of the task
	unblockCh   chan struct{}
	unblocked   bool
	unblockLock sync.Mutex

	// restartCh is used to restart a task
	restartCh chan *models.TaskEvent

	destroy      bool
	destroyCh    chan struct{}
	destroyLock  sync.Mutex
	destroyEvent *models.TaskEvent
	workUpdates  chan *models.TaskUpdate

	// waitCh closing marks the run loop as having exited
	waitCh chan struct{}

	// persistLock must be acquired when accessing fields stored by
	// SaveState. SaveState is called asynchronously to TaskRunner.Run by
	// AllocRunner, so all store fields must be synchronized using this
	// lock.
	persistLock sync.Mutex
}

// taskRunnerState is used to snapshot the store of the task runner
type workerState struct {
	Version         string
	Task            *models.Task
	HandleID        string
	PayloadRendered bool
}

// TaskStateUpdater is used to signal that tasks store has changed.
type TaskStateUpdater func(taskName, state string, event *models.TaskEvent)

// NewWorker is used to create a new task context
func NewWorker(logger *log.Logger, config *config.ClientConfig,
	updater TaskStateUpdater, alloc *models.Allocation,
	task *models.Task, workUpdates chan *models.TaskUpdate) *Worker {

	// Build the restart tracker.
	t := alloc.Job.LookupTask(alloc.Task)
	if t == nil {
		logger.Errorf("client: alloc '%s' for missing task '%s'", alloc.ID, alloc.Task)
		return nil
	}

	restartTracker := newRestartTracker()

	tc := &Worker{
		config:         config,
		updater:        updater,
		logger:         logger,
		restartTracker: restartTracker,
		alloc:          alloc,
		task:           task,
		destroyCh:      make(chan struct{}),
		waitCh:         make(chan struct{}),
		startCh:        make(chan struct{}, 1),
		unblockCh:      make(chan struct{}),
		restartCh:      make(chan *models.TaskEvent),
		workUpdates:    workUpdates,
	}

	return tc
}

// MarkReceived marks the task as received.
func (r *Worker) MarkReceived() {
	r.updater(r.task.Type, models.TaskStatePending, models.NewTaskEvent(models.TaskReceived))
}

// WaitCh returns a channel to wait for termination
func (r *Worker) WaitCh() <-chan struct{} {
	return r.waitCh
}

// stateFilePath returns the path to our store file
func (r *Worker) stateFilePath() string {
	// Get the MD5 of the task name
	hashVal := md5.Sum([]byte(r.task.Type))
	hashHex := hex.EncodeToString(hashVal[:])
	dirName := fmt.Sprintf("task-%s", hashHex)

	// Generate the path
	path := filepath.Join(r.config.StateDir, "alloc", r.alloc.ID,
		dirName, "store.json")
	return path
}

// SaveState is used to snapshot our store
func (r *Worker) SaveState() error {
	r.persistLock.Lock()
	defer r.persistLock.Unlock()

	r.handleLock.Lock()
	if r.handle != nil {
		id := &config.DriverCtx{}
		handleID := r.handle.ID()
		if err := json.Unmarshal([]byte(handleID), id); err != nil {
			r.logger.Errorf("client: failed to parse handle '%s': %v",
				handleID, err)
		}
		if id.DriverConfig.Gtid != "" {
			if r.task.Type == models.TaskTypeDest {
				r.workUpdates <- &models.TaskUpdate{
					JobID: r.alloc.JobID,
					Gtid:  id.DriverConfig.Gtid,
				}
			}
			r.task.Config["Gtid"] = id.DriverConfig.Gtid
		}
	}
	r.handleLock.Unlock()
	return nil
}

// DestroyState is used to cleanup after ourselves
func (r *Worker) DestroyState() error {
	r.persistLock.Lock()
	defer r.persistLock.Unlock()

	return os.RemoveAll(r.stateFilePath())
}

// setState is used to update the store of the task runner
func (r *Worker) setState(state string, event *models.TaskEvent) {
	// Persist our store to disk.
	if err := r.SaveState(); err != nil {
		r.logger.Errorf("client: failed to save store of Task Runner for task %q: %v", r.task.Type, err)
	}

	// Indicate the task has been updated.
	r.updater(r.task.Type, state, event)
}

// createDriver makes a driver for the task
func (r *Worker) createDriver() (driver.Driver, error) {
	driverCtx := driver.NewDriverContext(r.task.Type, r.alloc.ID, r.config, r.config.Node, r.logger)
	driver, err := driver.NewDriver(r.task.Driver, driverCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to create driver '%s' for alloc %s: %v",
			r.task.Driver, r.alloc.ID, err)
	}
	return driver, err
}

// Run is a long running routine used to manage the task
func (r *Worker) Run() {
	defer close(r.waitCh)
	r.logger.Debugf("client: starting task context for '%s' (alloc '%s')",
		r.task.Type, r.alloc.ID)

	// Create a driver so that we can determine the FSIsolation required
	_, err := r.createDriver()
	if err != nil {
		e := fmt.Errorf("failed to create driver of task %q for alloc %q: %v", r.task.Type, r.alloc.ID, err)
		r.setState(
			models.TaskStateDead,
			models.NewTaskEvent(models.TaskSetupFailure).SetSetupError(e).SetFailsTask())
		return
	}

	// Start the run loop
	r.run()

	return
}

// prestart handles life-cycle tasks that occur before the task has started.
func (r *Worker) prestart(resultCh chan bool) {
	for {
		// Send the start signal
		select {
		case r.startCh <- struct{}{}:
		default:
		}

		resultCh <- true
		return

		// Block for consul-template
		// TODO Hooks should register themselves as blocking and then we can
		// perioidcally enumerate what we are still blocked on
		select {
		case <-r.unblockCh:
			// Send the start signal
			select {
			case r.startCh <- struct{}{}:
			default:
			}

			resultCh <- true
			return
		case <-r.waitCh:
			// The run loop has exited so exit too
			resultCh <- false
			return
		}
	}
}

// run is the main run loop that handles starting the application, destroying
// it, restarts and signals.
func (r *Worker) run() {
	// Predeclare things so we can jump to the RESTART
	var stopCollection chan struct{}
	var handleWaitCh chan *models.WaitResult

	// If we already have a handle, populate the stopCollection and handleWaitCh
	// to fix the invariant that it exists.
	r.handleLock.Lock()
	handleEmpty := r.handle == nil
	r.handleLock.Unlock()

	if !handleEmpty {
		stopCollection = make(chan struct{})
		go r.collectResourceUsageStats(stopCollection)
		handleWaitCh = r.handle.WaitCh()
	}

	for {
		// Do the prestart activities
		prestartResultCh := make(chan bool, 1)
		go r.prestart(prestartResultCh)

	WAIT:
		for {
			select {
			case success := <-prestartResultCh:
				if !success {
					r.setState(models.TaskStateDead, nil)
					return
				}
			case <-r.startCh:
				// Start the task if not yet started or it is being forced. This logic
				// is necessary because in the case of a restore the handle already
				// exists.
				r.handleLock.Lock()
				handleEmpty := r.handle == nil
				r.handleLock.Unlock()

				if handleEmpty {
					startErr := r.startTask()
					r.restartTracker.SetStartError(startErr)
					if startErr != nil {
						r.setState("", models.NewTaskEvent(models.TaskDriverFailure).SetDriverError(startErr))
						goto RESTART
					}

					// Mark the task as started
					r.setState(models.TaskStateRunning, models.NewTaskEvent(models.TaskStarted))
					r.runningLock.Lock()
					r.running = true
					r.runningLock.Unlock()

					if stopCollection == nil {
						stopCollection = make(chan struct{})
						go r.collectResourceUsageStats(stopCollection)
					}

					handleWaitCh = r.handle.WaitCh()
				}

			case waitRes := <-handleWaitCh:
				if waitRes == nil {
					panic("nil wait")
				}

				r.runningLock.Lock()
				r.running = false
				r.runningLock.Unlock()

				// Stop collection of the task's resource usage
				close(stopCollection)

				// Log whether the task was successful or not.
				r.restartTracker.SetWaitResult(waitRes)
				r.setState("", r.waitErrorToEvent(waitRes))
				if !waitRes.Successful() {
					r.logger.Printf("client: task %q for alloc %q failed: %v", r.task.Type, r.alloc.ID, waitRes)
				} else {
					r.logger.Printf("client: task %q for alloc %q completed successfully", r.task.Type, r.alloc.ID)
				}

				break WAIT

			case event := <-r.restartCh:
				r.runningLock.Lock()
				running := r.running
				r.runningLock.Unlock()
				common := fmt.Sprintf("task %v for alloc %q", r.task.Type, r.alloc.ID)
				if !running {
					r.logger.Debugf("client: skipping restart of %v: task isn't running", common)
					continue
				}

				r.logger.Debugf("client: restarting %s: %v", common, event.RestartReason)
				r.setState(models.TaskStateRunning, event)
				r.killTask(nil)

				close(stopCollection)

				if handleWaitCh != nil {
					<-handleWaitCh
				}

				// Since the restart isn't from a failure, restart immediately
				// and don't count against the restart policy
				r.restartTracker.SetRestartTriggered()
				break WAIT

			case <-r.destroyCh:
				r.runningLock.Lock()
				running := r.running
				r.runningLock.Unlock()
				if !running {
					r.setState(models.TaskStateDead, r.destroyEvent)
					return
				}

				// Store the task event that provides context on the task
				// destroy. The Killed event is set from the alloc_runner and
				// doesn't add detail
				var killEvent *models.TaskEvent
				if r.destroyEvent.Type != models.TaskKilled {
					if r.destroyEvent.Type == models.TaskKilling {
						killEvent = r.destroyEvent
					} else {
						r.setState(models.TaskStateRunning, r.destroyEvent)
					}
				}

				r.killTask(killEvent)
				close(stopCollection)
				// Wait for handler to exit before calling cleanup
				<-handleWaitCh

				r.setState(models.TaskStateDead, nil)
				return
			}
		}

	RESTART:
		restart := r.shouldRestart()
		if !restart {
			r.setState(models.TaskStateDead, nil)
			return
		}

		// Clear the handle so a new driver will be created.
		r.handleLock.Lock()
		r.handle = nil
		handleWaitCh = nil
		stopCollection = nil
		r.handleLock.Unlock()
	}
}

// shouldRestart returns if the task should restart. If the return value is
// true, the task's restart policy has already been considered and any wait time
// between restarts has been applied.
func (r *Worker) shouldRestart() bool {
	state, when := r.restartTracker.GetState()
	reason := r.restartTracker.GetReason()
	switch state {
	case models.TaskNotRestarting, models.TaskTerminated:
		r.logger.Printf("client: Not restarting task: %v for alloc: %v ", r.task.Type, r.alloc.ID)
		if state == models.TaskNotRestarting {
			r.setState(models.TaskStateDead,
				models.NewTaskEvent(models.TaskNotRestarting).
					SetRestartReason(reason).SetFailsTask())
		}
		return false
	case models.TaskRestarting:
		r.logger.Printf("client: Restarting task %q for alloc %q in %v", r.task.Type, r.alloc.ID, when)
		r.setState(models.TaskStatePending,
			models.NewTaskEvent(models.TaskRestarting).
				SetRestartDelay(when).
				SetRestartReason(reason))
	default:
		r.logger.Errorf("client: restart tracker returned unknown store: %q", state)
		return false
	}

	// Sleep but watch for destroy events.
	select {
	case <-time.After(when):
	case <-r.destroyCh:
	}

	// Destroyed while we were waiting to restart, so abort.
	r.destroyLock.Lock()
	destroyed := r.destroy
	r.destroyLock.Unlock()
	if destroyed {
		r.logger.Debugf("client: Not restarting task: %v because it has been destroyed", r.task.Type)
		r.setState(models.TaskStateDead, r.destroyEvent)
		return false
	}

	return true
}

// killTask kills the running task. A killing event can optionally be passed and
// this event is used to mark the task as being killed. It provides a means to
// store extra information.
func (r *Worker) killTask(killingEvent *models.TaskEvent) {
	r.runningLock.Lock()
	running := r.running
	r.runningLock.Unlock()
	if !running {
		return
	}

	// Build the event
	var event *models.TaskEvent
	if killingEvent != nil {
		event = killingEvent
		event.Type = models.TaskKilling
	} else {
		event = models.NewTaskEvent(models.TaskKilling)
	}
	event.SetKillTimeout(models.DefaultKillTimeout)

	// Mark that we received the kill event
	r.setState(models.TaskStateRunning, event)

	// Kill the task using an exponential backoff in-case of failures.
	destroySuccess, err := r.handleDestroy()
	if !destroySuccess {
		// We couldn't successfully destroy the resource created.
		r.logger.Errorf("client: failed to kill task %q. Resources may have been leaked: %v", r.task.Type, err)
	}

	r.runningLock.Lock()
	r.running = false
	r.runningLock.Unlock()

	// Store that the task has been destroyed and any associated error.
	r.setState("", models.NewTaskEvent(models.TaskKilled).SetKillError(err))
}

// startTask creates the driver, task dir, and starts the task.
func (r *Worker) startTask() error {
	// Create a driver
	drv, err := r.createDriver()
	if err != nil {
		return fmt.Errorf("failed to create driver of task %q for alloc %q: %v",
			r.task.Type, r.alloc.ID, err)
	}

	// Run prestart
	ctx := driver.NewExecContext(r.alloc.Job.Name, r.alloc.Job.Type)

	// Start the job
	handle, err := drv.Start(ctx, r.task)
	if err != nil {
		wrapped := fmt.Sprintf("failed to start task %q for alloc %q: %v",
			r.task.Type, r.alloc.ID, err)
		r.logger.Warnf("client: %s", wrapped)
		return models.WrapRecoverable(wrapped, err)

	}

	r.handleLock.Lock()
	r.handle = handle
	r.handleLock.Unlock()
	return nil
}

// collectResourceUsageStats starts collecting resource usage stats of a Task.
// Collection ends when the passed channel is closed
func (r *Worker) collectResourceUsageStats(stopCollection <-chan struct{}) {
	// start collecting the stats right away and then start collecting every
	// collection interval
	next := time.NewTimer(0)
	defer next.Stop()
	for {
		select {
		case <-next.C:
			next.Reset(r.config.StatsCollectionInterval)
			if r.handle == nil {
				continue
			}
			ru, err := r.handle.Stats()

			if err != nil {
				// Check if the driver doesn't implement stats
				if err.Error() == driver.DriverStatsNotImplemented.Error() {
					r.logger.Debugf("client: driver for task %q in allocation %q doesn't support stats", r.task.Type, r.alloc.ID)
					return
				}

				// We do not log when the plugin is shutdown as this is simply a
				// race between the stopCollection channel being closed and calling
				// Stats on the handle.
				if !strings.Contains(err.Error(), "connection is shut down") {
					r.logger.Warnf("client: error fetching stats of task %v: %v", r.task.Type, err)
				}
				continue
			}

			r.taskStatsLock.Lock()
			r.taskStats = ru
			r.taskStatsLock.Unlock()
			if ru != nil {
				r.emitStats(ru)
			}
		case <-stopCollection:
			return
		}
	}
}

// LatestResourceUsage returns the last resource utilization datapoint collected
func (r *Worker) LatestTaskStats() *models.TaskStatistics {
	r.taskStatsLock.RLock()
	defer r.taskStatsLock.RUnlock()
	r.runningLock.Lock()
	defer r.runningLock.Unlock()

	// If the task is not running there can be no latest resource
	if !r.running {
		return nil
	}

	return r.taskStats
}

// handleDestroy kills the task handle. In the case that killing fails,
// handleDestroy will retry with an exponential backoff and will give up at a
// given limit. It returns whether the task was destroyed and the error
// associated with the last kill attempt.
func (r *Worker) handleDestroy() (destroyed bool, err error) {
	// Cap the number of times we attempt to kill the task.
	for i := 0; i < killFailureLimit; i++ {
		if err = r.handle.Shutdown(); err != nil {
			// Calculate the new backoff
			backoff := (1 << (2 * uint64(i))) * killBackoffBaseline
			if backoff > killBackoffLimit {
				backoff = killBackoffLimit
			}

			r.logger.Errorf("client: failed to kill task '%s' for alloc %q. Retrying in %v: %v",
				r.task.Type, r.alloc.ID, backoff, err)
			time.Sleep(time.Duration(backoff))
		} else {
			// Kill was successful
			return true, nil
		}
	}
	return
}

// Restart will restart the task
func (r *Worker) Restart(source, reason string) {
	reasonStr := fmt.Sprintf("%s: %s", source, reason)
	event := models.NewTaskEvent(models.TaskRestartSignal).SetRestartReason(reasonStr)

	select {
	case r.restartCh <- event:
	case <-r.waitCh:
	}
}

// Kill will kill a task and store the error, no longer restarting the task. If
// fail is set, the task is marked as having failed.
func (r *Worker) Kill(source, reason string, fail bool) {
	reasonStr := fmt.Sprintf("%s: %s", source, reason)
	event := models.NewTaskEvent(models.TaskKilling).SetKillReason(reasonStr)
	if fail {
		event.SetFailsTask()
	}

	r.logger.Debugf("client: killing task %v for alloc %q: %v", r.task.Type, r.alloc.ID, reasonStr)
	r.Destroy(event)
}

// UnblockStart unblocks the starting of the task. It currently assumes only
// consul-template will unblock
func (r *Worker) UnblockStart(source string) {
	r.unblockLock.Lock()
	defer r.unblockLock.Unlock()
	if r.unblocked {
		return
	}

	r.logger.Debugf("client: unblocking task %v for alloc %q: %v", r.task.Type, r.alloc.ID, source)
	r.unblocked = true
	close(r.unblockCh)
}

// Helper function for converting a WaitResult into a TaskTerminated event.
func (r *Worker) waitErrorToEvent(res *models.WaitResult) *models.TaskEvent {
	return models.NewTaskEvent(models.TaskTerminated).
		SetExitCode(res.ExitCode).
		SetExitMessage(res.Err)
}

// Destroy is used to indicate that the task context should be destroyed. The
// event parameter provides a context for the destroy.
func (r *Worker) Destroy(event *models.TaskEvent) {
	r.destroyLock.Lock()
	defer r.destroyLock.Unlock()

	if r.destroy {
		return
	}
	r.destroy = true
	r.destroyEvent = event
	close(r.destroyCh)
}

// emitStats emits resource usage stats of tasks to remote metrics collector
// sinks
func (r *Worker) emitStats(ru *models.TaskStatistics) {
	if r.config.PublishAllocationMetrics {
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "in_msgs"}, float32(ru.MsgStat.InMsgs))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "out_msgs"}, float32(ru.MsgStat.OutMsgs))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "in_bytes"}, float32(ru.MsgStat.InBytes))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "out_bytes"}, float32(ru.MsgStat.OutBytes))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "src_queue_size"}, float32(ru.BufferStat.ExtractorTxQueueSize))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "dest_group_queue_size"}, float32(ru.BufferStat.ApplierGroupTxQueueSize))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "dest_queue_size"}, float32(ru.BufferStat.ApplierTxQueueSize))
	}
	if ru.TableStats != nil && r.config.PublishAllocationMetrics {
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "insert"}, float32(ru.TableStats.InsertCount))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "update"}, float32(ru.TableStats.UpdateCount))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "table", "delete"}, float32(ru.TableStats.DelCount))
	}

	if ru.DelayCount != nil && r.config.PublishAllocationMetrics {
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "delay", "num"}, float32(ru.DelayCount.Num))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "delay", "time"}, float32(ru.DelayCount.Time))
	}

	if ru.ThroughputStat != nil && r.config.PublishAllocationMetrics {
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "throughput", "num"}, float32(ru.ThroughputStat.Num))
		metrics.SetGauge([]string{r.alloc.Job.Name, r.alloc.Task, "throughput", "time"}, float32(ru.ThroughputStat.Time))
	}
}
