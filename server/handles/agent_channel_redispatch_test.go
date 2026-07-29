package handles

import (
	"encoding/json"
	"github.com/AlistGo/alist-agent/proto"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// Channel-stability wave 2/2 (server/handles/agent_channel.go's redispatchInterval):
// the reverse supervisor's dispatchOnce ticks every 3s (reverseDispatchInterval)
// for as long as the channel is live, so a bound handler that unconditionally
// re-pushed task.dispatch on every tick would spam a still-running task with a
// fresh dispatch (and a fresh audit row) every 3 seconds — exactly the noise
// this wave fixes. These tests pin the three properties the fix promises: (1) a
// still-active task is NOT re-pushed inside the throttle window, (2) it IS
// re-pushed once the window elapses (the compensating case for a push the agent
// silently dropped), and (3) a terminal status report prunes the per-task
// bookkeeping so it can never grow unbounded.

// TestChannelHandlerThrottlesRedispatchOfActiveTask proves the core fix: two
// DispatchPending calls close together for the SAME still-active task push
// task.dispatch only once.
func TestChannelHandlerThrottlesRedispatchOfActiveTask(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskPending)

	var pushes atomic.Int64
	srv := serverAndAgent(t, fx.agent.AgentID, func(d proto.ChannelDispatch) {
		if d.TaskID == task.TaskID {
			pushes.Add(1)
		}
	})
	bound := RegisterAgentChannelHandlers(srv, fx.agent.AgentID)

	dispatched1, err := bound.DispatchPending(testCtx(t))
	if err != nil {
		t.Fatalf("first DispatchPending: %v", err)
	}
	if !dispatched1 {
		t.Fatal("first DispatchPending must push the pending task")
	}

	dispatched2, err := bound.DispatchPending(testCtx(t))
	if err != nil {
		t.Fatalf("second DispatchPending: %v", err)
	}
	if dispatched2 {
		t.Fatal("second DispatchPending, immediately after the first, must be throttled (task is still active and within redispatchInterval)")
	}

	time.Sleep(200 * time.Millisecond)
	if got := pushes.Load(); got != 1 {
		t.Fatalf("agent received %d task.dispatch pushes, want exactly 1 (the throttle must suppress the second)", got)
	}
}

// TestChannelHandlerRedispatchesAfterIntervalElapses proves the compensating
// case survives: once redispatchInterval elapses, a still-active task IS
// re-pushed (covers a push that reached the agent but was silently dropped or
// ignored). The interval is injected short so the test doesn't wait 60s.
func TestChannelHandlerRedispatchesAfterIntervalElapses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskPending)

	var pushes atomic.Int64
	srv := serverAndAgent(t, fx.agent.AgentID, func(d proto.ChannelDispatch) {
		if d.TaskID == task.TaskID {
			pushes.Add(1)
		}
	})
	bound := RegisterAgentChannelHandlers(srv, fx.agent.AgentID)
	bound.redispatchInterval = 50 * time.Millisecond

	dispatched1, err := bound.DispatchPending(testCtx(t))
	if err != nil {
		t.Fatalf("first DispatchPending: %v", err)
	}
	if !dispatched1 {
		t.Fatal("first DispatchPending must push the pending task")
	}

	time.Sleep(100 * time.Millisecond) // past the injected 50ms interval

	dispatched2, err := bound.DispatchPending(testCtx(t))
	if err != nil {
		t.Fatalf("second DispatchPending: %v", err)
	}
	if !dispatched2 {
		t.Fatal("second DispatchPending, after redispatchInterval elapsed, must re-push the still-active task")
	}

	time.Sleep(200 * time.Millisecond)
	if got := pushes.Load(); got != 2 {
		t.Fatalf("agent received %d task.dispatch pushes, want exactly 2 (one per successful DispatchPending)", got)
	}
}

// TestChannelHandlerClearsRedispatchBookkeepingOnTerminalStatus proves the
// dispatched map does not grow unboundedly: a terminal status report (done or
// failed) for a taskID prunes that entry. This is exercised directly against
// the unexported status handler (white-box, same package) since neither it nor
// the bookkeeping it prunes depend on a live channel.
func TestChannelHandlerClearsRedispatchBookkeepingOnTerminalStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	h := &agentChannelHandler{
		agentID:    fx.agent.AgentID,
		dispatched: map[string]time.Time{task.TaskID: time.Now()},
	}

	payload, err := json.Marshal(proto.ChannelStatusReq{
		TaskID:   task.TaskID,
		CapToken: capTok,
		Status:   model.AgentTaskDone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, chErr := h.status(testCtx(t), "", payload); chErr != nil {
		t.Fatalf("status(done): %v", chErr)
	}

	h.mu.Lock()
	_, stillTracked := h.dispatched[task.TaskID]
	remaining := len(h.dispatched)
	h.mu.Unlock()
	if stillTracked {
		t.Fatal("a terminal (done) status report must prune the task's redispatch bookkeeping")
	}
	if remaining != 0 {
		t.Fatalf("dispatched map has %d leftover entries after the only tracked task went terminal, want 0", remaining)
	}
}

// TestChannelHandlerClearsRedispatchBookkeepingOnFailedStatus mirrors the done
// case for the other terminal status.
func TestChannelHandlerClearsRedispatchBookkeepingOnFailedStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	h := &agentChannelHandler{
		agentID:    fx.agent.AgentID,
		dispatched: map[string]time.Time{task.TaskID: time.Now()},
	}

	payload, err := json.Marshal(proto.ChannelStatusReq{
		TaskID:   task.TaskID,
		CapToken: capTok,
		Status:   model.AgentTaskFailed,
		Detail:   "boom",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, chErr := h.status(testCtx(t), "", payload); chErr != nil {
		t.Fatalf("status(failed): %v", chErr)
	}

	h.mu.Lock()
	_, stillTracked := h.dispatched[task.TaskID]
	h.mu.Unlock()
	if stillTracked {
		t.Fatal("a terminal (failed) status report must prune the task's redispatch bookkeeping")
	}
}
