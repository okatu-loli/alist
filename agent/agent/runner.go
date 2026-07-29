package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// This file holds the concurrent heartbeat loop (design W12-4 (A)):
// `alist agent run` used to send one heartbeat, and if that heartbeat
// carried a task, run the ENTIRE copy INLINE before sending its next
// heartbeat — a long transfer therefore starved the heartbeat loop for as
// long as the copy took. That coupling was actively dangerous once paired
// with the server-side reliability sweep (design W12-4 (B),
// db.RequeueStaleAgentTasks): a server that reassigns a task away from any
// agent whose LastSeen looks stale would, under the old blocking loop,
// eventually reassign work out from under a live, healthy agent simply
// because it was busy — and since a task's AgentID is fixed at creation and
// db.ClaimPendingForAgent only ever hands a task to the SAME agent identity
// it already belongs to, a "requeue" the original agent didn't know about
// would just get silently re-claimed by that same agent's NEXT heartbeat
// while its FIRST copy might still be writing the same destination file —
// two goroutines/processes, one identity, one dst, concurrently. Runner
// fixes the coupling: the heartbeat ticks on ITS OWN schedule
// (Run's interval), a dispatched task runs in a background goroutine,
// and Tick refuses to start a second copy while one is already in flight
// (the in-flight guard below) — so LastSeen stays fresh for as long as the
// agent process is merely busy, and the server's staleness threshold
// (comfortably wider than the heartbeat interval — see
// db.staleAgentTaskAfter's doc) is only ever crossed by an agent that has
// actually stopped heartbeating, i.e. actually gone.
//
// Runner is deliberately injectable (Beat/RunTask are plain funcs, not
// hardwired to the real HTTP client) so this whole loop is unit-testable
// with fakes — no real HTTP, no process, no clock sleep longer than a test
// wants — see runner_test.go. cmd/agent.go's `agent run` is thin glue: it
// builds a Runner wired to the real Heartbeat/RunCopyTask and drives it off
// Run() until an OS signal.

// TaskToken is a concurrency-safe holder for one task's current capability
// token (design W12-4 (A)). Runner.Tick calls Set whenever a heartbeat
// response renews the SAME active task's token; Get (a method value, so it
// satisfies TokenFunc directly) is what gets threaded into RunCopyTask, so
// each of RunCopyTask's server calls reads whatever is current at the
// moment it fires — see TokenFunc's doc in task.go.
type TaskToken struct {
	v atomic.Value
}

// NewTaskToken returns a TaskToken already holding initial.
func NewTaskToken(initial string) *TaskToken {
	t := &TaskToken{}
	t.Set(initial)
	return t
}

// Set replaces the held token.
func (t *TaskToken) Set(tok string) { t.v.Store(tok) }

// Get returns the currently held token. As a method value (t.Get) it has
// exactly the signature of TokenFunc.
func (t *TaskToken) Get() string {
	s, _ := t.v.Load().(string)
	return s
}

// Runner drives the concurrent heartbeat/dispatch loop described above.
// The zero value is not usable — construct with NewRunner.
type Runner struct {
	// Beat performs one heartbeat and returns the server's response (or an
	// error, which Tick logs via OnHeartbeatError and otherwise ignores —
	// a single missed heartbeat is not fatal, the next tick tries again).
	Beat func(ctx context.Context) (*HeartbeatResponse, error)
	// RunTask executes one dispatched task to completion (or failure).
	// Defaults to RunCopyTask if left nil (NewRunner sets this).
	RunTask func(ctx context.Context, task Task, tokenFn TokenFunc) error
	// TaskTimeout bounds a single dispatched task's execution, independent
	// of any single heartbeat's own (much shorter) timeout. Zero means no
	// extra bound is applied beyond whatever ctx Run/Tick's caller supplies
	// (Run derives the background task's context from context.Background(),
	// never from the tick's own short-lived ctx — a copy must not be
	// cancelled merely because the heartbeat call that dispatched it
	// returned).
	TaskTimeout time.Duration

	// The following are optional observability hooks — every one of them
	// may be left nil, in which case Tick simply does not call it. They
	// exist so cmd/agent.go can hang its logging off Runner without Runner
	// itself depending on a logger, and so tests can assert on behavior
	// without scraping log output.
	OnHeartbeatOK    func()
	OnHeartbeatError func(err error)
	OnDisabled       func()
	OnTaskStart      func(taskID string)
	OnTaskRenew      func(taskID string)
	OnTaskSkipped    func(newTaskID, activeTaskID string)
	OnTaskDone       func(taskID string, err error)

	mu       sync.Mutex
	inFlight bool
	activeID string
	token    *TaskToken
	wg       sync.WaitGroup
}

// NewRunner constructs a Runner wired to beat/runTask. runTask is typically
// a thin closure over RunCopyTask binding the server's base URL, e.g.
// `func(ctx context.Context, task Task, tokenFn TokenFunc) error { return
// RunCopyTask(ctx, host, task, tokenFn) }` — see cmd/agent.go.
func NewRunner(beat func(ctx context.Context) (*HeartbeatResponse, error), runTask func(ctx context.Context, task Task, tokenFn TokenFunc) error) *Runner {
	return &Runner{Beat: beat, RunTask: runTask}
}

// Busy reports whether a task is currently in flight — mainly for tests;
// cmd/agent.go has no need to inspect this itself.
func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight
}

// Wait blocks until any in-flight task goroutine has finished. Used by
// tests (and available for a graceful-shutdown path) to observe a task's
// outcome deterministically rather than polling Busy().
func (r *Runner) Wait() { r.wg.Wait() }

// Tick performs exactly one heartbeat and reacts to whatever it returns:
//
//   - a heartbeat error: OnHeartbeatError, otherwise no-op (the caller's
//     next Tick, on Run's normal interval, tries again).
//   - Enabled=false: OnDisabled, otherwise no-op.
//   - no task in the response: OnHeartbeatOK, nothing else to do.
//   - a task, and NOTHING currently in flight: start it in a background
//     goroutine (OnTaskStart), tracked so a concurrent Tick's dispatch
//     decision sees inFlight=true immediately (the mutex-guarded state
//     change happens before the goroutine is even spawned).
//   - a task with the SAME TaskID as the one already in flight: this is a
//     renewed capability token for the still-running copy (see
//     server/handles.dispatchAgentTask's doc: every heartbeat re-mints a
//     token for the active task) — OnTaskRenew, and the in-flight task's
//     TaskToken is updated so its NEXT server call picks it up.
//   - a task with a DIFFERENT TaskID while one is already in flight: the
//     "one task at a time" rule — OnTaskSkipped, no new goroutine. This
//     should not normally happen (a single agent's active task is what the
//     server keeps re-dispatching), but Tick must never silently start a
//     second concurrent copy if it somehow does.
func (r *Runner) Tick(ctx context.Context) {
	resp, err := r.Beat(ctx)
	if err != nil {
		if r.OnHeartbeatError != nil {
			r.OnHeartbeatError(err)
		}
		return
	}
	if !resp.Enabled {
		if r.OnDisabled != nil {
			r.OnDisabled()
		}
		return
	}
	if r.OnHeartbeatOK != nil {
		r.OnHeartbeatOK()
	}
	if resp.Task == nil {
		return
	}

	t := resp.Task
	r.mu.Lock()
	switch {
	case !r.inFlight:
		r.inFlight = true
		r.activeID = t.TaskID
		r.token = NewTaskToken(t.CapToken)
		r.mu.Unlock()
		if r.OnTaskStart != nil {
			r.OnTaskStart(t.TaskID)
		}
		r.startTask(Task{TaskID: t.TaskID, Type: t.Type, SrcPath: t.SrcPath, DstDir: t.DstDir})
	case r.activeID == t.TaskID:
		r.token.Set(t.CapToken)
		r.mu.Unlock()
		if r.OnTaskRenew != nil {
			r.OnTaskRenew(t.TaskID)
		}
	default:
		activeID := r.activeID
		r.mu.Unlock()
		if r.OnTaskSkipped != nil {
			r.OnTaskSkipped(t.TaskID, activeID)
		}
	}
}

// startTask launches task in a background goroutine using the CURRENT
// token holder (already installed under the lock by Tick before this is
// called) as its TokenFunc, and clears in-flight state on completion.
func (r *Runner) startTask(task Task) {
	r.mu.Lock()
	token := r.token
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			r.inFlight = false
			r.activeID = ""
			r.token = nil
			r.mu.Unlock()
		}()

		taskCtx := context.Background()
		var cancel context.CancelFunc
		if r.TaskTimeout > 0 {
			taskCtx, cancel = context.WithTimeout(taskCtx, r.TaskTimeout)
			defer cancel()
		}
		err := r.RunTask(taskCtx, task, token.Get)
		if r.OnTaskDone != nil {
			r.OnTaskDone(task.TaskID, err)
		}
	}()
}

// Run drives Tick immediately, then again every interval, until ctx is
// done. It does not itself wait for a final in-flight task to finish on
// return — callers that need that (e.g. graceful shutdown) should call
// Wait() after Run returns.
func (r *Runner) Run(ctx context.Context, interval time.Duration) {
	r.Tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.Tick(ctx)
		case <-ctx.Done():
			return
		}
	}
}
