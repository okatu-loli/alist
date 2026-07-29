package channel

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startAgentListener spins up a real agent reverse-channel listener over a
// loopback HTTP server and returns its ws:// URL plus a channel that yields the
// live agent-side Channel once the server dials in and the handshake completes.
func startAgentListener(t *testing.T, agentID string, agentPriv ed25519.PrivateKey, pinnedServerPub ed25519.PublicKey) (wsURL string, agentChans <-chan *Channel) {
	t.Helper()
	got := make(chan *Channel, 1)
	l, err := NewListener(ListenerConfig{
		AgentID:         agentID,
		AgentPriv:       agentPriv,
		PinnedServerPub: pinnedServerPub,
		OnChannel:       func(ch *Channel) { got <- ch },
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), got
}

func TestEndToEndDialAndBidirectionalSend(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	const agentID = "e2e-agent"

	wsURL, agentChans := startAgentListener(t, agentID, agentPriv, serverPub)

	mgr := NewManager(serverPriv, func(id string) (ed25519.PublicKey, bool, error) {
		if id != agentID {
			return nil, false, errors.New("unknown")
		}
		return agentPub, true, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverCh, err := mgr.Dial(ctx, agentID, wsURL)
	if err != nil {
		t.Fatalf("Manager.Dial: %v", err)
	}
	defer mgr.CloseAll()

	// The manager holds the live channel.
	if got, ok := mgr.Get(agentID); !ok || got != serverCh {
		t.Fatal("manager did not store the dialed channel")
	}

	var agentCh *Channel
	select {
	case agentCh = <-agentChans:
	case <-time.After(3 * time.Second):
		t.Fatal("agent side never received a live channel")
	}

	// server -> agent
	agentCh.Handle("dispatch", func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ack":true}`), nil
	})
	resp, err := serverCh.Send(ctx, "dispatch", json.RawMessage(`{"task":"x"}`))
	if err != nil {
		t.Fatalf("server->agent Send: %v", err)
	}
	if string(resp) != `{"ack":true}` {
		t.Fatalf("unexpected resp: %s", resp)
	}

	// agent -> server (proves the channel is genuinely bidirectional)
	serverCh.Handle("heartbeat", func(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})
	resp2, err := agentCh.Send(ctx, "heartbeat", nil)
	if err != nil {
		t.Fatalf("agent->server Send: %v", err)
	}
	if string(resp2) != `{"ok":true}` {
		t.Fatalf("unexpected resp2: %s", resp2)
	}
}

// TestMITMTamperTornDown is the end-to-end proof that R2.5 closes the R2 §2
// hole. A relay sits between the server and the real agent listener. It RELAYS
// the handshake verbatim — which a MITM CAN do, since the ed25519 signatures are
// unforgeable but the messages are still relayable, exactly the R2 weakness — so
// the handshake completes and both real endpoints go live. But the relay then
// flips ONE byte of the first server→agent sealed frame. Because that frame is
// AEAD-sealed under the session key the relay does not (and cannot) know, the
// agent's AEAD open fails and its channel is torn down. Pre-R2.5 (plaintext
// frames) the tampered frame would have been processed; now it is rejected.
func TestMITMTamperTornDown(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	const agentID = "mitm-agent"

	agentURL, agentChans := startAgentListener(t, agentID, agentPriv, serverPub)

	// The relay: upgrades the server's inbound WS, dials the real agent, and
	// pumps both directions. Handshake TEXT frames pass verbatim; the FIRST
	// server→agent BINARY frame has one byte flipped.
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvConn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		agtConn, _, err := websocket.DefaultDialer.Dial(agentURL, nil)
		if err != nil {
			_ = srvConn.Close()
			return
		}
		var closeOnce sync.Once
		shut := func() { closeOnce.Do(func() { _ = srvConn.Close(); _ = agtConn.Close() }) }

		// server → agent, corrupting the first binary (sealed) frame.
		go func() {
			defer shut()
			tampered := false
			for {
				mt, data, err := srvConn.ReadMessage()
				if err != nil {
					return
				}
				if mt == websocket.BinaryMessage && !tampered {
					tampered = true
					data = append([]byte(nil), data...)
					data[len(data)-1] ^= 0x01 // flip a bit in the AEAD tag region
				}
				if err := agtConn.WriteMessage(mt, data); err != nil {
					return
				}
			}
		}()
		// agent → server, verbatim.
		go func() {
			defer shut()
			for {
				mt, data, err := agtConn.ReadMessage()
				if err != nil {
					return
				}
				if err := srvConn.WriteMessage(mt, data); err != nil {
					return
				}
			}
		}()
	}))
	defer relay.Close()
	relayURL := "ws" + strings.TrimPrefix(relay.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Server dials the relay; the handshake (relayed) completes normally.
	serverCh, err := DialAgent(ctx, relayURL, serverPriv, agentPub, agentID)
	if err != nil {
		t.Fatalf("DialAgent through relay (handshake should relay fine): %v", err)
	}
	defer serverCh.Close()

	var agentCh *Channel
	select {
	case agentCh = <-agentChans:
	case <-time.After(3 * time.Second):
		t.Fatal("agent side never went live")
	}

	// The agent registers a handler; if the tampered frame were EVER processed,
	// this would fire. It must NOT.
	handlerFired := make(chan struct{}, 1)
	agentCh.Handle("dispatch", func(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		handlerFired <- struct{}{}
		return json.RawMessage(`{"ack":true}`), nil
	})

	// Server sends: the frame is sealed, the relay flips a byte, the agent's
	// AEAD open fails → agent channel is torn down and the frame is never seen.
	go func() { _, _ = serverCh.Send(ctx, "dispatch", json.RawMessage(`{"task":"x"}`)) }()

	select {
	case <-agentCh.Done():
		// Correct: the tampered frame failed AEAD open and tore the channel down.
	case <-handlerFired:
		t.Fatal("SECURITY: agent processed a tampered frame — the AEAD did not protect the stream")
	case <-time.After(3 * time.Second):
		t.Fatal("agent channel was not torn down by the tampered frame")
	}
	if agentCh.Err() == nil {
		t.Fatal("expected a teardown error on the agent channel")
	}
}

// TestListenerClosesPriorChannelOnNewConnection pins the single-channel-per-
// listener guard (channel-stability wave 1): a second connection that
// completes the handshake against the same listener must tear down the first
// channel, so an agent process never ends up dispatching the same task twice
// over two concurrently-live channels.
func TestListenerClosesPriorChannelOnNewConnection(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	const agentID = "single-chan-agent"

	wsURL, agentChans := startAgentListener(t, agentID, agentPriv, serverPub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := DialAgent(ctx, wsURL, serverPriv, agentPub, agentID)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close()
	var firstAgentCh *Channel
	select {
	case firstAgentCh = <-agentChans:
	case <-time.After(3 * time.Second):
		t.Fatal("agent side never received the first channel")
	}

	second, err := DialAgent(ctx, wsURL, serverPriv, agentPub, agentID)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer second.Close()
	select {
	case <-agentChans:
	case <-time.After(3 * time.Second):
		t.Fatal("agent side never received the second channel")
	}

	select {
	case <-firstAgentCh.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("first channel was not closed once the second connection went live")
	}
	if firstAgentCh.Err() == nil {
		t.Fatal("expected a teardown error on the superseded first channel")
	}
}

func TestManagerRefusesUnknownAgent(t *testing.T) {
	_, serverPriv := genKey(t)
	mgr := NewManager(serverPriv, func(string) (ed25519.PublicKey, bool, error) {
		return nil, false, errors.New("agent not found")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := mgr.Dial(ctx, "ghost", "ws://127.0.0.1:1/"); err == nil {
		t.Fatal("manager must refuse an unknown agent before dialing")
	}
}

func TestManagerRefusesDisabledAgent(t *testing.T) {
	agentPub, _ := genKey(t)
	_, serverPriv := genKey(t)
	mgr := NewManager(serverPriv, func(string) (ed25519.PublicKey, bool, error) {
		return agentPub, false /* disabled */, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := mgr.Dial(ctx, "disabled-agent", "ws://127.0.0.1:1/")
	if err == nil {
		t.Fatal("manager must refuse a disabled agent")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected not-enabled error, got: %v", err)
	}
}

func TestNewListenerRefusesEmptyPinnedKey(t *testing.T) {
	_, agentPriv := genKey(t)
	_, err := NewListener(ListenerConfig{
		AgentID:         "a",
		AgentPriv:       agentPriv,
		PinnedServerPub: nil, // no pinned server key
		OnChannel:       func(*Channel) {},
	})
	if err == nil {
		t.Fatal("NewListener must refuse to build without a pinned server key (fail closed)")
	}
	if !strings.Contains(err.Error(), "pinned server public key") {
		t.Fatalf("expected pinned-key error, got: %v", err)
	}
}

func TestListenerRejectsNonWebSocket(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	_ = agentPub
	serverPub, _ := genKey(t)
	l, err := NewListener(ListenerConfig{
		AgentID:         "a",
		AgentPriv:       agentPriv,
		PinnedServerPub: serverPub,
		OnChannel:       func(*Channel) {},
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	srv := httptest.NewServer(l)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-websocket request, got %d", resp.StatusCode)
	}
}

func TestListenerSeenNonceReplayCache(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	_ = agentPub
	serverPub, _ := genKey(t)
	l, err := NewListener(ListenerConfig{
		AgentID:         "a",
		AgentPriv:       agentPriv,
		PinnedServerPub: serverPub,
		OnChannel:       func(*Channel) {},
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	if l.seenServerNonce("nonce-1") {
		t.Fatal("first sighting must be fresh")
	}
	if !l.seenServerNonce("nonce-1") {
		t.Fatal("second sighting of the same nonce must be reported as replay")
	}
}

func TestHandshakeThrottleBlocks(t *testing.T) {
	th := newHandshakeThrottle()
	ip := "1.2.3.4"
	for i := 0; i < th.max; i++ {
		if th.blocked(ip) {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		th.record(ip)
	}
	if !th.blocked(ip) {
		t.Fatal("throttle must block after max attempts")
	}
	if th.blocked("5.6.7.8") {
		t.Fatal("throttle must be per-IP")
	}
}

func TestDialWithBackoffAbortsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	attempts := 0
	_, err := DialWithBackoff(ctx, BackoffConfig{Initial: 20 * time.Millisecond, Max: 40 * time.Millisecond},
		func(context.Context) (*Channel, error) {
			attempts++
			return nil, errors.New("nope")
		})
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if attempts == 0 {
		t.Fatal("dial should have been attempted at least once")
	}
}

func TestDialWithBackoffSucceeds(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	const agentID = "backoff-agent"
	wsURL, _ := startAgentListener(t, agentID, agentPriv, serverPub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts := 0
	ch, err := DialWithBackoff(ctx, BackoffConfig{Initial: 10 * time.Millisecond},
		func(c context.Context) (*Channel, error) {
			attempts++
			if attempts < 2 {
				return nil, errors.New("transient")
			}
			return DialAgent(c, wsURL, serverPriv, agentPub, agentID)
		})
	if err != nil {
		t.Fatalf("DialWithBackoff: %v", err)
	}
	defer ch.Close()
	if attempts < 2 {
		t.Fatalf("expected a retry, got %d attempts", attempts)
	}
}
