package channel

import (
	"crypto/ed25519"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/gorilla/websocket"
)

// This file is the AGENT (listener) side of the reverse connection (design R2):
// the agent, which HAS a public route, exposes ONE WebSocket route that the
// server dials into. It is a NEW anonymous-until-handshake inbound surface, so
// it is bounded on every axis: only a WS-upgrade request is accepted (anything
// else is rejected), a per-IP throttle blunts a handshake-flood, the handshake
// itself is size- and time-bounded (see handshake.go), and an agent with no
// PINNED server key refuses to construct a listener at all (fail closed).

// ListenerConfig configures the agent's reverse-channel listener. The listener
// verifies every inbound server against PinnedServerPub — the key handed over
// and pinned at pairing — and signs its reply with AgentPriv.
type ListenerConfig struct {
	// AgentID is this agent's stable id (must match the ServerHello's agent_id).
	AgentID string
	// AgentPriv is this agent's long-term ed25519 private key (Identity.PrivKey).
	AgentPriv ed25519.PrivateKey
	// PinnedServerPub is the server public key pinned at pairing. If empty the
	// listener refuses to build (fail closed): with no key to verify the server
	// against, no inbound connection can ever be trusted.
	PinnedServerPub ed25519.PublicKey
	// OnChannel is invoked with the live Channel once the handshake succeeds.
	// R2 keeps it alive / logs; R3 registers domain handlers on it. Required.
	OnChannel func(*Channel)
	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time
}

// handshakeThrottle rate-limits handshake attempts per client IP on this
// anonymous inbound surface. Self-contained (fixed thresholds, its own
// go-cache) so the agent process — which has no settings DB — needs no server
// config to get basic flood protection.
type handshakeThrottle struct {
	cache  cache.ICache[int]
	max    int
	window time.Duration
}

func newHandshakeThrottle() *handshakeThrottle {
	return &handshakeThrottle{cache: cache.NewMemCache[int](), max: 20, window: time.Minute}
}

func (t *handshakeThrottle) blocked(ip string) bool {
	n, ok := t.cache.Get(ip)
	return ok && n >= t.max
}

func (t *handshakeThrottle) record(ip string) {
	n, _ := t.cache.Get(ip)
	t.cache.Set(ip, n+1, cache.WithEx[int](t.window))
}

// Listener is the agent's reverse-channel HTTP handler. Build it with
// NewListener and mount its ServeHTTP on a single route (e.g. /agentchannel).
type Listener struct {
	cfg       ListenerConfig
	upgrader  websocket.Upgrader
	throttle  *handshakeThrottle
	seenMu    sync.Mutex
	seenNonce cache.ICache[bool]

	// cur holds the single currently-live channel this listener has accepted,
	// if any. Channel-stability wave 1: a reverse agent should never end up
	// serving two concurrent channels for the same server relationship — e.g.
	// a client that redials before noticing its old socket is actually dead
	// (a slow/ambiguous NAT teardown) would otherwise leave a stale channel
	// dispatching tasks alongside the new one. Every successful handshake
	// atomically swaps itself in here and closes whatever was there before,
	// so at most one channel is ever live at a time.
	cur atomic.Pointer[Channel]
}

// NewListener validates cfg (notably: a non-empty pinned server key — fail
// closed) and returns a ready handler. A missing pinned key, identity, or
// callback is a hard error so a misconfigured `agent serve` refuses to start
// rather than exposing an unauthenticated surface.
func NewListener(cfg ListenerConfig) (*Listener, error) {
	if len(cfg.PinnedServerPub) != ed25519.PublicKeySize {
		return nil, errors.New("agentchan: refusing to listen — no pinned server public key (reverse mode requires the key pinned at pairing)")
	}
	if len(cfg.AgentPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("agentchan: refusing to listen — agent private key is missing/malformed")
	}
	if cfg.AgentID == "" {
		return nil, errors.New("agentchan: refusing to listen — agentID is required")
	}
	if cfg.OnChannel == nil {
		return nil, errors.New("agentchan: refusing to listen — OnChannel callback is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Listener{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: handshakeStepTimeout,
			ReadBufferSize:   4 << 10,
			WriteBufferSize:  4 << 10,
			// This is a machine-to-machine API, not a browser origin; the
			// mutual-auth handshake — not an Origin header — is the security
			// boundary, so accept any origin and let the handshake decide.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		throttle:  newHandshakeThrottle(),
		seenNonce: cache.NewMemCache[bool](),
	}, nil
}

// ServeHTTP upgrades a WebSocket request, runs the agent leg of the handshake,
// and on success hands the live Channel to OnChannel. Non-WS requests, blocked
// IPs, and failed handshakes are rejected without a channel ever going live.
func (l *Listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if l.throttle.blocked(ip) {
		http.Error(w, "too many handshake attempts", http.StatusTooManyRequests)
		return
	}
	// Reject anything that is not a WebSocket upgrade before touching the
	// handshake: the surface exposes exactly one thing.
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "expected a websocket upgrade", http.StatusBadRequest)
		return
	}
	conn, err := l.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		l.throttle.record(ip)
		return
	}

	sessionKey, err := RunAgentHandshake(conn, l.cfg.AgentPriv, l.cfg.AgentID, l.cfg.PinnedServerPub, l.cfg.Now(), l.seenServerNonce)
	if err != nil {
		l.throttle.record(ip)
		_ = conn.Close()
		return
	}

	ch, err := newChannel(conn, "a", sessionKey)
	if err != nil {
		_ = conn.Close()
		return
	}
	ch.Start()
	// Enforce single-channel-per-listener: swap this newly-live channel in and
	// close out whatever channel was previously live, so a duplicate/stale
	// connection can never keep dispatching tasks alongside the new one.
	//
	// Review note (post-wave-1, recorded not acted on): the Swap and the
	// OnChannel callback below are NOT one atomic step. Two concurrent
	// ServeHTTP goroutines can both reach here close together; whichever
	// finishes its handshake second wins the Swap (closes the first's
	// channel), but the FIRST goroutine's OnChannel can still fire afterwards
	// — with a channel that is already closed. The current consumer
	// (cmd/agent.go's buildReverseChannelListener OnChannel) only logs
	// "reverse channel established" in that case; NewReverseAgent registering
	// handlers on an already-dead channel is harmless (nothing will ever
	// dispatch on it, and its own Done()/Err() already reflect the teardown).
	// Not fixed here — no observed harm, and true atomicity would need
	// OnChannel to run while still holding whatever serializes the swap,
	// which is a bigger change than this wave's scope.
	if old := l.cur.Swap(ch); old != nil {
		_ = old.Close()
	}
	l.cfg.OnChannel(ch)
}

// seenServerNonce is the listener's defense-in-depth replay cache: a ServerHello
// nonce already accepted (within the freshness window) is refused if replayed.
func (l *Listener) seenServerNonce(nonce string) bool {
	l.seenMu.Lock()
	defer l.seenMu.Unlock()
	if _, ok := l.seenNonce.Get(nonce); ok {
		return true
	}
	// Remember for longer than the freshness window so a nonce can never age
	// out of the cache while a hello bearing it could still pass freshness.
	l.seenNonce.Set(nonce, true, cache.WithEx[bool](2*handshakeFreshness))
	return false
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
