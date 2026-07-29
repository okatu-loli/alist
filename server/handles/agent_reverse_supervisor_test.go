package handles

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"github.com/AlistGo/alist-agent/proto"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentpkg "github.com/AlistGo/alist-agent/agent"
	"github.com/AlistGo/alist-agent/channel"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// spawnLoopbackAgent stands up a real loopback agent WS listener that verifies
// the dialing server against serverPub and signs its reply with agentPriv (the
// production agent-side surface). It returns the ws:// URL the supervisor dials.
func spawnLoopbackAgent(t *testing.T, agentID string, agentPriv ed25519.PrivateKey, serverPub ed25519.PublicKey, onChannel func(*channel.Channel)) string {
	t.Helper()
	l, err := channel.NewListener(channel.ListenerConfig{
		AgentID:         agentID,
		AgentPriv:       agentPriv,
		PinnedServerPub: serverPub,
		OnChannel:       onChannel,
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// staticAgents is a mutable listAgents source for tests: it lets a test swap the
// enumerated set (e.g. disable an agent) and Refresh the supervisor.
type staticAgents struct {
	mu     sync.Mutex
	agents []model.Agent
}

func (s *staticAgents) set(a []model.Agent) {
	s.mu.Lock()
	s.agents = a
	s.mu.Unlock()
}

func (s *staticAgents) list() ([]model.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Agent, len(s.agents))
	copy(out, s.agents)
	return out, nil
}

// serverAndAgent builds one live reverse channel pair and returns BOTH ends,
// with the agent end instrumented to count/record the task.dispatch pushes it
// receives. The server end has NO handlers yet — the caller registers the bound
// handler so it holds the *agentChannelHandler.
func serverAndAgent(t *testing.T, agentID string, onDispatch func(proto.ChannelDispatch)) *channel.Channel {
	t.Helper()
	agentPub, agentPriv := genEd25519(t)
	serverPub, serverPriv := genEd25519(t)

	got := make(chan *channel.Channel, 1)
	url := spawnLoopbackAgent(t, agentID, agentPriv, serverPub, func(ch *channel.Channel) { got <- ch })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, err := channel.DialAgent(ctx, url, serverPriv, agentPub, agentID)
	if err != nil {
		t.Fatalf("DialAgent: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	var agentEnd *channel.Channel
	select {
	case agentEnd = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("agent side never received a live channel")
	}
	t.Cleanup(func() { _ = agentEnd.Close() })
	agentEnd.Handle(proto.MethodTaskDispatch, func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		var d proto.ChannelDispatch
		if err := json.Unmarshal(p, &d); err == nil && onDispatch != nil {
			onDispatch(d)
		}
		return nil, nil
	})
	return server
}

// TestReverseDispatchRightChannelOnly is the security-contract test: two bound
// handlers, for agents A and B; a task belonging to B is dispatched ONLY over
// B's channel (through B's bound handler), and A's bound handler dispatches
// nothing — structurally, because DispatchPending uses the handler's own proven
// id, never a guessed one.
func TestReverseDispatchRightChannelOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fxA := setupAgentTaskFixture(t)
	fxB := setupAgentTaskFixture(t)
	taskB := fxB.createTask(t, model.AgentTaskPending)

	var gotA, gotB atomic.Int64
	srvA := serverAndAgent(t, fxA.agent.AgentID, func(d proto.ChannelDispatch) { gotA.Add(1) })
	srvB := serverAndAgent(t, fxB.agent.AgentID, func(d proto.ChannelDispatch) {
		if d.TaskID == taskB.TaskID {
			gotB.Add(1)
		}
	})

	boundA := RegisterAgentChannelHandlers(srvA, fxA.agent.AgentID)
	boundB := RegisterAgentChannelHandlers(srvB, fxB.agent.AgentID)

	// A's bound handler dispatches nothing (no task for A).
	dispatchedA, err := boundA.DispatchPending(testCtx(t))
	if err != nil {
		t.Fatalf("A DispatchPending: %v", err)
	}
	if dispatchedA {
		t.Fatal("agent A's channel must not dispatch — it has no task; a task for B must never go over A's channel")
	}
	// B's bound handler dispatches B's task.
	dispatchedB, err := boundB.DispatchPending(testCtx(t))
	if err != nil {
		t.Fatalf("B DispatchPending: %v", err)
	}
	if !dispatchedB {
		t.Fatal("agent B's channel must dispatch its own pending task")
	}

	// Let the pushes settle, then assert only B received it.
	time.Sleep(200 * time.Millisecond)
	if gotA.Load() != 0 {
		t.Fatalf("agent A received %d dispatch pushes; a task for B must NEVER reach A's channel", gotA.Load())
	}
	if gotB.Load() != 1 {
		t.Fatalf("agent B received %d dispatch pushes, want exactly 1", gotB.Load())
	}
	// Structural guarantee: the bound handler's id is the handshake-proven id.
	if boundA.AgentID() != fxA.agent.AgentID || boundB.AgentID() != fxB.agent.AgentID {
		t.Fatal("bound handler id must equal the handshake-proven agent id")
	}
}

// newTestReverseAgent creates a reverse-configured agent ROW (with a real pinned
// pubkey) plus a loopback listener for it, and returns the supervisor-facing
// model.Agent + the agent's private key. onChannel is invoked on the agent side
// when the server dials in.
func newTestReverseAgent(t *testing.T, fx agentTaskFixture, serverPub ed25519.PublicKey, onChannel func(*channel.Channel)) model.Agent {
	t.Helper()
	agentPub, agentPriv := genEd25519(t)
	url := spawnLoopbackAgent(t, fx.agent.AgentID, agentPriv, serverPub, onChannel)

	fx.agent.PubKey = base64.StdEncoding.EncodeToString(agentPub)
	fx.agent.Mode = model.AgentModeReverse
	fx.agent.ReverseAddr = url
	fx.agent.Enabled = true
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatalf("update agent to reverse: %v", err)
	}
	return *fx.agent
}

// TestReverseSupervisorDialsAndDispatches drives the full supervisor: it
// enumerates a reverse agent, dials its loopback listener (resolving the pinned
// pubkey + Enabled through the DB), registers handlers, and pushes a pending
// task over the RIGHT channel.
func TestReverseSupervisorDialsAndDispatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	fx.createTask(t, model.AgentTaskPending)

	serverPub, serverPriv := genEd25519(t)
	var gotDispatch atomic.Int64
	rev := newTestReverseAgent(t, fx, serverPub, func(ch *channel.Channel) {
		ch.Handle(proto.MethodTaskDispatch, func(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
			gotDispatch.Add(1)
			return nil, nil
		})
	})

	sup := newReverseSupervisor(serverPriv)
	sup.listAgents = (&staticAgents{agents: []model.Agent{rev}}).list
	sup.dispatchInterval = 100 * time.Millisecond
	sup.reEnumInterval = 200 * time.Millisecond
	t.Cleanup(sup.Stop)
	sup.Start()

	waitFor(t, 5*time.Second, func() bool { return gotDispatch.Load() >= 1 })
}

// TestReverseSupervisorSkipsDisabledAgent pins that a disabled reverse agent is
// never dialed (desiredSet filters Enabled).
func TestReverseSupervisorSkipsDisabledAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	serverPub, serverPriv := genEd25519(t)
	var dialed atomic.Int64
	rev := newTestReverseAgent(t, fx, serverPub, func(ch *channel.Channel) { dialed.Add(1) })
	rev.Enabled = false // disabled in the enumerated set

	sup := newReverseSupervisor(serverPriv)
	sup.listAgents = (&staticAgents{agents: []model.Agent{rev}}).list
	sup.dispatchInterval = 100 * time.Millisecond
	sup.reEnumInterval = 200 * time.Millisecond
	t.Cleanup(sup.Stop)
	sup.Start()

	time.Sleep(600 * time.Millisecond)
	if dialed.Load() != 0 {
		t.Fatalf("a disabled reverse agent must not be dialed; got %d dials", dialed.Load())
	}
	sup.mu.Lock()
	n := len(sup.maintainers)
	sup.mu.Unlock()
	if n != 0 {
		t.Fatalf("disabled agent must have no maintainer; got %d", n)
	}
}

// TestReverseSupervisorDropsDisabledAgent pins that disabling a live reverse
// agent (and refreshing) drops its maintainer and closes its channel.
func TestReverseSupervisorDropsDisabledAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	serverPub, serverPriv := genEd25519(t)
	chans := make(chan *channel.Channel, 4)
	rev := newTestReverseAgent(t, fx, serverPub, func(ch *channel.Channel) { chans <- ch })

	src := &staticAgents{agents: []model.Agent{rev}}
	sup := newReverseSupervisor(serverPriv)
	sup.listAgents = src.list
	sup.dispatchInterval = 100 * time.Millisecond
	sup.reEnumInterval = time.Hour // only explicit Refresh drives reconcile
	t.Cleanup(sup.Stop)
	sup.Start()

	var live *channel.Channel
	select {
	case live = <-chans:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor never dialed the enabled reverse agent")
	}

	// Disable and refresh: the maintainer must be dropped and the channel closed.
	disabled := rev
	disabled.Enabled = false
	src.set([]model.Agent{disabled})
	sup.Refresh()

	waitFor(t, 5*time.Second, func() bool {
		select {
		case <-live.Done():
			return true
		default:
			return false
		}
	})
	sup.mu.Lock()
	n := len(sup.maintainers)
	sup.mu.Unlock()
	if n != 0 {
		t.Fatalf("disabled agent's maintainer must be dropped; got %d", n)
	}
}

// TestReverseSupervisorSurfacesDialFailure pins observability: while dials keep
// failing (e.g. the agent answers 503 "not paired yet"), the tracked reverse
// status must carry the LAST dial error, not a bare "dialing" — the admin
// surface is otherwise blind to a permanently failing dial loop.
func TestReverseSupervisorSurfacesDialFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	agentPub, _ := genEd25519(t)
	_, serverPriv := genEd25519(t)

	// Stands in for an agent whose data listener is not up: every dial gets a
	// plain-HTTP 503, so the websocket handshake fails on each attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "agent not paired yet", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	fx.agent.PubKey = base64.StdEncoding.EncodeToString(agentPub)
	fx.agent.Mode = model.AgentModeReverse
	fx.agent.ReverseAddr = "ws" + strings.TrimPrefix(srv.URL, "http")
	fx.agent.Enabled = true
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatalf("update agent to reverse: %v", err)
	}

	sup := newReverseSupervisor(serverPriv)
	sup.listAgents = (&staticAgents{agents: []model.Agent{*fx.agent}}).list
	sup.backoff = channel.BackoffConfig{Initial: 50 * time.Millisecond, Max: 100 * time.Millisecond}
	t.Cleanup(sup.Stop)
	sup.Start()

	waitFor(t, 5*time.Second, func() bool {
		st, ok := sup.statusOf(fx.agent.AgentID)
		return ok && st.State == reverseStateDialing && strings.Contains(st.Detail, "bad handshake")
	})
}

// TestReverseSupervisorNoReverseAgentsNoGoroutine pins the no-op property: with
// zero reverse agents, Start spins NO control goroutine and dials nothing.
func TestReverseSupervisorNoReverseAgentsNoGoroutine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, serverPriv := genEd25519(t)

	// An outbound agent in the set must not trigger any dialing.
	outbound := model.Agent{AgentID: "outbound-x", Enabled: true, Mode: model.AgentModeOutbound}
	sup := newReverseSupervisor(serverPriv)
	sup.listAgents = (&staticAgents{agents: []model.Agent{outbound}}).list
	t.Cleanup(sup.Stop)
	sup.Start()

	sup.mu.Lock()
	running := sup.ctrlRunning
	n := len(sup.maintainers)
	sup.mu.Unlock()
	if running {
		t.Fatal("no reverse agents configured: the control goroutine must not start")
	}
	if n != 0 {
		t.Fatalf("no reverse agents configured: no maintainers, got %d", n)
	}
}

// TestReverseSupervisorLazyStartOnRefresh pins that a reverse agent APPEARING
// after boot (admin configures it → Refresh) lazily starts the supervisor.
func TestReverseSupervisorLazyStartOnRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	serverPub, serverPriv := genEd25519(t)

	var dialed atomic.Int64
	rev := newTestReverseAgent(t, fx, serverPub, func(ch *channel.Channel) { dialed.Add(1) })

	src := &staticAgents{agents: nil} // start empty
	sup := newReverseSupervisor(serverPriv)
	sup.listAgents = src.list
	sup.dispatchInterval = 100 * time.Millisecond
	sup.reEnumInterval = time.Hour
	t.Cleanup(sup.Stop)
	sup.Start() // no-op, nothing configured yet

	sup.mu.Lock()
	running := sup.ctrlRunning
	sup.mu.Unlock()
	if running {
		t.Fatal("control loop must not run before any reverse agent exists")
	}

	// Admin configures the reverse agent, then the PATCH signals Refresh.
	src.set([]model.Agent{rev})
	sup.Refresh()
	waitFor(t, 5*time.Second, func() bool { return dialed.Load() >= 1 })
}

// TestReverseSupervisorEndToEnd is the R4 e2e over loopback: a reverse agent
// configured in the DB → the supervisor (started via the real boot entrypoint
// InitAgentReverseSupervisor + SetServerIdentity) dials the loopback listener →
// the serve-loop ReverseAgent handles the push → the copy runs back over the
// same channel into the shared cores → the destination bytes land on disk and
// the task is marked done.
func TestReverseSupervisorEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	// The channel src-link falls back to this server's signed /d route.
	r := gin.New()
	r.GET("/d/*path", middlewares.Down(), Down)
	dsrv := httptest.NewServer(r)
	defer dsrv.Close()
	oldSiteURL := conf.Conf.SiteURL
	conf.Conf.SiteURL = dsrv.URL
	t.Cleanup(func() { conf.Conf.SiteURL = oldSiteURL })

	task := fx.createTask(t, model.AgentTaskPending)

	// A real server identity + boot wiring. The listener pins this server pub.
	si := newTestServerIdentity(t)
	agentsrv.SetServerIdentity(si)
	t.Cleanup(func() { agentsrv.SetServerIdentity(nil) })
	serverPub := mustDecodePub(t, si.PubKeyBase64())

	// The serve-loop: the agent listener spins a ReverseAgent per channel, just
	// like cmd/agent.go's OnChannel does.
	done := make(chan error, 1)
	newTestReverseAgent(t, fx, serverPub, func(ch *channel.Channel) {
		ra := agentpkg.NewReverseAgent(ch)
		ra.OnTaskDone = func(_ string, err error) { done <- err }
	})

	// Real boot entrypoint (reads the process server identity, enumerates the DB).
	InitAgentReverseSupervisor()
	t.Cleanup(func() {
		if s := getReverseSupervisor(); s != nil {
			s.Stop()
		}
		currentReverseSupervisorMu.Lock()
		currentReverseSupervisor = nil
		currentReverseSupervisorMu.Unlock()
	})
	// Speed the singleton's dispatch tick without changing production defaults:
	// dispatch fires immediately on connect anyway, so the pending task goes out
	// at once.

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reverse copy failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("reverse copy never completed")
	}

	got, err := os.ReadFile(filepath.Join(fx.diskDir, "dst", "file.bin"))
	if err != nil {
		t.Fatalf("destination must exist after the reverse copy: %v", err)
	}
	if string(got) != "agent task payload" {
		t.Fatalf("reverse-copied content = %q", got)
	}
	final, err := db.GetAgentTaskByTaskID(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != model.AgentTaskDone || final.Progress != 100 {
		t.Fatalf("final task state = %+v, want done/100", final)
	}
}

// --- helpers ---

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func newTestServerIdentity(t *testing.T) *agentsrv.ServerIdentity {
	t.Helper()
	si, err := agentsrv.LoadOrCreateServerIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	return si
}

func mustDecodePub(t *testing.T, b64 string) ed25519.PublicKey {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		t.Fatalf("bad server pubkey")
	}
	return ed25519.PublicKey(raw)
}
