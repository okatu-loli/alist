package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlistGo/alist-agent/channel"
	"github.com/AlistGo/alist-agent/proto"
)

// This file unit-tests the agent-side reverse-mode path (design R3) — the
// channelCapClient + ReverseAgent — end to end over a REAL loopback channel,
// but against FAKE server-side cap handlers (no DB / op / fs). It proves the
// transport abstraction on its own: RunCopyTaskWith drives the identical
// sequence (status running -> src-link -> streaming put -> status done) over
// the channel, and the streaming put reassembles byte-for-byte. The full
// authorization integration is covered in server/handles/agent_channel_test.go.

func genReverseKeys(t *testing.T) (agentPub ed25519.PublicKey, agentPriv ed25519.PrivateKey, serverPub ed25519.PublicKey, serverPriv ed25519.PrivateKey) {
	t.Helper()
	var err error
	agentPub, agentPriv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverPub, serverPriv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// liveChannelPair dials a real loopback listener and returns both live ends.
func liveChannelPair(t *testing.T, agentID string) (server, agent *channel.Channel) {
	t.Helper()
	agentPub, agentPriv, serverPub, serverPriv := genReverseKeys(t)

	got := make(chan *channel.Channel, 1)
	l, err := channel.NewListener(channel.ListenerConfig{
		AgentID:         agentID,
		AgentPriv:       agentPriv,
		PinnedServerPub: serverPub,
		OnChannel:       func(ch *channel.Channel) { got <- ch },
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, err = channel.DialAgent(ctx, wsURL, serverPriv, agentPub, agentID)
	if err != nil {
		t.Fatalf("DialAgent: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	select {
	case agent = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("agent side never went live")
	}
	t.Cleanup(func() { _ = agent.Close() })
	return server, agent
}

// fakeCapServer registers canned cap handlers on the server end of a channel
// and records what the agent sent, so a test can assert on the exact sequence
// RunCopyTaskWith produced.
type fakeCapServer struct {
	srcURL string

	mu        sync.Mutex
	statuses  []string
	putBuf    map[string][]byte // streamID -> reassembled bytes
	assembled string
}

func newFakeCapServer(server *channel.Channel, srcURL string) *fakeCapServer {
	f := &fakeCapServer{srcURL: srcURL, putBuf: map[string][]byte{}}

	server.Handle(proto.MethodTaskStatus, func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		var req proto.ChannelStatusReq
		_ = json.Unmarshal(p, &req)
		f.mu.Lock()
		f.statuses = append(f.statuses, req.Status)
		f.mu.Unlock()
		return json.Marshal(proto.ChannelAck{OK: true})
	})
	server.Handle(proto.MethodTaskProgress, func(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(proto.ChannelAck{OK: true})
	})
	server.Handle(proto.MethodTaskSrcLink, func(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(proto.ChannelSrcLinkResp{URL: f.srcURL, Size: -1, Proxied: true})
	})
	server.Handle(proto.MethodTaskPutBegin, func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		var req proto.ChannelPutBeginReq
		_ = json.Unmarshal(p, &req)
		f.mu.Lock()
		f.putBuf[req.StreamID] = nil
		f.mu.Unlock()
		return json.Marshal(proto.ChannelAck{OK: true})
	})
	server.Handle(proto.MethodTaskPutChunk, func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		var req proto.ChannelPutChunkReq
		_ = json.Unmarshal(p, &req)
		f.mu.Lock()
		f.putBuf[req.StreamID] = append(f.putBuf[req.StreamID], req.Data...)
		f.mu.Unlock()
		return json.Marshal(proto.ChannelAck{OK: true})
	})
	server.Handle(proto.MethodTaskPutEnd, func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		var req proto.ChannelPutEndReq
		_ = json.Unmarshal(p, &req)
		f.mu.Lock()
		f.assembled = string(f.putBuf[req.StreamID])
		f.mu.Unlock()
		return json.Marshal(proto.ChannelAck{OK: true})
	})
	return f
}

// TestRunCopyTaskWithOverChannelCapClient drives the transport-agnostic
// executor over the reverse channel and asserts the same sequence + a
// byte-identical streamed upload as the HTTP path.
func TestRunCopyTaskWithOverChannelCapClient(t *testing.T) {
	payload := strings.Repeat("chan-payload-", 300_000) // spans many chunks
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srcSrv.Close()

	server, agent := liveChannelPair(t, "reverse-unit-agent")
	f := newFakeCapServer(server, srcSrv.URL)

	cc := NewChannelCapClient(agent)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := RunCopyTaskWith(ctx, cc, Task{TaskID: "t1", Type: "copy", SrcPath: "/x/src", DstDir: "/y/dst"}, StaticToken("cap"))
	if err != nil {
		t.Fatalf("RunCopyTaskWith over channel: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) < 2 || f.statuses[0] != proto.TaskStatusRunning || f.statuses[len(f.statuses)-1] != proto.TaskStatusDone {
		t.Fatalf("expected first=running last=done, got %v", f.statuses)
	}
	if f.assembled != payload {
		t.Fatalf("streamed upload mismatch: got %d bytes, want %d", len(f.assembled), len(payload))
	}
}

// TestReverseAgentRunsPushedTask proves the server->agent dispatch push wiring:
// pushing task.dispatch makes the ReverseAgent run RunCopyTaskWith over the
// channel to completion.
func TestReverseAgentRunsPushedTask(t *testing.T) {
	payload := "pushed-task-bytes"
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srcSrv.Close()

	server, agent := liveChannelPair(t, "reverse-dispatch-agent")
	f := newFakeCapServer(server, srcSrv.URL)

	done := make(chan error, 1)
	ra := NewReverseAgent(agent)
	ra.OnTaskDone = func(_ string, err error) { done <- err }

	disp, _ := json.Marshal(proto.ChannelDispatch{TaskID: "t9", Type: "copy", SrcPath: "/a", DstDir: "/b", CapToken: "cap"})
	if err := server.Push(proto.MethodTaskDispatch, disp); err != nil {
		t.Fatalf("push dispatch: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reverse agent task failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reverse agent never ran the pushed task")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.assembled != payload {
		t.Fatalf("pushed-task upload mismatch: got %q want %q", f.assembled, payload)
	}
}

// TestReverseAgentLogTailReturnsEntriesAfterSince pins the agent-side
// agent.log.tail handler: the server sends {since} over the channel and gets
// back exactly the ring entries with seq > since, plus the right next cursor.
func TestReverseAgentLogTailReturnsEntriesAfterSince(t *testing.T) {
	server, agent := liveChannelPair(t, "reverse-logtail-agent")

	ring := NewLogRing(100)
	for i := 1; i <= 5; i++ {
		ring.Append(time.Now(), "info", "line")
	}
	ra := NewReverseAgent(agent)
	ra.LogRing = ring

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// since=0 → all 5, next=5.
	req, _ := json.Marshal(proto.ChannelLogTailReq{Since: 0})
	raw, err := server.Send(ctx, proto.MethodAgentLogTail, req)
	if err != nil {
		t.Fatalf("log.tail send: %v", err)
	}
	var resp proto.ChannelLogTailResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode log.tail resp: %v", err)
	}
	if len(resp.Entries) != 5 || resp.Next != 5 {
		t.Fatalf("Since(0) over channel = %d entries next %d, want 5, 5", len(resp.Entries), resp.Next)
	}

	// since=3 → only 4,5.
	req, _ = json.Marshal(proto.ChannelLogTailReq{Since: 3})
	raw, _ = server.Send(ctx, proto.MethodAgentLogTail, req)
	_ = json.Unmarshal(raw, &resp)
	if len(resp.Entries) != 2 || resp.Entries[0].Seq != 4 || resp.Next != 5 {
		t.Fatalf("Since(3) over channel = %+v, want 2 entries starting seq 4 next 5", resp)
	}
}

// TestReverseAgentLogTailCapsCount pins the per-call cap (proto.MaxLogTailEntries):
// a ring with more than the cap returns exactly the cap and a next cursor the
// caller can resume from without skipping.
func TestReverseAgentLogTailCapsCount(t *testing.T) {
	server, agent := liveChannelPair(t, "reverse-logtail-cap-agent")

	ring := NewLogRing(proto.MaxLogTailEntries + 100)
	for i := 0; i < proto.MaxLogTailEntries+100; i++ {
		ring.Append(time.Now(), "info", "x")
	}
	ra := NewReverseAgent(agent)
	ra.LogRing = ring

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := json.Marshal(proto.ChannelLogTailReq{Since: 0})
	raw, err := server.Send(ctx, proto.MethodAgentLogTail, req)
	if err != nil {
		t.Fatalf("log.tail send: %v", err)
	}
	var resp proto.ChannelLogTailResp
	_ = json.Unmarshal(raw, &resp)
	if len(resp.Entries) != proto.MaxLogTailEntries {
		t.Fatalf("capped tail returned %d, want %d", len(resp.Entries), proto.MaxLogTailEntries)
	}
	if resp.Next != int64(proto.MaxLogTailEntries) {
		t.Fatalf("capped tail next = %d, want %d", resp.Next, proto.MaxLogTailEntries)
	}
}

// TestReverseAgentLogTailNoRing pins the clean-degrade path: with no ring
// attached the method still answers with an empty tail echoing the cursor,
// rather than erroring the poll.
func TestReverseAgentLogTailNoRing(t *testing.T) {
	server, agent := liveChannelPair(t, "reverse-logtail-noring-agent")
	_ = NewReverseAgent(agent) // no LogRing set

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := json.Marshal(proto.ChannelLogTailReq{Since: 7})
	raw, err := server.Send(ctx, proto.MethodAgentLogTail, req)
	if err != nil {
		t.Fatalf("log.tail send: %v", err)
	}
	var resp proto.ChannelLogTailResp
	_ = json.Unmarshal(raw, &resp)
	if len(resp.Entries) != 0 || resp.Next != 7 {
		t.Fatalf("no-ring tail = %+v, want 0 entries next 7", resp)
	}
}

// TestReverseAgentOneTaskAtATime pins the one-in-flight guard: a second
// dispatch arriving while the first copy is running is skipped, not run
// concurrently.
func TestReverseAgentOneTaskAtATime(t *testing.T) {
	release := make(chan struct{})
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the first copy open in its download
		_, _ = w.Write([]byte("x"))
	}))
	defer srcSrv.Close()

	server, agent := liveChannelPair(t, "reverse-guard-agent")
	_ = newFakeCapServer(server, srcSrv.URL)

	var skipped int32
	skippedCh := make(chan struct{}, 4)
	ra := NewReverseAgent(agent)
	ra.OnTaskSkipped = func(_, _ string) {
		skipped++
		skippedCh <- struct{}{}
	}

	disp1, _ := json.Marshal(proto.ChannelDispatch{TaskID: "t1", Type: "copy", SrcPath: "/a", DstDir: "/b", CapToken: "cap"})
	disp2, _ := json.Marshal(proto.ChannelDispatch{TaskID: "t2", Type: "copy", SrcPath: "/a", DstDir: "/b", CapToken: "cap"})
	if err := server.Push(proto.MethodTaskDispatch, disp1); err != nil {
		t.Fatal(err)
	}
	// Wait until the first copy is actually in flight (blocked in the held
	// download) before pushing the second.
	deadline := time.After(5 * time.Second)
	for !ra.Busy() {
		select {
		case <-deadline:
			t.Fatal("first dispatched task never went in-flight")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := server.Push(proto.MethodTaskDispatch, disp2); err != nil {
		t.Fatal(err)
	}

	select {
	case <-skippedCh:
		// good: the second dispatch was skipped while one was in flight.
	case <-time.After(5 * time.Second):
		t.Fatal("second concurrent dispatch was not skipped")
	}
	close(release)
	ra.Wait()
	if skipped == 0 {
		t.Fatal("expected the second concurrent dispatch to be skipped")
	}
}

// TestReverseAgentsShareTaskGateAcrossChannels pins the process-level
// TaskGate (channel-stability wave 1, defense in depth alongside the
// listener's single-live-channel enforcement): two DISTINCT ReverseAgents —
// modeling two channels briefly alive for the same underlying agent process,
// e.g. a redial racing the old channel's teardown — sharing one *TaskGate
// must still only ever run the SAME dispatched task once, even though each
// ReverseAgent's own per-instance in-flight bookkeeping has no idea the other
// exists.
func TestReverseAgentsShareTaskGateAcrossChannels(t *testing.T) {
	release := make(chan struct{})
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the first copy open in its download
		_, _ = w.Write([]byte("x"))
	}))
	defer srcSrv.Close()

	server1, agent1 := liveChannelPair(t, "shared-gate-agent-1")
	_ = newFakeCapServer(server1, srcSrv.URL)
	server2, agent2 := liveChannelPair(t, "shared-gate-agent-2")
	_ = newFakeCapServer(server2, srcSrv.URL)

	gate := NewTaskGate()

	var skipped int32
	skippedCh := make(chan struct{}, 4)
	ra1 := NewReverseAgent(agent1, gate)
	ra1.OnTaskSkipped = func(_, _ string) {
		atomic.AddInt32(&skipped, 1)
		skippedCh <- struct{}{}
	}
	ra2 := NewReverseAgent(agent2, gate)
	ra2.OnTaskSkipped = func(_, _ string) {
		atomic.AddInt32(&skipped, 1)
		skippedCh <- struct{}{}
	}

	disp, _ := json.Marshal(proto.ChannelDispatch{TaskID: "shared-t1", Type: "copy", SrcPath: "/a", DstDir: "/b", CapToken: "cap"})
	if err := server1.Push(proto.MethodTaskDispatch, disp); err != nil {
		t.Fatal(err)
	}
	// Wait until the first ReverseAgent's copy is actually in flight (blocked
	// in the held download) before dispatching the SAME task on the second
	// channel.
	deadline := time.After(5 * time.Second)
	for !gate.Busy() {
		select {
		case <-deadline:
			t.Fatal("first dispatched task never went in-flight")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := server2.Push(proto.MethodTaskDispatch, disp); err != nil {
		t.Fatal(err)
	}

	select {
	case <-skippedCh:
		// good: the second channel's dispatch of the same task was skipped —
		// the shared gate, not either ReverseAgent's own bookkeeping, caught
		// it.
	case <-time.After(5 * time.Second):
		t.Fatal("second channel's dispatch of the same task was not skipped by the shared gate")
	}
	close(release)
	ra1.Wait()
	ra2.Wait()
	if atomic.LoadInt32(&skipped) == 0 {
		t.Fatal("expected the second channel's dispatch to be skipped")
	}
}
