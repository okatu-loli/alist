package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/AlistGo/alist-agent/channel"
	"github.com/AlistGo/alist-agent/proto"
)

// ReverseAgent is the agent-side driver for reverse mode (design R3): once the
// server has dialed in and the mutual-auth handshake produced a live
// *channel.Channel, the agent registers a task.dispatch PUSH handler on it. When
// the server pushes a claimed task (+ a freshly minted capability token), the
// agent runs the copy through RunCopyTaskWith over a channelCapClient — the
// exact same executor `agent run` uses over HTTP, just a different transport.
//
// Like the outbound Runner, it enforces one-task-at-a-time: a second dispatch
// arriving while a copy is in flight is skipped (the server keeps re-dispatching
// the active task on reconnect, but the agent must never start two concurrent
// copies of it). Every capability call the copy makes is independently
// re-authorized server-side over the channel (server/handles/agent_task_core.go)
// — reverse mode is not a standing grant.
type ReverseAgent struct {
	ch *channel.Channel

	// TaskTimeout bounds one dispatched copy; zero means no extra bound beyond
	// channel teardown. The copy's context is derived from context.Background()
	// (not any single frame's short-lived ctx) but is cancelled if the channel
	// dies, so a dropped channel unwinds an in-flight copy.
	TaskTimeout time.Duration

	// Optional observability hooks (all may be nil) — the agent CLI hangs
	// logging off them; tests observe outcomes without scraping logs.
	OnTaskStart   func(taskID string)
	OnTaskSkipped func(taskID, activeID string)
	OnTaskDone    func(taskID string, err error)

	// LogRing, if non-nil, is exposed to the server over the agent.log.tail
	// channel method (design SP-G1): the server pulls this agent's recent log
	// lines for the admin observability surface. nil => an empty tail (the
	// method still answers, just with no entries), so a build with no ring
	// attached degrades cleanly rather than erroring the poll.
	LogRing *LogRing

	// gate is the one-task-at-a-time guard. It defaults to a ReverseAgent-
	// private TaskGate (the historical per-channel behavior), but
	// NewReverseAgent accepts a shared *TaskGate so every ReverseAgent this
	// process constructs across reconnects/duplicate channels enforces the
	// SAME process-wide guard (channel-stability wave 1, defense in depth
	// alongside the listener's single-live-channel enforcement).
	gate *TaskGate
	wg   sync.WaitGroup
}

// NewReverseAgent builds a ReverseAgent bound to a live channel and registers
// its task.dispatch handler. The channel must already be started. An optional
// shared *TaskGate may be passed so every ReverseAgent built against the same
// underlying agent process enforces one task in flight process-wide rather
// than merely per-channel; omitting it gives this ReverseAgent its own
// private gate (equivalent to the pre-wave-1 per-instance guard).
func NewReverseAgent(ch *channel.Channel, gate ...*TaskGate) *ReverseAgent {
	g := NewTaskGate()
	if len(gate) > 0 && gate[0] != nil {
		g = gate[0]
	}
	ra := &ReverseAgent{ch: ch, gate: g}
	ch.Handle(proto.MethodTaskDispatch, ra.handleDispatch)
	ch.Handle(proto.MethodAgentLogTail, ra.handleLogTail)
	return ra
}

// handleLogTail answers a server-initiated agent.log.tail REQUEST: return the
// ring's buffered entries with seq > since (capped at proto.MaxLogTailEntries) plus
// the next cursor. It never blocks and never mutates state — a pure read of the
// in-memory ring. With no ring attached it returns an empty tail echoing the
// requested cursor, so the poll degrades cleanly instead of failing.
func (ra *ReverseAgent) handleLogTail(_ context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelLogTailReq
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
	}
	if ra.LogRing == nil {
		return json.Marshal(proto.ChannelLogTailResp{Entries: []proto.LogEntry{}, Next: req.Since})
	}
	entries, next := ra.LogRing.Since(req.Since, proto.MaxLogTailEntries)
	if entries == nil {
		entries = []proto.LogEntry{}
	}
	return json.Marshal(proto.ChannelLogTailResp{Entries: entries, Next: next})
}

// Busy reports whether a dispatched copy is currently in flight.
func (ra *ReverseAgent) Busy() bool {
	return ra.gate.Busy()
}

// Wait blocks until any in-flight dispatched copy has finished (tests / a
// graceful-shutdown path).
func (ra *ReverseAgent) Wait() { ra.wg.Wait() }

// handleDispatch is the task.dispatch PUSH handler. It decodes the pushed task
// + capability token and, if nothing is already in flight, launches the copy in
// a background goroutine so the channel's push-serving goroutine returns
// immediately. Returns no response (a push has none).
func (ra *ReverseAgent) handleDispatch(_ context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var d proto.ChannelDispatch
	if err := json.Unmarshal(payload, &d); err != nil {
		return nil, nil
	}
	if d.TaskID == "" || d.CapToken == "" {
		return nil, nil
	}

	ok, active := ra.gate.TryStart(d.TaskID)
	if !ok {
		if ra.OnTaskSkipped != nil {
			ra.OnTaskSkipped(d.TaskID, active)
		}
		return nil, nil
	}

	if ra.OnTaskStart != nil {
		ra.OnTaskStart(d.TaskID)
	}

	task := Task{TaskID: d.TaskID, Type: d.Type, SrcPath: d.SrcPath, DstDir: d.DstDir}
	capToken := d.CapToken

	ra.wg.Add(1)
	go func() {
		defer ra.wg.Done()
		defer ra.gate.Finish()

		// The copy outlives this push's own ctx (which the channel cancels the
		// moment the push handler returned), so derive from Background — but
		// cancel it if the channel tears down, so a dropped reverse channel
		// unwinds an in-flight copy rather than leaking a goroutine blocked on
		// a dead Send.
		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if ra.TaskTimeout > 0 {
			var tcancel context.CancelFunc
			runCtx, tcancel = context.WithTimeout(runCtx, ra.TaskTimeout)
			defer tcancel()
		}
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ra.ch.Done():
				cancel()
			case <-stop:
			}
		}()

		err := RunCopyTaskWith(runCtx, NewChannelCapClient(ra.ch), task, StaticToken(capToken))
		if ra.OnTaskDone != nil {
			ra.OnTaskDone(task.TaskID, err)
		}
	}()

	return nil, nil
}
