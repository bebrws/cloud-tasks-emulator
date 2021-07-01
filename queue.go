package main

import (
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	pduration "github.com/golang/protobuf/ptypes/duration"

	tasks "google.golang.org/genproto/googleapis/cloud/tasks/v2"
)

// Queue holds all internals for a task queue
type Queue struct {
	name string

	state *tasks.Queue

	fire chan *Task

	work chan *Task

	ts map[string]*Task

	tsMux sync.Mutex

	tokenBucket chan bool

	maxDispatchesPerSecond float64

	cancelTokenGenerator chan bool

	cancelDispatcher chan bool

	cancelWorkers chan bool

	cancelled bool

	paused bool

	onTaskDone func(task *Task)
}

// NewQueue creates a new task queue
func NewQueue(name string, state *tasks.Queue, onTaskDone func(task *Task)) (*Queue, *tasks.Queue) {
	setInitialQueueState(state)

	queue := &Queue{
		name:                   name,
		state:                  state,
		fire:                   make(chan *Task),
		work:                   make(chan *Task),
		ts:                     make(map[string]*Task),
		onTaskDone:             onTaskDone,
		tokenBucket:            make(chan bool, state.GetRateLimits().GetMaxBurstSize()),
		maxDispatchesPerSecond: state.GetRateLimits().GetMaxDispatchesPerSecond(),
		cancelTokenGenerator:   make(chan bool, 1),
		cancelDispatcher:       make(chan bool, 1),
		cancelWorkers:          make(chan bool, 1),
	}
	// Fill the token bucket
	for i := 0; i < int(state.GetRateLimits().GetMaxBurstSize()); i++ {
		queue.tokenBucket <- true
	}

	return queue, state
}

func (queue *Queue) setTask(taskName string, task *Task) {
	queue.tsMux.Lock()
	defer queue.tsMux.Unlock()
	queue.ts[taskName] = task
}

func (queue *Queue) removeTask(taskName string) {
	queue.setTask(taskName, nil)
}

func setInitialQueueState(queueState *tasks.Queue) {
	if queueState.GetRateLimits() == nil {
		queueState.RateLimits = &tasks.RateLimits{}
	}
	if queueState.GetRateLimits().GetMaxDispatchesPerSecond() == 0 {
		queueState.RateLimits.MaxDispatchesPerSecond = 500.0
	}

	maxDispatchesPerSecond, err := strconv.ParseFloat(os.Getenv("MAX_DISPATCHES_PER_SECOND"), 64)
	if err == nil && maxDispatchesPerSecond != 0 {
		queueState.RateLimits.MaxDispatchesPerSecond = maxDispatchesPerSecond
	}

	if queueState.GetRateLimits().GetMaxBurstSize() == 0 {
		queueState.RateLimits.MaxBurstSize = 100
	}

	maxBurstSize, err := strconv.ParseInt(os.Getenv("MAX_BURST_SIZE"), 10, 32)
	if err == nil && maxBurstSize != 0 {
		queueState.RateLimits.MaxBurstSize = int32(maxBurstSize)
	}

	if queueState.GetRateLimits().GetMaxConcurrentDispatches() == 0 {
		queueState.RateLimits.MaxConcurrentDispatches = 1000
	}

	maxConcurrentDispatches, err := strconv.ParseInt(os.Getenv("MAX_CONCURRENT_DISPATCHES"), 10, 32)
	if err == nil && maxConcurrentDispatches != 0 {
		queueState.RateLimits.MaxConcurrentDispatches = int32(maxConcurrentDispatches)
	}

	if queueState.GetRetryConfig() == nil {
		queueState.RetryConfig = &tasks.RetryConfig{}
	}
	if queueState.GetRetryConfig().GetMaxAttempts() == 0 {
		queueState.RetryConfig.MaxAttempts = 100
	}
	maxAttempts, err := strconv.ParseInt(os.Getenv("MAX_ATTEMPTS"), 10, 32)
	if err == nil && maxAttempts != 0 {
		queueState.RetryConfig.MaxAttempts = int32(maxAttempts)
	}

	if queueState.GetRetryConfig().GetMaxDoublings() == 0 {
		queueState.RetryConfig.MaxDoublings = 16
	}
	maxDoublings, err := strconv.ParseInt(os.Getenv("MAX_DOUBLINGS"), 10, 32)
	if err == nil && maxDoublings != 0 {
		queueState.RetryConfig.MaxDoublings = int32(maxDoublings)
	}

	if queueState.GetRetryConfig().GetMinBackoff() == nil {
		queueState.RetryConfig.MinBackoff = &pduration.Duration{
			Nanos: 100000000,
		}
	}
	minBackoff, err := strconv.ParseInt(os.Getenv("MIN_BACKOFF"), 10, 32)
	if err == nil && minBackoff != 0 {
		queueState.RetryConfig.MinBackoff = &pduration.Duration{
			Nanos: int32(minBackoff),
		}
	}

	if queueState.GetRetryConfig().GetMaxBackoff() == nil {
		queueState.RetryConfig.MaxBackoff = &pduration.Duration{
			Seconds: 3600,
		}
	}
	maxBackoff, err := strconv.ParseInt(os.Getenv("MAX_BACKOFF"), 10, 32)
	if err == nil && maxBackoff != 0 {
		queueState.RetryConfig.MaxBackoff = &pduration.Duration{
			Nanos: int32(maxBackoff),
		}
	}

	queueState.State = tasks.Queue_RUNNING
}

func (queue *Queue) runWorkers() {
	for i := 0; i < int(queue.state.GetRateLimits().GetMaxConcurrentDispatches()); i++ {
		go queue.runWorker()
	}
}

func (queue *Queue) runWorker() {
	for {
		select {
		case task := <-queue.work:
			task.Attempt()
		case <-queue.cancelWorkers:
			// Forward for next worker
			queue.cancelWorkers <- true
			return
		}
	}
}

func (queue *Queue) runTokenGenerator() {
	period := time.Second / time.Duration(queue.maxDispatchesPerSecond)
	// Use Timer with Reset() in place of time.Ticker as the latter was causing high CPU usage in Docker
	t := time.NewTimer(period)

	for {
		select {
		case <-t.C:
			select {
			case queue.tokenBucket <- true:
				// Added token
				t.Reset(period)
			case <-queue.cancelTokenGenerator:
				return
			}
		case <-queue.cancelTokenGenerator:
			if !t.Stop() {
				<-t.C
			}
			return
		}
	}
}

func (queue *Queue) runDispatcher() {
	for {
		select {
		// Consume a token
		case <-queue.tokenBucket:
			select {
			// Wait for task
			case task := <-queue.fire:
				// Pass on to workers
				queue.work <- task
			case <-queue.cancelDispatcher:
				return
			}
		case <-queue.cancelDispatcher:
			return
		}
	}
}

// Run starts the queue (workers, token generator and dispatcher)
func (queue *Queue) Run() {
	go queue.runWorkers()
	go queue.runTokenGenerator()
	go queue.runDispatcher()
}

// NewTask creates a new task on the queue
func (queue *Queue) NewTask(newTaskState *tasks.Task) (*Task, *tasks.Task) {
	task := NewTask(queue, newTaskState, func(task *Task) {
		queue.removeTask(task.state.GetName())
		queue.onTaskDone(task)
	})

	taskState := proto.Clone(task.state).(*tasks.Task)

	queue.setTask(taskState.GetName(), task)

	task.Schedule()

	return task, taskState
}

// Delete stops, purges and removes the queue
func (queue *Queue) Delete() {
	if !queue.cancelled {
		queue.cancelled = true
		log.Println("Stopping queue")
		queue.cancelTokenGenerator <- true
		queue.cancelDispatcher <- true
		queue.cancelWorkers <- true

		queue.Purge()
	}
}

// Purge purges all tasks from the queue
func (queue *Queue) Purge() {
	go func() {

		queue.tsMux.Lock()
		defer queue.tsMux.Unlock()

		for _, task := range queue.ts {
			// Avoid task firing
			if task != nil {
				task.Delete()
			}
		}
	}()
}

// Pause pauses the queue
func (queue *Queue) Pause() {
	if !queue.paused {
		queue.paused = true
		queue.state.State = tasks.Queue_PAUSED

		queue.cancelDispatcher <- true
		queue.cancelWorkers <- true
	}
}

// Resume resumes a paused queue
func (queue *Queue) Resume() {
	if queue.paused {
		queue.paused = false
		queue.state.State = tasks.Queue_RUNNING

		go queue.runDispatcher()
		go queue.runWorkers()
	}
}
