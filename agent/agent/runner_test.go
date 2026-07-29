package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Runner tests (design W12-4 (A)): every property here is exercised with
// fake Beat/RunTask funcs — no real HTTP, no process — so "the heartbeat
// loop is never blocked by a slow copy" and "a renewal reaches the copy's
// next call" are pinned deterministically and fast.

// TestRunnerHeartbeatKeepsTickingWhileTaskRuns pins the core fix: a
// dispatched task that blocks for a long time must NOT stop the heartbeat
// loop from continuing to tick on its own interval. This is THE mechanism
// that keeps a busy agent's LastSeen fresh (see
// db.RequeueStaleAgentTasks' doc on the safety property this whole
// feature rests on).
func TestRunnerHeartbeatKeepsTickingWhileTaskRuns(t *testing.T) {
	var beatCount int32
	started := make(chan struct{})
	unblock := make(chan struct{})
	doneCh := make(chan error, 1)

	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) {
			n := atomic.AddInt32(&beatCount, 1)
			if n == 1 {
				return &HeartbeatResponse{OK: true, Enabled: true, Task: &HeartbeatTask{
					TaskID: "slow-1", Type: "copy", SrcPath: "/a", DstDir: "/b", CapToken: "tok-1",
				}}, nil
			}
			return &HeartbeatResponse{OK: true, Enabled: true}, nil
		},
		func(ctx context.Context, task Task, tokenFn TokenFunc) error {
			close(started)
			<-unblock
			return nil
		},
	)
	r.OnTaskDone = func(taskID string, err error) { doneCh <- err }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, 5*time.Millisecond)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("task never started")
	}

	// While the task is blocked, the heartbeat loop must keep ticking on
	// its own schedule -- give it long enough for many more ticks than a
	// blocked loop could ever produce.
	time.Sleep(80 * time.Millisecond)
	if n := atomic.LoadInt32(&beatCount); n < 5 {
		t.Fatalf("heartbeat loop appears blocked by the in-flight task: only %d beats in 80ms at a 5ms interval", n)
	}
	if !r.Busy() {
		t.Fatal("runner must report busy while the task is still blocked")
	}

	close(unblock)
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("task returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task never completed")
	}
	r.Wait()
	if r.Busy() {
		t.Fatal("runner must report not-busy after the task completes")
	}
}

// TestRunnerRenewsTokenForSameActiveTask pins token-provider threading at
// the Runner level (design W12-4 (A)): a SECOND heartbeat dispatching the
// SAME active TaskID with a different CapToken must update the token the
// in-flight task's TokenFunc reads — not start a second copy, not require
// the task to be told explicitly.
func TestRunnerRenewsTokenForSameActiveTask(t *testing.T) {
	unblock := make(chan struct{})
	tokenAtEnd := make(chan string, 1)
	var beatN int32

	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) {
			n := atomic.AddInt32(&beatN, 1)
			switch n {
			case 1:
				return &HeartbeatResponse{OK: true, Enabled: true, Task: &HeartbeatTask{TaskID: "renew-1", CapToken: "tok-v1"}}, nil
			case 2:
				return &HeartbeatResponse{OK: true, Enabled: true, Task: &HeartbeatTask{TaskID: "renew-1", CapToken: "tok-v2"}}, nil
			default:
				return &HeartbeatResponse{OK: true, Enabled: true}, nil
			}
		},
		func(ctx context.Context, task Task, tokenFn TokenFunc) error {
			<-unblock
			tokenAtEnd <- tokenFn()
			return nil
		},
	)

	var renewCalls int32
	r.OnTaskRenew = func(taskID string) {
		if taskID != "renew-1" {
			t.Errorf("OnTaskRenew got unexpected taskID %q", taskID)
		}
		atomic.AddInt32(&renewCalls, 1)
	}

	r.Tick(context.Background()) // dispatches renew-1 with tok-v1; RunTask blocks on unblock
	if !r.Busy() {
		t.Fatal("expected busy after the first dispatch")
	}
	r.Tick(context.Background()) // same TaskID, renewed CapToken tok-v2 -- must NOT start a second task

	if atomic.LoadInt32(&renewCalls) != 1 {
		t.Fatalf("expected exactly 1 OnTaskRenew call, got %d", renewCalls)
	}

	close(unblock)
	select {
	case got := <-tokenAtEnd:
		if got != "tok-v2" {
			t.Fatalf("the in-flight task's tokenFn must reflect the renewal, got %q, want tok-v2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task never observed unblock")
	}
	r.Wait()
}

// TestRunnerSkipsDifferentTaskWhileBusy pins the "one task at a time" rule:
// a heartbeat dispatching a DIFFERENT TaskID while one is already in flight
// must be skipped (OnTaskSkipped), never started as a second concurrent
// copy — see this file's top-of-file doc on why a second concurrent copy
// against a task the agent doesn't even know about would be dangerous.
func TestRunnerSkipsDifferentTaskWhileBusy(t *testing.T) {
	unblock := make(chan struct{})
	taskAStarted := make(chan struct{})
	var beatN int32
	var starts int32

	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) {
			n := atomic.AddInt32(&beatN, 1)
			switch n {
			case 1:
				return &HeartbeatResponse{OK: true, Enabled: true, Task: &HeartbeatTask{TaskID: "task-a", CapToken: "tok-a"}}, nil
			case 2:
				return &HeartbeatResponse{OK: true, Enabled: true, Task: &HeartbeatTask{TaskID: "task-b", CapToken: "tok-b"}}, nil
			default:
				return &HeartbeatResponse{OK: true, Enabled: true}, nil
			}
		},
		func(ctx context.Context, task Task, tokenFn TokenFunc) error {
			atomic.AddInt32(&starts, 1)
			close(taskAStarted)
			<-unblock
			return nil
		},
	)

	var skipped struct{ newID, activeID string }
	skipCh := make(chan struct{}, 1)
	r.OnTaskSkipped = func(newTaskID, activeTaskID string) {
		skipped.newID, skipped.activeID = newTaskID, activeTaskID
		skipCh <- struct{}{}
	}

	r.Tick(context.Background()) // dispatches task-a
	select {
	case <-taskAStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("task-a's RunTask never started")
	}

	r.Tick(context.Background()) // task-b arrives while task-a is still in flight -> must be skipped

	select {
	case <-skipCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnTaskSkipped never fired")
	}
	if skipped.newID != "task-b" || skipped.activeID != "task-a" {
		t.Fatalf("OnTaskSkipped(%q, %q), want (task-b, task-a)", skipped.newID, skipped.activeID)
	}
	if n := atomic.LoadInt32(&starts); n != 1 {
		t.Fatalf("RunTask must only ever have been started ONCE (never a second concurrent copy), got %d starts", n)
	}

	close(unblock)
	r.Wait()
	if n := atomic.LoadInt32(&starts); n != 1 {
		t.Fatalf("still only 1 start expected after the in-flight task finished, got %d", n)
	}
}

// TestRunnerAllowsNewTaskAfterPreviousCompletes pins that the in-flight
// guard is per-copy, not permanent: once a task finishes, the NEXT distinct
// task a heartbeat dispatches is started normally.
func TestRunnerAllowsNewTaskAfterPreviousCompletes(t *testing.T) {
	var beatN int32
	var started []string

	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) {
			n := atomic.AddInt32(&beatN, 1)
			switch n {
			case 1:
				return &HeartbeatResponse{OK: true, Enabled: true, Task: &HeartbeatTask{TaskID: "first", CapToken: "t1"}}, nil
			case 2:
				return &HeartbeatResponse{OK: true, Enabled: true, Task: &HeartbeatTask{TaskID: "second", CapToken: "t2"}}, nil
			default:
				return &HeartbeatResponse{OK: true, Enabled: true}, nil
			}
		},
		func(ctx context.Context, task Task, tokenFn TokenFunc) error {
			return nil // completes immediately
		},
	)
	doneCh := make(chan string, 4)
	r.OnTaskStart = func(taskID string) { started = append(started, taskID) }
	r.OnTaskDone = func(taskID string, err error) { doneCh <- taskID }

	r.Tick(context.Background())
	select {
	case id := <-doneCh:
		if id != "first" {
			t.Fatalf("expected 'first' to finish, got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first task never completed")
	}
	r.Wait()

	r.Tick(context.Background())
	select {
	case id := <-doneCh:
		if id != "second" {
			t.Fatalf("expected 'second' to finish, got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second task never completed")
	}
	r.Wait()

	if len(started) != 2 || started[0] != "first" || started[1] != "second" {
		t.Fatalf("expected both tasks to start in order, got %v", started)
	}
}

// TestRunnerHeartbeatErrorDoesNotPanicAndCallsHook pins that a failed
// heartbeat is handled gracefully (no task dispatch attempted) and reported
// through OnHeartbeatError.
func TestRunnerHeartbeatErrorDoesNotPanicAndCallsHook(t *testing.T) {
	wantErr := errors.New("network hiccup")
	var gotErr error
	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) { return nil, wantErr },
		func(ctx context.Context, task Task, tokenFn TokenFunc) error { return nil },
	)
	r.OnHeartbeatError = func(err error) { gotErr = err }

	r.Tick(context.Background())
	if gotErr != wantErr {
		t.Fatalf("OnHeartbeatError got %v, want %v", gotErr, wantErr)
	}
	if r.Busy() {
		t.Fatal("a failed heartbeat must never leave the runner busy")
	}
}

// TestRunnerDisabledCallsHookAndSkipsTask pins that Enabled=false is
// respected even if the (malformed/stale) response also carried a task --
// a disabled agent must never start executing anything.
func TestRunnerDisabledCallsHookAndSkipsTask(t *testing.T) {
	var disabledCalls int32
	var starts int32
	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) {
			return &HeartbeatResponse{OK: true, Enabled: false, Task: &HeartbeatTask{TaskID: "x", CapToken: "y"}}, nil
		},
		func(ctx context.Context, task Task, tokenFn TokenFunc) error {
			atomic.AddInt32(&starts, 1)
			return nil
		},
	)
	r.OnDisabled = func() { atomic.AddInt32(&disabledCalls, 1) }

	r.Tick(context.Background())
	if n := atomic.LoadInt32(&disabledCalls); n != 1 {
		t.Fatalf("expected OnDisabled once, got %d", n)
	}
	if n := atomic.LoadInt32(&starts); n != 0 {
		t.Fatal("a disabled heartbeat must never start a task")
	}
}

// TestRunnerNoTaskIsNoop pins the common case: an OK, enabled heartbeat
// with no task calls OnHeartbeatOK and starts nothing.
func TestRunnerNoTaskIsNoop(t *testing.T) {
	var okCalls int32
	var starts int32
	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) {
			return &HeartbeatResponse{OK: true, Enabled: true}, nil
		},
		func(ctx context.Context, task Task, tokenFn TokenFunc) error {
			atomic.AddInt32(&starts, 1)
			return nil
		},
	)
	r.OnHeartbeatOK = func() { atomic.AddInt32(&okCalls, 1) }

	r.Tick(context.Background())
	if n := atomic.LoadInt32(&okCalls); n != 1 {
		t.Fatalf("expected OnHeartbeatOK once, got %d", n)
	}
	if n := atomic.LoadInt32(&starts); n != 0 {
		t.Fatal("no task in the response must never start one")
	}
	if r.Busy() {
		t.Fatal("must not be busy with no task dispatched")
	}
}

// TestRunnerRunStopsOnContextCancel pins that Run's loop actually exits
// once its ctx is cancelled, rather than ticking forever.
func TestRunnerRunStopsOnContextCancel(t *testing.T) {
	var beats int32
	r := NewRunner(
		func(ctx context.Context) (*HeartbeatResponse, error) {
			atomic.AddInt32(&beats, 1)
			return &HeartbeatResponse{OK: true, Enabled: true}, nil
		},
		func(ctx context.Context, task Task, tokenFn TokenFunc) error { return nil },
	)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		r.Run(ctx, 5*time.Millisecond)
		close(runDone)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
	if atomic.LoadInt32(&beats) == 0 {
		t.Fatal("expected at least one heartbeat before cancellation")
	}
}

// TestTaskTokenConcurrentGetSet exercises TaskToken directly under -race:
// concurrent Get/Set must never race (it is read from a background task
// goroutine while Set is called from the heartbeat loop's goroutine).
func TestTaskTokenConcurrentGetSet(t *testing.T) {
	tok := NewTaskToken("v0")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = tok.Get()
		}
	}()
	for i := 0; i < 200; i++ {
		tok.Set("v1")
	}
	<-done
}
