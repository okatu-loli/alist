package handles

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AlistGo/alist-agent/channel"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	log "github.com/sirupsen/logrus"
)

// Server-side reverse-connection supervisor (design R4). In reverse mode the
// server has the reachable route: it DIALS each configured reverse agent's WS
// listener, holds a persistent mutual-authed AEAD channel, serves the agent's
// cap frames over it (RegisterAgentChannelHandlers), and PUSHES pending tasks
// down it. This file owns the lifecycle of those dials.
//
// Lifecycle:
//   - enumerate: db.ListAgents() filtered to Mode=="reverse" && ReverseAddr!=""
//     && Enabled — the "desired" set.
//   - dial: one maintainer goroutine per desired agent, each looping
//     DialWithBackoff(Manager.Dial) → the Manager resolves the agent's pinned
//     pubkey + Enabled through db.GetAgentByAgentID BEFORE opening the socket
//     (fail closed on unknown/disabled), runs the server leg of the handshake,
//     and hands back a live channel. On the live channel the maintainer
//     registers the cap handlers and dispatches on a ticker until the channel
//     drops, then re-dials (backoff).
//   - refresh: a periodic re-enumerate (reEnumInterval) plus an explicit signal
//     (Refresh, fired by the admin PATCH) reconciles desired vs running — new
//     reverse agents start a maintainer, agents that stopped being
//     reverse/enabled/present get their maintainer cancelled and channel closed.
//   - no-op when empty: Start with zero desired reverse agents spins NO
//     goroutine at all (not even the control loop). The control loop is started
//     lazily the moment the first reverse agent appears (Start finds one, or a
//     later Refresh does), and supervises from there.
//
// The dispatch id is ALWAYS the handshake-proven id: the maintainer registers
// handlers bound to the very id it resolved+dialed+handshook, and dispatches
// only through that bound handler's DispatchPending — never the free
// DispatchAgentTaskOverChannel with a guessed id (design R4 security contract).

const (
	// reverseDispatchInterval is how often, while a channel is live, the
	// supervisor re-checks for a pending/active task to push down it. This can
	// stay short (a fresh pending task should go out fast) because dispatchOnce
	// no longer re-pushes unconditionally: the bound handler throttles re-pushes
	// of a still-active task to at most once per redispatchInterval (see
	// agent_channel.go), so a short tick keeps latency low for genuinely new
	// tasks without spamming task.dispatch (and the audit log) at an already
	// running one every tick.
	reverseDispatchInterval = 3 * time.Second
	// reverseReEnumInterval is how often the supervisor re-enumerates the DB to
	// pick up newly-configured reverse agents / drop removed ones, independent
	// of the admin PATCH's explicit Refresh signal (which reconciles
	// immediately). A minute is plenty: config changes are rare and the admin
	// path already signals, so this is just the backstop.
	reverseReEnumInterval = time.Minute
)

// reverseConnState is a coarse connection status for the admin surface.
type reverseConnState string

const (
	reverseStateDialing   reverseConnState = "dialing"
	reverseStateConnected reverseConnState = "connected"
	reverseStateError     reverseConnState = "error"
)

// ReverseStatus is the per-agent reverse-connection status the admin DTO/status
// endpoint surfaces. Zero value (State "") means "no reverse supervisor is
// tracking this agent" — e.g. an outbound agent, or before the first dial.
type ReverseStatus struct {
	State    reverseConnState `json:"state,omitempty"`
	Detail   string           `json:"detail,omitempty"`
	LastSeen *time.Time       `json:"last_seen,omitempty"`
}

// reverseSupervisor holds the running reverse dials. It is a process-wide
// singleton (see currentReverseSupervisor) so the admin PATCH can Refresh it.
type reverseSupervisor struct {
	serverPriv ed25519.PrivateKey
	mgr        *channel.Manager

	// listAgents is injectable so tests drive the enumerate step off a fake set
	// without a live DB; production uses db.ListAgents.
	listAgents func() ([]model.Agent, error)

	dispatchInterval time.Duration
	reEnumInterval   time.Duration
	backoff          channel.BackoffConfig

	mu          sync.Mutex
	ctrlRunning bool
	ctx         context.Context
	cancel      context.CancelFunc
	refresh     chan struct{}
	maintainers map[string]*reverseMaintainer // keyed by agentID

	statusMu sync.Mutex
	status   map[string]ReverseStatus // keyed by agentID

	// chanMu guards chans: the LIVE channel per connected agent, so an admin
	// observability pull (agent.log.tail — design SP-G1) can reach the exact
	// channel this agent's maintainer is holding. A maintainer publishes its
	// channel here the instant the handshake proves live and removes it the
	// instant the channel drops, so channelFor returns non-nil ONLY while a
	// genuine, authenticated channel is up — the fail-closed source of truth
	// for "is this agent connected right now".
	chanMu sync.Mutex
	chans  map[string]*channel.Channel // keyed by agentID
}

// reverseMaintainer is one agent's dial/hold/dispatch/reconnect loop.
type reverseMaintainer struct {
	agentID string
	addr    string
	cancel  context.CancelFunc
	done    chan struct{}
}

// newReverseSupervisor builds a supervisor signing with serverPriv and
// resolving agents through db.GetAgentByAgentID (so every dial re-verifies the
// pinned pubkey + Enabled at the DB before the socket opens — fail closed).
func newReverseSupervisor(serverPriv ed25519.PrivateKey) *reverseSupervisor {
	s := &reverseSupervisor{
		serverPriv:       serverPriv,
		listAgents:       db.ListAgents,
		dispatchInterval: reverseDispatchInterval,
		reEnumInterval:   reverseReEnumInterval,
		// Channel-stability wave 1: a channel that drops (peer restart, brief
		// network blip, the new heartbeat detecting a genuinely dead peer)
		// should be re-dialed quickly rather than waiting out the library
		// default (1s initial / 30s max) — a short reverse-agent outage should
		// self-heal in well under a minute. 200ms initial still backs off
		// (doubling, per BackoffConfig's default Factor) up to a 15s cap so a
		// persistently-failing agent (misconfigured, unreachable) doesn't spin
		// the dial loop hot.
		backoff:     channel.BackoffConfig{Initial: 200 * time.Millisecond, Max: 15 * time.Second},
		refresh:     make(chan struct{}, 1),
		maintainers: make(map[string]*reverseMaintainer),
		status:      make(map[string]ReverseStatus),
		chans:       make(map[string]*channel.Channel),
	}
	s.mgr = channel.NewManager(serverPriv, dbAgentResolver)
	return s
}

// dbAgentResolver adapts db.GetAgentByAgentID to a channel.AgentResolver: it
// returns the agent's pinned ed25519 public key and Enabled flag, or an error
// for an unknown/malformed row. The Manager refuses to dial an agent this
// rejects or reports disabled — the reverse mirror of the heartbeat path's
// AgentDisabled refusal.
func dbAgentResolver(agentID string) (ed25519.PublicKey, bool, error) {
	a, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		return nil, false, err
	}
	pub, err := base64.StdEncoding.DecodeString(a.PubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, false, fmt.Errorf("agent %s has a malformed pinned public key", agentID)
	}
	return ed25519.PublicKey(pub), a.Enabled, nil
}

// desiredSet returns the reverse agents that should have a live dial right now:
// Mode=="reverse", a non-empty ReverseAddr, and Enabled. Keyed by AgentID ->
// ReverseAddr so reconcile can diff both membership and address changes.
func (s *reverseSupervisor) desiredSet() (map[string]string, error) {
	agents, err := s.listAgents()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, a := range agents {
		if a.IsReverse() && a.ReverseAddr != "" && a.Enabled {
			out[a.AgentID] = a.ReverseAddr
		}
	}
	return out, nil
}

// Start does an initial reconcile. If there is no reverse agent to supervise it
// returns having spun NOTHING (true no-op). Otherwise it starts the single
// control goroutine, which then owns periodic re-enumeration + refresh.
func (s *reverseSupervisor) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	desired, err := s.desiredSet()
	if err != nil {
		log.Errorf("reverse supervisor: initial enumerate failed: %+v", err)
		return
	}
	if len(desired) == 0 {
		// No-op: nothing to dial, so no control loop and no maintainers. A later
		// Refresh (admin configures a reverse agent) starts everything lazily.
		return
	}
	s.ensureControlLocked()
}

// Refresh reconciles desired vs running as soon as possible: it starts the
// control loop if it is not already running (the first reverse agent appearing
// after boot), then pokes it. Non-blocking; a coalesced signal is fine.
func (s *reverseSupervisor) Refresh() {
	s.mu.Lock()
	s.ensureControlLocked()
	s.mu.Unlock()
	select {
	case s.refresh <- struct{}{}:
	default:
	}
}

// Stop tears down every maintainer and the control loop (server shutdown / test
// cleanup).
func (s *reverseSupervisor) Stop() {
	s.mu.Lock()
	running := s.ctrlRunning
	cancel := s.cancel
	s.mu.Unlock()
	if running && cancel != nil {
		cancel()
	}
}

// ensureControlLocked starts the control goroutine if it is not already
// running. Caller holds s.mu.
func (s *reverseSupervisor) ensureControlLocked() {
	if s.ctrlRunning {
		return
	}
	s.ctrlRunning = true
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.control(s.ctx)
}

// control is the single supervisory goroutine: reconcile immediately, then on
// each re-enum tick or refresh signal, until ctx is cancelled. On exit it stops
// every maintainer and clears the running flag so a later Refresh restarts a
// fresh control loop.
func (s *reverseSupervisor) control(ctx context.Context) {
	ticker := time.NewTicker(s.reEnumInterval)
	defer ticker.Stop()
	s.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			s.stopAllMaintainers()
			s.mu.Lock()
			s.ctrlRunning = false
			s.mu.Unlock()
			return
		case <-ticker.C:
			s.reconcile(ctx)
		case <-s.refresh:
			s.reconcile(ctx)
		}
	}
}

// reconcile diffs the desired reverse-agent set against the running maintainers
// and starts/stops/replaces them to match.
func (s *reverseSupervisor) reconcile(ctx context.Context) {
	desired, err := s.desiredSet()
	if err != nil {
		log.Errorf("reverse supervisor: enumerate failed: %+v", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return
	}

	// Stop maintainers no longer desired, or whose address changed (restart with
	// the new address on the next pass).
	for id, m := range s.maintainers {
		addr, want := desired[id]
		if !want || addr != m.addr {
			m.cancel()
			delete(s.maintainers, id)
			s.clearStatus(id)
			s.mgr.CloseAgent(id)
		}
	}

	// Start maintainers for newly-desired agents.
	for id, addr := range desired {
		if _, running := s.maintainers[id]; running {
			continue
		}
		mctx, mcancel := context.WithCancel(ctx)
		m := &reverseMaintainer{agentID: id, addr: addr, cancel: mcancel, done: make(chan struct{})}
		s.maintainers[id] = m
		go s.maintain(mctx, m)
	}
}

// stopAllMaintainers cancels every running maintainer and closes its channel.
func (s *reverseSupervisor) stopAllMaintainers() {
	s.mu.Lock()
	ms := make([]*reverseMaintainer, 0, len(s.maintainers))
	for id, m := range s.maintainers {
		ms = append(ms, m)
		delete(s.maintainers, id)
	}
	s.mu.Unlock()
	for _, m := range ms {
		m.cancel()
		s.clearStatus(m.agentID)
		s.mgr.CloseAgent(m.agentID)
	}
}

// maintain is one agent's forever loop: dial (with backoff) → register handlers
// bound to the handshake-proven id → dispatch until the channel drops → re-dial,
// all until mctx is cancelled (agent removed/disabled/addr-changed, or supervisor
// stopped).
func (s *reverseSupervisor) maintain(mctx context.Context, m *reverseMaintainer) {
	defer close(m.done)
	for {
		if mctx.Err() != nil {
			return
		}
		s.setStatus(m.agentID, ReverseStatus{State: reverseStateDialing, Detail: "dialing " + m.addr})
		ch, err := channel.DialWithBackoff(mctx, s.backoff, func(c context.Context) (*channel.Channel, error) {
			ch, derr := s.mgr.Dial(c, m.agentID, m.addr)
			if derr != nil && c.Err() == nil {
				// Surface the failure on the admin status while the backoff loop
				// keeps retrying — a permanently failing dial (e.g. the agent
				// answers 503 "not paired yet") is otherwise invisible.
				s.setStatus(m.agentID, ReverseStatus{State: reverseStateDialing, Detail: fmt.Sprintf("dialing %s; last error: %v", m.addr, derr)})
			}
			return ch, derr
		})
		if err != nil {
			// Only ctx cancellation ends DialWithBackoff with an error.
			if mctx.Err() == nil {
				s.setStatus(m.agentID, ReverseStatus{State: reverseStateError, Detail: err.Error()})
			}
			return
		}

		// The handshake proved this agent's identity == m.agentID (the Manager
		// resolved that id's pinned pubkey and the agent signed against it), so
		// binding the handlers — and therefore DispatchPending — to m.agentID
		// carries the proven identity. There is no other id in play.
		bound := RegisterAgentChannelHandlers(ch, m.agentID)
		now := time.Now()
		s.setStatus(m.agentID, ReverseStatus{State: reverseStateConnected, Detail: "channel live", LastSeen: &now})
		// Publish the live channel so an admin log-tail pull can reach it while
		// it is up (design SP-G1). Cleared the instant serveChannel returns
		// (channel dropped or maintainer cancelled), so it is never a stale
		// handle to a dead channel.
		s.setChannel(m.agentID, ch)
		log.Infof("reverse supervisor: channel live to agent %s (%s)", m.agentID, m.addr)

		s.serveChannel(mctx, ch, bound)
		s.clearChannel(m.agentID)

		if mctx.Err() != nil {
			_ = ch.Close()
			return
		}
		// Channel dropped on its own while still desired: loop and re-dial.
		log.Warnf("reverse supervisor: channel to agent %s closed: %v; re-dialing", m.agentID, ch.Err())
	}
}

// serveChannel dispatches pending tasks over a live channel until it drops or
// mctx is cancelled. It dispatches once immediately (so a task waiting at
// connect time goes out without waiting a full tick) then on every tick.
func (s *reverseSupervisor) serveChannel(mctx context.Context, ch *channel.Channel, bound *agentChannelHandler) {
	ticker := time.NewTicker(s.dispatchInterval)
	defer ticker.Stop()
	s.dispatchOnce(mctx, bound)
	for {
		select {
		case <-mctx.Done():
			return
		case <-ch.Done():
			return
		case <-ticker.C:
			s.dispatchOnce(mctx, bound)
		}
	}
}

// dispatchOnce pushes any pending/active task over the bound handler's channel
// using ONLY the handshake-proven bound id (structural R4 contract). On a
// successful dispatch it refreshes the agent's last-seen-over-channel marker.
func (s *reverseSupervisor) dispatchOnce(ctx context.Context, bound *agentChannelHandler) {
	dispatched, err := bound.DispatchPending(ctx)
	if err != nil {
		log.Errorf("reverse supervisor: dispatch to agent %s failed: %+v", bound.AgentID(), err)
		return
	}
	if dispatched {
		now := time.Now()
		s.setStatus(bound.AgentID(), ReverseStatus{State: reverseStateConnected, Detail: "dispatched task", LastSeen: &now})
	}
}

func (s *reverseSupervisor) setStatus(agentID string, st ReverseStatus) {
	s.statusMu.Lock()
	s.status[agentID] = st
	s.statusMu.Unlock()
}

func (s *reverseSupervisor) clearStatus(agentID string) {
	s.statusMu.Lock()
	delete(s.status, agentID)
	s.statusMu.Unlock()
}

func (s *reverseSupervisor) statusOf(agentID string) (ReverseStatus, bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	st, ok := s.status[agentID]
	return st, ok
}

func (s *reverseSupervisor) setChannel(agentID string, ch *channel.Channel) {
	s.chanMu.Lock()
	s.chans[agentID] = ch
	s.chanMu.Unlock()
}

func (s *reverseSupervisor) clearChannel(agentID string) {
	s.chanMu.Lock()
	delete(s.chans, agentID)
	s.chanMu.Unlock()
}

// channelFor returns the live channel currently held for agentID, or nil if the
// agent is not connected right now — the fail-closed signal the log-tail
// endpoint keys on.
func (s *reverseSupervisor) channelFor(agentID string) *channel.Channel {
	s.chanMu.Lock()
	defer s.chanMu.Unlock()
	return s.chans[agentID]
}

// currentReverseSupervisor is the process-wide singleton the admin PATCH
// signals and the status endpoint reads.
var (
	currentReverseSupervisorMu sync.RWMutex
	currentReverseSupervisor   *reverseSupervisor
)

// InitAgentReverseSupervisor loads the process server identity (design R1) and,
// if one is wired, builds + starts the reverse supervisor (design R4). Called
// from cmd/common.go Init() after InitServerIdentity(). It is a no-op when no
// server identity is present (reverse mode needs the server's private key to
// sign the handshake) or when no reverse agents are configured (Start spins no
// goroutine). Safe to call once at boot.
func InitAgentReverseSupervisor() {
	si := agentsrv.CurrentServerIdentity()
	if si == nil {
		// No reverse identity wired: pairing omits server_pubkey and reverse
		// mode is unavailable this run. Nothing to supervise.
		return
	}
	s := newReverseSupervisor(si.SigningKey())
	currentReverseSupervisorMu.Lock()
	currentReverseSupervisor = s
	currentReverseSupervisorMu.Unlock()
	s.Start()
}

// getReverseSupervisor returns the process-wide supervisor, or nil.
func getReverseSupervisor() *reverseSupervisor {
	currentReverseSupervisorMu.RLock()
	defer currentReverseSupervisorMu.RUnlock()
	return currentReverseSupervisor
}

// signalReverseSupervisorRefresh nudges the running supervisor to re-dial after
// an admin mode/addr/enabled change. No-op if no supervisor is running (e.g. a
// boot with no server identity) — the change still lands in the DB and a later
// enumerate picks it up if a supervisor is ever started.
func signalReverseSupervisorRefresh() {
	if s := getReverseSupervisor(); s != nil {
		s.Refresh()
	}
}

// reverseStatusFor returns the supervisor's tracked reverse status for an agent,
// or the zero value if none is tracked (outbound agent, or no supervisor).
func reverseStatusFor(agentID string) ReverseStatus {
	s := getReverseSupervisor()
	if s == nil {
		return ReverseStatus{}
	}
	st, _ := s.statusOf(agentID)
	return st
}

// reverseChannelFor returns the live reverse channel the supervisor holds for
// an agent, or nil when none is up (no supervisor, or the agent is not
// connected right now). The log-tail endpoint uses it as its fail-closed
// "connected?" check (design SP-G1).
func reverseChannelFor(agentID string) *channel.Channel {
	s := getReverseSupervisor()
	if s == nil {
		return nil
	}
	return s.channelFor(agentID)
}

// errNoReverseSupervisor is returned by the status endpoint when reverse mode
// is unavailable this run (no server identity).
var errNoReverseSupervisor = errors.New("reverse connection mode is not available (no server identity configured)")
