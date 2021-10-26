// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	enumsspb "go.temporal.io/server/api/enums/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/primitives/timestamp"
	"go.temporal.io/server/common/tasks"
	"go.temporal.io/server/service/history/configs"
	"go.temporal.io/server/service/history/shard"
	"go.temporal.io/server/service/history/workflow"

	"go.temporal.io/server/common"
	"go.temporal.io/server/common/backoff"
	"go.temporal.io/server/common/clock"
	"go.temporal.io/server/common/collection"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/quotas"
)

var (
	emptyTime = time.Time{}

	loadNamespaceEntryForTimerTaskRetryDelay = 100 * time.Millisecond
	loadTimerTaskThrottleRetryDelay          = 5 * time.Second
)

type (
	timerQueueProcessorBase struct {
		scope                int
		shard                shard.Context
		historyService       *historyEngineImpl
		cache                workflow.Cache
		executionManager     persistence.ExecutionManager
		status               int32
		shutdownWG           sync.WaitGroup
		shutdownCh           chan struct{}
		config               *configs.Config
		logger               log.Logger
		metricsClient        metrics.Client
		metricsScope         metrics.Scope
		timerFiredCount      uint64
		timerProcessor       timerProcessor
		timerQueueAckMgr     timerQueueAckMgr
		timerGate            TimerGate
		timeSource           clock.TimeSource
		rateLimiter          quotas.RateLimiter
		retryPolicy          backoff.RetryPolicy
		lastPollTime         time.Time
		taskProcessor        *taskProcessor // TODO: deprecate task processor, in favor of queueTaskProcessor
		queueTaskProcessor   queueTaskProcessor
		redispatchQueue      collection.Queue
		queueTaskInitializer queueTaskInitializer

		// timer notification
		newTimerCh  chan struct{}
		newTimeLock sync.Mutex
		newTime     time.Time
	}
)

func newTimerQueueProcessorBase(
	scope int,
	shard shard.Context,
	historyService *historyEngineImpl,
	timerProcessor timerProcessor,
	queueTaskProcessor queueTaskProcessor,
	timerQueueAckMgr timerQueueAckMgr,
	redispatchQueue collection.Queue,
	queueTaskInitializer queueTaskInitializer,
	timerGate TimerGate,
	maxPollRPS dynamicconfig.IntPropertyFn,
	logger log.Logger,
	metricsScope metrics.Scope,
) *timerQueueProcessorBase {

	logger = log.With(logger, tag.ComponentTimerQueue)
	config := shard.GetConfig()

	var taskProcessor *taskProcessor
	if !config.TimerProcessorEnablePriorityTaskProcessor() {
		options := taskProcessorOptions{
			workerCount: config.TimerTaskWorkerCount(),
			queueSize:   config.TimerTaskWorkerCount() * config.TimerTaskBatchSize(),
		}
		taskProcessor = newTaskProcessor(options, shard, historyService.historyCache, logger)
	}

	base := &timerQueueProcessorBase{
		scope:                scope,
		shard:                shard,
		historyService:       historyService,
		timerProcessor:       timerProcessor,
		cache:                historyService.historyCache,
		executionManager:     shard.GetExecutionManager(),
		status:               common.DaemonStatusInitialized,
		shutdownCh:           make(chan struct{}),
		config:               config,
		logger:               logger,
		metricsClient:        historyService.metricsClient,
		metricsScope:         metricsScope,
		timerQueueAckMgr:     timerQueueAckMgr,
		timerGate:            timerGate,
		timeSource:           shard.GetTimeSource(),
		newTimerCh:           make(chan struct{}, 1),
		lastPollTime:         time.Time{},
		taskProcessor:        taskProcessor,
		queueTaskProcessor:   queueTaskProcessor,
		redispatchQueue:      redispatchQueue,
		queueTaskInitializer: queueTaskInitializer,
		rateLimiter: quotas.NewDefaultOutgoingDynamicRateLimiter(
			func() float64 { return float64(maxPollRPS()) },
		),
		retryPolicy: common.CreatePersistanceRetryPolicy(),
	}

	return base
}

func (t *timerQueueProcessorBase) Start() {
	if !atomic.CompareAndSwapInt32(&t.status, common.DaemonStatusInitialized, common.DaemonStatusStarted) {
		return
	}

	if t.taskProcessor != nil {
		t.taskProcessor.start()
	}
	t.shutdownWG.Add(1)
	// notify a initial scan
	t.notifyNewTimer(time.Time{})
	go t.processorPump()

	t.logger.Info("Timer queue processor started.")
}

func (t *timerQueueProcessorBase) Stop() {
	if !atomic.CompareAndSwapInt32(&t.status, common.DaemonStatusStarted, common.DaemonStatusStopped) {
		return
	}

	t.timerGate.Close()
	close(t.shutdownCh)
	t.retryTasks()

	if success := common.AwaitWaitGroup(&t.shutdownWG, time.Minute); !success {
		t.logger.Warn("Timer queue processor timedout on shutdown.")
	}

	if t.taskProcessor != nil {
		t.taskProcessor.stop()
	}
	t.logger.Info("Timer queue processor stopped.")
}

func (t *timerQueueProcessorBase) processorPump() {
	defer t.shutdownWG.Done()

RetryProcessor:
	for {
		select {
		case <-t.shutdownCh:
			break RetryProcessor
		default:
			err := t.internalProcessor()
			if err != nil {
				t.logger.Error("processor pump failed with error", tag.Error(err))
			}
		}
	}

	t.logger.Info("Timer queue processor pump shutting down.")
	t.logger.Info("Timer processor exiting.")
}

// NotifyNewTimers - Notify the processor about the new timer events arrival.
// This should be called each time new timer events arrives, otherwise timers maybe fired unexpected.
func (t *timerQueueProcessorBase) notifyNewTimers(
	timerTasks []tasks.Task,
) {

	if len(timerTasks) == 0 {
		return
	}

	newTime := timerTasks[0].GetVisibilityTime()
	for _, task := range timerTasks {
		ts := task.GetVisibilityTime()
		if ts.Before(newTime) {
			newTime = ts
		}
	}

	t.notifyNewTimer(newTime)
}

func (t *timerQueueProcessorBase) notifyNewTimer(
	newTime time.Time,
) {

	t.newTimeLock.Lock()
	defer t.newTimeLock.Unlock()
	if t.newTime.IsZero() || newTime.Before(t.newTime) {
		t.newTime = newTime
		select {
		case t.newTimerCh <- struct{}{}:
			// Notified about new time.
		default:
			// Channel "full" -> drop and move on, this will happen only if service is in high load.
		}
	}
}

func (t *timerQueueProcessorBase) internalProcessor() error {
	pollTimer := time.NewTimer(backoff.JitDuration(
		t.config.TimerProcessorMaxPollInterval(),
		t.config.TimerProcessorMaxPollIntervalJitterCoefficient(),
	))
	defer pollTimer.Stop()

	updateAckTimer := time.NewTimer(backoff.JitDuration(
		t.config.TimerProcessorUpdateAckInterval(),
		t.config.TimerProcessorUpdateAckIntervalJitterCoefficient(),
	))
	defer updateAckTimer.Stop()

	redispatchTimer := time.NewTimer(backoff.JitDuration(
		t.config.TimerProcessorRedispatchInterval(),
		t.config.TimerProcessorRedispatchIntervalJitterCoefficient(),
	))
	defer redispatchTimer.Stop()

	for {
		// Wait until one of four things occurs:
		// 1. we get notified of a new message
		// 2. the timer gate fires (message scheduled to be delivered)
		// 3. shutdown was triggered.
		// 4. updating ack level
		//
		select {
		case <-t.shutdownCh:
			t.logger.Debug("Timer queue processor pump shutting down.")
			return nil
		case <-t.timerQueueAckMgr.getFinishedChan():
			// timer queue ack manager indicate that all task scanned
			// are finished and no more tasks
			// use a separate goroutine since the caller hold the shutdownWG
			go t.Stop()
			return nil
		case <-t.timerGate.FireChan():
			if !t.isPriorityTaskProcessorEnabled() || t.redispatchQueue.Len() <= t.config.TimerProcessorMaxRedispatchQueueSize() {
				lookAheadTimer, err := t.readAndFanoutTimerTasks()
				if err != nil {
					return err
				}
				if lookAheadTimer != nil {
					t.timerGate.Update(timestamp.TimeValue(lookAheadTimer.VisibilityTime))
				}
				continue
			}

			// has too many pending tasks in re-dispatch queue, block loading tasks from persistence
			t.redispatchTasks()
			// re-enqueue the event to see if we need keep re-dispatching or load new tasks from persistence
			t.notifyNewTimer(time.Time{})
		case <-pollTimer.C:
			pollTimer.Reset(backoff.JitDuration(
				t.config.TimerProcessorMaxPollInterval(),
				t.config.TimerProcessorMaxPollIntervalJitterCoefficient(),
			))
			if t.lastPollTime.Add(t.config.TimerProcessorMaxPollInterval()).Before(t.timeSource.Now()) {
				lookAheadTimer, err := t.readAndFanoutTimerTasks()
				if err != nil {
					return err
				}
				if lookAheadTimer != nil {
					t.timerGate.Update(timestamp.TimeValue(lookAheadTimer.VisibilityTime))
				}
			}
		case <-updateAckTimer.C:
			updateAckTimer.Reset(backoff.JitDuration(
				t.config.TimerProcessorUpdateAckInterval(),
				t.config.TimerProcessorUpdateAckIntervalJitterCoefficient(),
			))
			if err := t.timerQueueAckMgr.updateAckLevel(); err == shard.ErrShardClosed {
				// shard is closed, shutdown timerQProcessor and bail out
				go t.Stop()
				return err
			}
		case <-t.newTimerCh:
			t.newTimeLock.Lock()
			newTime := t.newTime
			t.newTime = emptyTime
			t.newTimeLock.Unlock()
			// New Timer has arrived.
			t.metricsClient.IncCounter(t.scope, metrics.NewTimerNotifyCounter)
			t.timerGate.Update(newTime)
		case <-redispatchTimer.C:
			redispatchTimer.Reset(backoff.JitDuration(
				t.config.TimerProcessorRedispatchInterval(),
				t.config.TimerProcessorRedispatchIntervalJitterCoefficient(),
			))
			t.redispatchTasks()
		}
	}
}

func (t *timerQueueProcessorBase) readAndFanoutTimerTasks() (*persistencespb.TimerTaskInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimerTaskThrottleRetryDelay)
	if err := t.rateLimiter.Wait(ctx); err != nil {
		cancel()
		t.notifyNewTimer(time.Time{}) // re-enqueue the event
		return nil, nil
	}
	cancel()

	t.lastPollTime = t.timeSource.Now()
	timerTasks, lookAheadTask, moreTasks, err := t.timerQueueAckMgr.readTimerTasks()
	if err != nil {
		t.notifyNewTimer(time.Time{}) // re-enqueue the event
		return nil, err
	}

	for _, task := range t.convert(timerTasks) {
		if submitted := t.submitTask(task); !submitted {
			return nil, nil
		}
		select {
		case <-t.shutdownCh:
			return nil, nil
		default:
		}
	}

	if !moreTasks {
		if lookAheadTask == nil {
			return nil, nil
		}
		return t.convert([]tasks.Task{lookAheadTask})[0], nil
	}

	t.notifyNewTimer(time.Time{}) // re-enqueue the event
	return nil, nil
}

func (t *timerQueueProcessorBase) submitTask(
	taskInfo queueTaskInfo,
) bool {
	if !t.isPriorityTaskProcessorEnabled() {
		return t.taskProcessor.addTask(
			newTaskInfo(
				t.timerProcessor,
				taskInfo,
				initializeLoggerForTask(t.shard.GetShardID(), taskInfo, t.logger),
			),
		)
	}

	timeQueueTask := t.queueTaskInitializer(taskInfo)
	submitted, err := t.queueTaskProcessor.TrySubmit(timeQueueTask)
	if err != nil {
		return false
	}
	if !submitted {
		t.redispatchQueue.Add(timeQueueTask)
	}

	return true
}

func (t *timerQueueProcessorBase) redispatchTasks() {
	if !t.isPriorityTaskProcessorEnabled() {
		return
	}

	redispatchQueueTasks(
		t.redispatchQueue,
		t.queueTaskProcessor,
		t.logger,
		t.metricsScope,
		t.shutdownCh,
	)
}

func (t *timerQueueProcessorBase) retryTasks() {
	if t.taskProcessor != nil {
		t.taskProcessor.retryTasks()
	}
}

func (t *timerQueueProcessorBase) complete(
	timerTask *persistencespb.TimerTaskInfo,
) {
	t.timerQueueAckMgr.completeTimerTask(*timerTask.GetVisibilityTime(), timerTask.GetTaskId())
	atomic.AddUint64(&t.timerFiredCount, 1)
}

func (t *timerQueueProcessorBase) isPriorityTaskProcessorEnabled() bool {
	return t.taskProcessor == nil
}

func (t *timerQueueProcessorBase) getTimerFiredCount() uint64 {
	return atomic.LoadUint64(&t.timerFiredCount)
}

func getTimerTaskMetricScope(
	taskType enumsspb.TaskType,
	isActive bool,
) int {
	switch taskType {
	case enumsspb.TASK_TYPE_WORKFLOW_TASK_TIMEOUT:
		if isActive {
			return metrics.TimerActiveTaskWorkflowTaskTimeoutScope
		}
		return metrics.TimerStandbyTaskWorkflowTaskTimeoutScope
	case enumsspb.TASK_TYPE_ACTIVITY_TIMEOUT:
		if isActive {
			return metrics.TimerActiveTaskActivityTimeoutScope
		}
		return metrics.TimerStandbyTaskActivityTimeoutScope
	case enumsspb.TASK_TYPE_USER_TIMER:
		if isActive {
			return metrics.TimerActiveTaskUserTimerScope
		}
		return metrics.TimerStandbyTaskUserTimerScope
	case enumsspb.TASK_TYPE_WORKFLOW_RUN_TIMEOUT:
		if isActive {
			return metrics.TimerActiveTaskWorkflowTimeoutScope
		}
		return metrics.TimerStandbyTaskWorkflowTimeoutScope
	case enumsspb.TASK_TYPE_DELETE_HISTORY_EVENT:
		if isActive {
			return metrics.TimerActiveTaskDeleteHistoryEventScope
		}
		return metrics.TimerStandbyTaskDeleteHistoryEventScope
	case enumsspb.TASK_TYPE_ACTIVITY_RETRY_TIMER:
		if isActive {
			return metrics.TimerActiveTaskActivityRetryTimerScope
		}
		return metrics.TimerStandbyTaskActivityRetryTimerScope
	case enumsspb.TASK_TYPE_WORKFLOW_BACKOFF_TIMER:
		if isActive {
			return metrics.TimerActiveTaskWorkflowBackoffTimerScope
		}
		return metrics.TimerStandbyTaskWorkflowBackoffTimerScope
	default:
		if isActive {
			return metrics.TimerActiveQueueProcessorScope
		}
		return metrics.TimerStandbyQueueProcessorScope
	}
}

// TODO @wxing1292 deprecate this additional conversion before 1.14
func (t *timerQueueProcessorBase) convert(
	genericTasks []tasks.Task,
) []*persistencespb.TimerTaskInfo {
	queueTasks := make([]*persistencespb.TimerTaskInfo, len(genericTasks))
	serializer := serialization.TaskSerializer{}

	for index, task := range genericTasks {
		var timerTask *persistencespb.TimerTaskInfo
		switch task := task.(type) {
		case *tasks.WorkflowTaskTimeoutTask:
			timerTask = serializer.TimerWorkflowTaskToProto(task)
		case *tasks.WorkflowBackoffTimerTask:
			timerTask = serializer.TimerWorkflowDelayTaskToProto(task)
		case *tasks.ActivityTimeoutTask:
			timerTask = serializer.TimerActivityTaskToProto(task)
		case *tasks.ActivityRetryTimerTask:
			timerTask = serializer.TimerActivityRetryTaskToProto(task)
		case *tasks.UserTimerTask:
			timerTask = serializer.TimerUserTaskToProto(task)
		case *tasks.WorkflowTimeoutTask:
			timerTask = serializer.TimerWorkflowRunToProto(task)
		case *tasks.DeleteHistoryEventTask:
			timerTask = serializer.TimerWorkflowCleanupTaskToProto(task)
		default:
			panic(fmt.Sprintf("Unknown timer task type: %v", task))
		}
		queueTasks[index] = timerTask
	}
	return queueTasks
}
