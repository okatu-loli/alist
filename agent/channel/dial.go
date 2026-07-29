package channel

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// This file is the SERVER (dialer) side of the reverse connection (design R2):
// the server, which HAS a reachable route to the agent's WS listener, dials in,
// proves itself, verifies the agent, and holds the live Channel. R4 wires
// "which agents are reverse-mode and at what URL"; R2 provides the primitive
// (DialAgent), a small reconnect/backoff helper, and a Manager that resolves an
// agent's stored pubkey+Enabled before dialing and holds the resulting
// channels. Tests drive it against a loopback agent listener.

// dialHandshakeTimeout bounds the whole dial+handshake so a hung agent can't
// wedge a dial goroutine. The per-step handshake deadlines back this up.
const dialHandshakeTimeout = 20 * time.Second

// DialAgent dials the agent's WebSocket listener at wsURL (ws:// or wss://),
// runs the SERVER leg of the mutual-auth handshake (signing with serverPriv,
// verifying the agent against agentPub), and returns a live Channel on success.
// On any handshake failure it closes the connection and returns the error —
// the channel never goes live. The caller is responsible for closing the
// returned Channel.
func DialAgent(ctx context.Context, wsURL string, serverPriv ed25519.PrivateKey, agentPub ed25519.PublicKey, agentID string) (*Channel, error) {
	if len(serverPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("agentchan: server private key is missing/malformed")
	}
	if len(agentPub) != ed25519.PublicKeySize {
		return nil, errors.New("agentchan: agent public key is missing/malformed")
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialHandshakeTimeout)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("agentchan: dial %s: %w", wsURL, err)
	}
	sessionKey, err := RunServerHandshake(conn, serverPriv, agentPub, agentID, time.Now())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	ch, err := newChannel(conn, "s", sessionKey)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	ch.Start()
	return ch, nil
}

// AgentResolver looks up a paired agent's public key and Enabled flag by its
// AgentID. It is the seam the Manager uses to enforce "server only dials a
// KNOWN, ENABLED agent" without this package importing internal/db. A real
// wiring resolves against db.GetAgentByAgentID; tests supply a fake.
type AgentResolver func(agentID string) (pubKey ed25519.PublicKey, enabled bool, err error)

// Manager holds the server's live reverse channels keyed by AgentID and gates
// every dial through the resolver (unknown or disabled agents are refused
// before the socket is even opened). It is the R2 stub of the server-side
// reverse-channel supervisor; R4 adds the config of which agents/URLs to dial.
type Manager struct {
	serverPriv ed25519.PrivateKey
	resolve    AgentResolver

	mu       sync.Mutex
	channels map[string]*Channel
}

// NewManager builds a Manager that signs handshakes with serverPriv and
// resolves agents through resolve.
func NewManager(serverPriv ed25519.PrivateKey, resolve AgentResolver) *Manager {
	return &Manager{
		serverPriv: serverPriv,
		resolve:    resolve,
		channels:   make(map[string]*Channel),
	}
}

// Dial resolves agentID (rejecting unknown or DISABLED agents up front), dials
// its listener at wsURL, runs the handshake, and stores + returns the live
// channel. An existing live channel for the same agent is closed and replaced.
func (m *Manager) Dial(ctx context.Context, agentID, wsURL string) (*Channel, error) {
	if m.resolve == nil {
		return nil, errors.New("agentchan: manager has no agent resolver")
	}
	pub, enabled, err := m.resolve(agentID)
	if err != nil {
		return nil, fmt.Errorf("agentchan: resolve agent %s: %w", agentID, err)
	}
	if !enabled {
		// Fail closed: a disabled (or unconfirmed) agent must not get a live
		// reverse channel, mirroring the heartbeat path's AgentDisabled refusal.
		return nil, fmt.Errorf("agentchan: agent %s is not enabled; refusing to dial", agentID)
	}
	ch, err := DialAgent(ctx, wsURL, m.serverPriv, pub, agentID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if old := m.channels[agentID]; old != nil {
		_ = old.Close()
	}
	m.channels[agentID] = ch
	m.mu.Unlock()

	// Reap the map entry when this channel dies so Get never hands back a dead
	// channel that Dial replaced or the peer dropped.
	go func() {
		<-ch.Done()
		m.mu.Lock()
		if m.channels[agentID] == ch {
			delete(m.channels, agentID)
		}
		m.mu.Unlock()
	}()

	return ch, nil
}

// Get returns the live channel for agentID, if any.
func (m *Manager) Get(agentID string) (*Channel, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[agentID]
	return ch, ok
}

// CloseAgent tears down and forgets agentID's channel.
func (m *Manager) CloseAgent(agentID string) {
	m.mu.Lock()
	ch := m.channels[agentID]
	delete(m.channels, agentID)
	m.mu.Unlock()
	if ch != nil {
		_ = ch.Close()
	}
}

// CloseAll tears down every held channel (server shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	chs := make([]*Channel, 0, len(m.channels))
	for _, ch := range m.channels {
		chs = append(chs, ch)
	}
	m.channels = make(map[string]*Channel)
	m.mu.Unlock()
	for _, ch := range chs {
		_ = ch.Close()
	}
}

// BackoffConfig tunes DialWithBackoff. Zero values fall back to sane defaults.
type BackoffConfig struct {
	Initial time.Duration
	Max     time.Duration
	Factor  float64
}

func (b BackoffConfig) withDefaults() BackoffConfig {
	if b.Initial <= 0 {
		b.Initial = time.Second
	}
	if b.Max <= 0 {
		b.Max = 30 * time.Second
	}
	if b.Factor <= 1 {
		b.Factor = 2
	}
	return b
}

// DialWithBackoff repeatedly invokes dial until it succeeds or ctx is done,
// sleeping an exponentially growing (capped) delay between attempts. It is the
// reconnect helper a reverse-mode supervisor (R4) loops on; R2 ships and tests
// the primitive. The returned Channel is live; the caller owns closing it.
func DialWithBackoff(ctx context.Context, cfg BackoffConfig, dial func(context.Context) (*Channel, error)) (*Channel, error) {
	cfg = cfg.withDefaults()
	delay := cfg.Initial
	for {
		ch, err := dial(ctx)
		if err == nil {
			return ch, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("agentchan: dial retries aborted: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay = time.Duration(float64(delay) * cfg.Factor)
		if delay > cfg.Max {
			delay = cfg.Max
		}
	}
}
