package channel

import (
	"context"
	"crypto/rand"
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

// testSessionKey returns a random 32-byte AEAD key. The frame-layer tests wrap a
// memConn pair in two channels that must share ONE key so each can open the
// other's sealed frames (in production the key comes from the handshake ECDH).
func testSessionKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, aeadKeyBytes)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	return k
}

// mustChannel builds a channel or fails the test.
func mustChannel(t *testing.T, conn wsConn, side string, key []byte) *Channel {
	t.Helper()
	ch, err := newChannel(conn, side, key)
	if err != nil {
		t.Fatalf("newChannel(%q): %v", side, err)
	}
	return ch
}

// memConn is an in-memory wsConn pair used by the frame-layer tests: it lets us
// drive Channel deterministically (req/resp correlation, timeout, malformed,
// oversized) without a real socket. The handshake and end-to-end tests use
// genuine loopback WebSockets instead (see handshake_test.go / dial_test.go).
type memConn struct {
	out       chan memMsg // messages this side writes
	in        chan memMsg // messages this side reads (peer's out)
	closed    chan struct{}
	closeOnce sync.Once
	limit     int64
	limitMu   sync.Mutex

	pongMu      sync.Mutex
	pongHandler func(appData string) error
}

type memMsg struct {
	mt   int
	data []byte
}

func newMemConnPair() (*memConn, *memConn) {
	aToB := make(chan memMsg, 8)
	bToA := make(chan memMsg, 8)
	a := &memConn{out: aToB, in: bToA, closed: make(chan struct{})}
	b := &memConn{out: bToA, in: aToB, closed: make(chan struct{})}
	return a, b
}

// ReadMessage mimics gorilla's real behavior of absorbing control frames
// (PING/PONG) inside the read loop rather than ever handing them to the
// caller as a message: a PING is auto-answered with a PONG (exactly what the
// real websocket transport does when something is pumping ReadMessage), and a
// PONG fires the registered pong handler. Only a genuine data frame is
// returned. This lets the channel-stability heartbeat tests exercise the real
// pingLoop/SetPongHandler wiring in channel.go against this in-memory fake.
func (m *memConn) ReadMessage() (int, []byte, error) {
	for {
		select {
		case <-m.closed:
			return 0, nil, errors.New("memconn: closed")
		case msg, ok := <-m.in:
			if !ok {
				return 0, nil, errors.New("memconn: peer closed")
			}
			switch msg.mt {
			case websocket.PingMessage:
				_ = m.WriteControl(websocket.PongMessage, msg.data, time.Time{})
				continue
			case websocket.PongMessage:
				m.pongMu.Lock()
				h := m.pongHandler
				m.pongMu.Unlock()
				if h != nil {
					_ = h(string(msg.data))
				}
				continue
			}
			m.limitMu.Lock()
			lim := m.limit
			m.limitMu.Unlock()
			if lim > 0 && int64(len(msg.data)) > lim {
				return 0, nil, errors.New("memconn: message exceeds read limit")
			}
			return msg.mt, msg.data, nil
		}
	}
}

func (m *memConn) WriteMessage(mt int, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case <-m.closed:
		return errors.New("memconn: closed")
	case m.out <- memMsg{mt: mt, data: cp}:
		return nil
	case <-time.After(time.Second):
		return errors.New("memconn: write blocked")
	}
}

// WriteControl mirrors WriteMessage for control frames (PING/PONG): the fake
// has no separate control-write path, but production gorilla documents
// WriteControl as safe to call concurrently with WriteMessage, which is all
// the channel-layer callers (pingLoop, and this fixture's own auto-PONG) rely
// on.
func (m *memConn) WriteControl(mt int, data []byte, _ time.Time) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case <-m.closed:
		return errors.New("memconn: closed")
	case m.out <- memMsg{mt: mt, data: cp}:
		return nil
	case <-time.After(time.Second):
		return errors.New("memconn: write blocked")
	}
}

func (m *memConn) SetReadDeadline(time.Time) error  { return nil }
func (m *memConn) SetWriteDeadline(time.Time) error { return nil }
func (m *memConn) SetReadLimit(l int64) {
	m.limitMu.Lock()
	m.limit = l
	m.limitMu.Unlock()
}
func (m *memConn) SetPongHandler(h func(appData string) error) {
	m.pongMu.Lock()
	m.pongHandler = h
	m.pongMu.Unlock()
}
func (m *memConn) Close() error {
	m.closeOnce.Do(func() { close(m.closed) })
	return nil
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// livePair wraps a memConn pair in two started channels: server side "s",
// agent side "a".
func livePair(t *testing.T) (server, agent *Channel) {
	t.Helper()
	ca, cb := newMemConnPair()
	key := testSessionKey(t)
	server = mustChannel(t, ca, "s", key)
	agent = mustChannel(t, cb, "a", key)
	server.Start()
	agent.Start()
	t.Cleanup(func() { server.Close(); agent.Close() })
	return server, agent
}

func TestBidirectionalRequestResponse(t *testing.T) {
	server, agent := livePair(t)

	// agent handles a request from the server
	agent.Handle("ping", func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		return mustJSON(t, map[string]string{"pong": string(p)}), nil
	})
	// server handles a request from the agent (proves symmetry)
	server.Handle("whoami", func(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return mustJSON(t, "server"), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := server.Send(ctx, "ping", mustJSON(t, "hi"))
	if err != nil {
		t.Fatalf("server->agent Send: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if got["pong"] != `"hi"` {
		t.Fatalf("unexpected resp: %v", got)
	}

	resp2, err := agent.Send(ctx, "whoami", nil)
	if err != nil {
		t.Fatalf("agent->server Send: %v", err)
	}
	if string(resp2) != `"server"` {
		t.Fatalf("unexpected resp2: %s", resp2)
	}
}

func TestRequestErrorPropagates(t *testing.T) {
	server, agent := livePair(t)
	agent.Handle("boom", func(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("kaboom")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := server.Send(ctx, "boom", nil)
	if err == nil {
		t.Fatal("expected error from handler")
	}
	var fe *FrameError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FrameError, got %T: %v", err, err)
	}
	if fe.Message != "kaboom" {
		t.Fatalf("unexpected error message: %q", fe.Message)
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	server, _ := livePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := server.Send(ctx, "nope", nil)
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	var fe *FrameError
	if !errors.As(err, &fe) || fe.Code != "unknown_method" {
		t.Fatalf("expected unknown_method FrameError, got %v", err)
	}
}

func TestSendTimeoutOnNoResponse(t *testing.T) {
	server, agent := livePair(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	agent.Handle("slow", func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := server.Send(ctx, "slow", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Send took too long, timeout not honored")
	}
}

func TestPushDeliveredToHandler(t *testing.T) {
	server, agent := livePair(t)
	got := make(chan string, 1)
	agent.Handle("note", func(_ context.Context, _ string, p json.RawMessage) (json.RawMessage, error) {
		got <- string(p)
		return nil, nil
	})
	if err := server.Push("note", mustJSON(t, "hello")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	select {
	case v := <-got:
		if v != `"hello"` {
			t.Fatalf("unexpected push payload: %s", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("push not delivered")
	}
}

func TestIDsDisjointAcrossSides(t *testing.T) {
	ca, cb := newMemConnPair()
	key := testSessionKey(t)
	s := mustChannel(t, ca, "s", key)
	a := mustChannel(t, cb, "a", key)
	// Server IDs are s-prefixed, agent IDs a-prefixed — provably disjoint even
	// though both counters start at 1.
	if s.nextID() != "s-1" || s.nextID() != "s-2" {
		t.Fatal("server ID scheme wrong")
	}
	if a.nextID() != "a-1" || a.nextID() != "a-2" {
		t.Fatal("agent ID scheme wrong")
	}
}

func TestMalformedFrameTearsDownNotPanic(t *testing.T) {
	ca, cb := newMemConnPair()
	key := testSessionKey(t)
	ch := mustChannel(t, ca, "s", key)
	ch.Start()
	defer ch.Close()
	// Peer seals garbage that is not valid JSON with the CORRECT key: the AEAD
	// opens fine (it authenticates), but the decrypted bytes aren't a Frame, so
	// the malformed-frame path fires and tears the channel down.
	peerSealer, err := newFrameSealer(key, "a")
	if err != nil {
		t.Fatalf("newFrameSealer: %v", err)
	}
	sealed, err := peerSealer.seal([]byte("{not json"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := cb.WriteMessage(websocket.BinaryMessage, sealed); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	select {
	case <-ch.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not tear down on malformed frame")
	}
	if ch.Err() == nil {
		t.Fatal("expected teardown error")
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	ca, cb := newMemConnPair()
	key := testSessionKey(t)
	ch := mustChannel(t, ca, "s", key)
	ch.Start()
	defer ch.Close()
	// One byte over the frame cap the read loop installs.
	big := make([]byte, maxFrameBytes+1)
	if err := cb.WriteMessage(websocket.BinaryMessage, big); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	select {
	case <-ch.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("channel did not tear down on oversized frame")
	}
	if ch.Err() == nil {
		t.Fatal("expected teardown error on oversized frame")
	}
}

// After R2.5 the whole frame stream is AEAD-sealed BINARY. A TEXT message on the
// live channel is a protocol violation on the sealed stream and must tear the
// channel down (fail closed) rather than being processed as an unsealed frame.
func TestUnsealedTextFrameTearsDown(t *testing.T) {
	ca, cb := newMemConnPair()
	key := testSessionKey(t)
	ch := mustChannel(t, ca, "s", key)
	ch.Start()
	defer ch.Close()
	if err := cb.WriteMessage(websocket.TextMessage, []byte(`{"type":"push","method":"x"}`)); err != nil {
		t.Fatalf("peer write text: %v", err)
	}
	select {
	case <-ch.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not tear down on an unsealed text frame")
	}
	if ch.Err() == nil {
		t.Fatal("expected teardown error on an unsealed text frame")
	}
}

func TestSendAfterCloseFails(t *testing.T) {
	server, _ := livePair(t)
	server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := server.Send(ctx, "ping", nil); err == nil {
		t.Fatal("expected Send to fail after Close")
	}
}

// spyConn wraps a memConn to record WriteControl calls, so a test can observe
// that the heartbeat ping loop actually fires without needing a peer to
// interpret the frame.
type spyConn struct {
	*memConn
	mu    sync.Mutex
	pings int
}

func (s *spyConn) WriteControl(mt int, data []byte, deadline time.Time) error {
	if mt == websocket.PingMessage {
		s.mu.Lock()
		s.pings++
		s.mu.Unlock()
	}
	return s.memConn.WriteControl(mt, data, deadline)
}

func (s *spyConn) pingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// TestPingLoopWritesPeriodicPings pins Start's pingLoop: with a short
// pingPeriod injected, WriteControl(PING, ...) is actually invoked — the
// mechanism channel-stability wave 1 relies on to keep a NAT mapping alive
// and to give the peer's read deadline something to reset against.
func TestPingLoopWritesPeriodicPings(t *testing.T) {
	ca, cb := newMemConnPair()
	spy := &spyConn{memConn: ca}
	key := testSessionKey(t)
	ch := mustChannel(t, spy, "s", key)
	ch.pingPeriod = 15 * time.Millisecond
	ch.Start()
	defer ch.Close()

	// Drain cb so the spy's WriteControl (which sends into the shared
	// buffered channel) never blocks waiting for a reader.
	go func() {
		for {
			if _, _, err := cb.ReadMessage(); err != nil {
				return
			}
		}
	}()

	deadline := time.After(2 * time.Second)
	for spy.pingCount() < 1 {
		select {
		case <-deadline:
			t.Fatal("no ping was sent within the deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// failPingConn wraps memConn to make every PING WriteControl fail while
// leaving data WriteMessage (and any non-ping WriteControl) untouched, so a
// test can isolate "just the ping write hiccups" from "the socket is dead".
type failPingConn struct {
	*memConn
}

func (f *failPingConn) WriteControl(mt int, data []byte, deadline time.Time) error {
	if mt == websocket.PingMessage {
		return errors.New("failpingconn: ping write always fails")
	}
	return f.memConn.WriteControl(mt, data, deadline)
}

// TestPingWriteFailureDoesNotTearDownChannel pins the post-wave-1 review fix:
// a failed PING write is NOT authoritative that the peer is dead (it only
// proves this one control-frame write didn't land — e.g. lock contention with
// a large in-flight data write on a slow uplink, or a transient hiccup) and
// must NOT tear the channel down. Only the read side's pongWait deadline gets
// to declare a peer dead. A channel whose ping writes fail every single cycle
// must stay live (and fully usable for real data traffic) throughout.
func TestPingWriteFailureDoesNotTearDownChannel(t *testing.T) {
	ca, cb := newMemConnPair()
	failing := &failPingConn{memConn: ca}
	key := testSessionKey(t)
	ch := mustChannel(t, failing, "s", key)
	ch.pingPeriod = 15 * time.Millisecond
	ch.Start()
	defer ch.Close()

	// Drain cb (a raw, unwrapped memConn) so writeFrame's real data traffic
	// below has somewhere to land.
	go func() {
		for {
			if _, _, err := cb.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Give the ping loop several chances to fail. A channel that (incorrectly)
	// treated a ping write failure as fatal would have torn itself down well
	// before this elapses.
	time.Sleep(150 * time.Millisecond)
	if ch.Err() != nil {
		t.Fatalf("channel was torn down by a mere ping write failure: %v", ch.Err())
	}

	// The channel must still be fully usable for real (non-ping) traffic —
	// writeFrame's own path is untouched by failPingConn.
	if err := ch.Push("x", mustJSON(t, "hi")); err != nil {
		t.Fatalf("channel unusable for real traffic after ping write failures: %v", err)
	}
	if ch.Err() != nil {
		t.Fatalf("channel torn down after Push despite ping-only failures: %v", ch.Err())
	}
}

// failWriteConn wraps memConn to make every WriteMessage fail, simulating a
// broken/dead socket at write time.
type failWriteConn struct {
	*memConn
}

func (f *failWriteConn) WriteMessage(int, []byte) error {
	return errors.New("failwriteconn: write always fails")
}

// TestWriteFailureTearsDownChannel pins the writeFrame fix: a failed
// WriteMessage must tear the channel down (Done fires with a non-nil Err())
// rather than silently returning an error while everything else on the
// channel keeps believing it is still live.
func TestWriteFailureTearsDownChannel(t *testing.T) {
	ca, _ := newMemConnPair()
	key := testSessionKey(t)
	failing := &failWriteConn{memConn: ca}
	ch := mustChannel(t, failing, "s", key)
	ch.Start()
	defer ch.Close()

	if err := ch.Push("x", nil); err == nil {
		t.Fatal("expected Push to fail when the underlying write fails")
	}

	select {
	case <-ch.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not tear down after a write failure")
	}
	if ch.Err() == nil {
		t.Fatal("expected a teardown error after a write failure")
	}
}

// realWSConnPair returns a genuine loopback pair of *websocket.Conn (no
// application-level crypto handshake — the frame-layer tests share a raw key
// the same way livePair does over memConn). Real gorilla ping/pong and
// read-deadline semantics are not reproducible over the in-memory fake, so
// the heartbeat keepalive/dead-peer-detection tests need actual sockets.
func realWSConnPair(t *testing.T) (client, server *websocket.Conn) {
	t.Helper()
	upgraded := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upgraded <- conn
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case server = <-upgraded:
	case <-time.After(2 * time.Second):
		t.Fatal("server side never upgraded")
	}
	return c, server
}

// TestKeepaliveSurvivesIdlePeriod is the real-gorilla-loopback integration
// test for the NAT-idle-drop fix (channel-stability wave 1): two live
// channels, both with a short ping period/pong wait, sit completely idle
// (no Send/Push either direction) well past several heartbeat cycles and
// must both remain live throughout — the ping/pong keepalive, not silence,
// is what keeps them up.
func TestKeepaliveSurvivesIdlePeriod(t *testing.T) {
	clientConn, serverConn := realWSConnPair(t)
	key := testSessionKey(t)

	client := mustChannel(t, clientConn, "s", key)
	client.pingPeriod = 30 * time.Millisecond
	client.pongWait = 120 * time.Millisecond
	client.Start()
	defer client.Close()

	server := mustChannel(t, serverConn, "a", key)
	server.pingPeriod = 30 * time.Millisecond
	server.pongWait = 120 * time.Millisecond
	server.Start()
	defer server.Close()

	// Idle for several multiples of pongWait with zero application traffic.
	time.Sleep(500 * time.Millisecond)

	if client.Err() != nil {
		t.Fatalf("client channel died during an idle period despite keepalive: %v", client.Err())
	}
	if server.Err() != nil {
		t.Fatalf("server channel died during an idle period despite keepalive: %v", server.Err())
	}
}

// TestSilentPeerDetectedWithinPongWait is the real-gorilla-loopback
// integration test for the dead-peer-detection half of the fix: the peer
// never starts a Channel — nobody ever calls ReadMessage on its side of the
// socket, so gorilla never auto-answers the client's pings with pongs. This
// is exactly the NAT/middlebox black-hole failure mode the wave 1 report
// diagnosed: bytes go out, nothing ever comes back. The client's own read
// deadline must expire and tear the channel down within pongWait instead of
// hanging on the dead socket forever.
func TestSilentPeerDetectedWithinPongWait(t *testing.T) {
	clientConn, serverConn := realWSConnPair(t)
	key := testSessionKey(t)

	client := mustChannel(t, clientConn, "s", key)
	client.pingPeriod = 30 * time.Millisecond
	client.pongWait = 150 * time.Millisecond
	client.Start()
	defer client.Close()
	defer serverConn.Close()

	start := time.Now()
	select {
	case <-client.Done():
		if client.Err() == nil {
			t.Fatal("expected a teardown error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not detect the silent peer within pongWait")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dead-peer detection took too long: %v (want within ~pongWait)", elapsed)
	}
}
