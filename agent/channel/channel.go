package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

// wsConn is the minimal slice of *websocket.Conn this package uses. Kept as an
// interface so the frame/handshake logic is exercisable without a real socket,
// though the tests drive it over genuine loopback WebSockets.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	// WriteControl writes a control frame (PING/PONG/CLOSE). gorilla documents
	// this as safe to call concurrently with WriteMessage/other control writes
	// (it does not share the data-message write path), so the ping loop calls
	// it directly rather than taking writeMu — control frames never go through
	// the AEAD sealer either, so there is no nonce-ordering concern.
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	SetReadLimit(limit int64)
	// SetPongHandler installs the callback gorilla invokes (from inside
	// ReadMessage) when a PONG control frame arrives. The channel uses it to
	// refresh the read deadline, so a peer that is merely idle — but still
	// answering pings — is never mistaken for a dead one.
	SetPongHandler(h func(appData string) error)
	Close() error
}

// maxFrameBytes bounds a single inbound frame once the channel is live. Control
// frames are small; R3's streaming will use its own chunk sizing on binary
// frames. An oversized frame trips the read limit and tears the channel down
// rather than being buffered.
const maxFrameBytes = 8 << 20 // 8 MiB

// defaultSendTimeout bounds a Send whose caller passed a context without its
// own deadline, so an in-flight request can never wait forever for a resp that
// never comes.
const defaultSendTimeout = 30 * time.Second

// Heartbeat tuning (channel-stability wave 1). A reverse channel can sit
// completely idle between tasks — no requests, no pushes — for well past the
// ~10 minute idle window a lot of home-router/NAT tables use to evict a
// mapping. With zero bytes ever flowing, neither side notices until the next
// Send, which then hangs against a socket the network has already silently
// dropped (a bare WebSocket close, code 1006). A periodic PING with a bounded
// read deadline fixes both problems: it keeps bytes flowing (refreshing the
// NAT mapping) and gives the reader a hard bound after which a genuinely dead
// peer is detected and torn down instead of hung on forever.
//
// defaultPongWait is set well above 2x defaultPingPeriod so two consecutive
// dropped pings (e.g. a slow write under load) don't false-positive a live
// peer as dead.
//
// review fix (post-wave-1): the PING write's own deadline is bounded by the
// ping period itself, NOT a short fixed timeout — see pingLoop. gorilla's
// WriteControl shares the SAME underlying write lock as a data WriteMessage
// (conn.go: both block on c.mu), so a single legitimate large frame — e.g.
// PutContent streaming ~1.4MB chunks over a slow (~1Mbps-class) home-
// broadband agent uplink — can hold that lock for several seconds. A short
// fixed ping-write timeout (the original 10s) would then time out under
// perfectly healthy, if slow, traffic and closeWith a live channel: exactly
// the false-positive teardown this whole wave exists to eliminate.
const (
	defaultPingPeriod = 30 * time.Second
	defaultPongWait   = 75 * time.Second
)

// Handler processes an inbound FrameReq or FramePush. For a req its return
// value becomes the FrameResp payload (or, on error, the FrameError); for a
// push the return values are ignored. R3 registers the domain handlers; R2
// ships none.
type Handler func(ctx context.Context, method string, payload json.RawMessage) (json.RawMessage, error)

// Channel is a live, authenticated, bidirectional request/response+push pipe
// over one WebSocket connection. It is symmetric: both ends may Send and both
// receive requests. It knows nothing about the domain (tasks/caps) — callers
// register Handlers for the methods they support.
type Channel struct {
	conn wsConn
	side string // "s" (server/dialer) or "a" (agent/listener) — the ID prefix

	// sealer AEAD-seals every outbound frame and opens every inbound one under
	// the handshake-derived session key (design R2.5). Frames go on the wire as
	// BINARY WebSocket messages carrying only the ciphertext; the per-direction
	// nonce counter is implicit (see crypto.go).
	sealer *frameSealer

	writeMu sync.Mutex // serializes concurrent writes (gorilla forbids parallel writes)

	// pingPeriod/pongWait tune the heartbeat (see the const block above for the
	// production defaults); newChannel seeds them, tests may lower both before
	// calling Start to exercise the keepalive/detection timing quickly.
	pingPeriod time.Duration
	pongWait   time.Duration

	idCounter atomic.Uint64

	inflightMu sync.Mutex
	inflight   map[string]chan *Frame

	handlersMu sync.RWMutex
	handlers   map[string]Handler

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

// newChannel wraps an already-handshaked conn with the session key derived by
// the handshake. side must be "s" or "a" — it prefixes every locally-generated
// request ID so a server-initiated ID and an agent-initiated ID can never
// collide even though both sides count from the same starting point, and it
// selects the AEAD send/recv direction (see newFrameSealer). It fails if the
// session key is malformed. The receive loop is started by the caller via Start.
func newChannel(conn wsConn, side string, sessionKey []byte) (*Channel, error) {
	sealer, err := newFrameSealer(sessionKey, side)
	if err != nil {
		return nil, err
	}
	return &Channel{
		conn:       conn,
		side:       side,
		sealer:     sealer,
		pingPeriod: defaultPingPeriod,
		pongWait:   defaultPongWait,
		inflight:   make(map[string]chan *Frame),
		handlers:   make(map[string]Handler),
		closed:     make(chan struct{}),
	}, nil
}

// nextID returns a per-channel-unique frame ID: the side prefix ("s"/"a") plus
// a monotonic counter. No time or math/rand source is used, so IDs are
// deterministic within a side and — because of the distinct prefix — provably
// disjoint from the peer's IDs.
func (c *Channel) nextID() string {
	return c.side + "-" + fmt.Sprintf("%d", c.idCounter.Add(1))
}

// Handle registers h for method. Not safe to call concurrently with itself for
// the same method, but safe against a running receive loop.
func (c *Channel) Handle(method string, h Handler) {
	c.handlersMu.Lock()
	c.handlers[method] = h
	c.handlersMu.Unlock()
}

// Start launches the receive loop and the heartbeat ping loop. Call exactly
// once, after handshake. The pong handler is installed here, synchronously,
// before either goroutine starts, so there is no window where a PONG could
// arrive and find no handler registered.
func (c *Channel) Start() {
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(c.pongWait))
	})
	go c.readLoop()
	go c.pingLoop()
}

// pingLoop sends a PING control frame every pingPeriod until the channel
// closes. Both ends of a Channel run this (the type is symmetric), so either
// side's silence is caught by the other's read deadline regardless of which
// end initiated the connection.
//
// Ping only feeds liveness; it never judges death — the read side (readLoop's
// pongWait deadline) is the SOLE authority for declaring a peer dead. Two
// deliberate choices follow from that division of labor:
//
//  1. The write deadline passed to WriteControl is bounded by pingPeriod
//     itself, not a short fixed timeout. gorilla's WriteControl blocks on the
//     SAME underlying write lock a data WriteMessage holds (conn.go: both
//     take c.mu), so a big legitimate frame in flight on a slow uplink can
//     hold that lock for seconds. Worst case here is simply "this ping's
//     write queues behind the data write and lands roughly on schedule for
//     the next tick" — never a spurious timeout on a channel that is busy,
//     not dead.
//  2. A failed ping write does NOT tear the channel down. It only means this
//     one write didn't land (lock contention as above, a transient hiccup,
//     etc.) — not that the peer is gone. If the peer really is gone, no data
//     and no PONG will ever arrive and readLoop's pongWait deadline tears the
//     channel down on schedule. Closing here on a mere write hiccup would be
//     exactly the false-positive teardown of a healthy-but-slow channel that
//     this whole wave exists to eliminate.
func (c *Channel) pingLoop() {
	ticker := time.NewTicker(c.pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.pingPeriod)); err != nil {
				log.Debugf("agentchan: ping write failed (non-fatal; the read deadline is the authority on peer death): %v", err)
			}
		}
	}
}

// Done is closed when the channel is torn down (peer close, read error, or
// Close). Err (after Done fires) reports why.
func (c *Channel) Done() <-chan struct{} { return c.closed }

// Err returns the teardown cause, or nil if still live.
func (c *Channel) Err() error {
	select {
	case <-c.closed:
		return c.closeErr
	default:
		return nil
	}
}

// Send issues a request and blocks for the matching response. It fails if ctx
// is cancelled, the channel closes, or (if ctx has no deadline) the default
// timeout elapses. A FrameError in the response is returned as *FrameError.
func (c *Channel) Send(ctx context.Context, method string, payload json.RawMessage) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultSendTimeout)
		defer cancel()
	}

	id := c.nextID()
	respCh := make(chan *Frame, 1)

	c.inflightMu.Lock()
	// Closed check under the lock so a Send racing Close either registers and
	// is woken by closeInflight, or sees the channel already closed here.
	select {
	case <-c.closed:
		c.inflightMu.Unlock()
		return nil, c.teardownErr()
	default:
	}
	c.inflight[id] = respCh
	c.inflightMu.Unlock()

	defer func() {
		c.inflightMu.Lock()
		delete(c.inflight, id)
		c.inflightMu.Unlock()
	}()

	if err := c.writeFrame(&Frame{Type: FrameReq, ID: id, Method: method, Payload: payload}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, c.teardownErr()
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Payload, nil
	}
}

// Push sends a fire-and-forget message. It returns only a write/transport
// error — there is no response to await.
func (c *Channel) Push(method string, payload json.RawMessage) error {
	return c.writeFrame(&Frame{Type: FramePush, Method: method, Payload: payload})
}

// Close tears the channel down: it closes the socket and wakes every in-flight
// Send. Idempotent.
func (c *Channel) Close() error {
	return c.closeWith(errors.New("agentchan: channel closed"))
}

func (c *Channel) closeWith(cause error) error {
	c.closeOnce.Do(func() {
		c.closeErr = cause
		close(c.closed)
		_ = c.conn.Close()
		c.closeInflight()
	})
	return nil
}

func (c *Channel) teardownErr() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	return errors.New("agentchan: channel closed")
}

// closeInflight drains the in-flight map so no Send blocks past teardown. The
// select in Send on c.closed already unblocks waiters; this clears the map.
func (c *Channel) closeInflight() {
	c.inflightMu.Lock()
	c.inflight = make(map[string]chan *Frame)
	c.inflightMu.Unlock()
}

func (c *Channel) writeFrame(f *Frame) error {
	select {
	case <-c.closed:
		return c.teardownErr()
	default:
	}
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("agentchan: encode frame: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// Seal UNDER writeMu so the per-direction nonce counter advances in the exact
	// same order frames hit the wire — otherwise two concurrent writers could
	// seal in one order and transmit in another, desynchronizing the receiver's
	// implicit counter. (gorilla also forbids concurrent writes.)
	sealed, err := c.sealer.seal(b)
	if err != nil {
		// Nonce exhaustion (practically unreachable): tear down rather than ever
		// risk reusing a nonce.
		c.closeWith(err)
		return err
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(handshakeStepTimeout)); err != nil {
		return err
	}
	if err := c.conn.WriteMessage(websocket.BinaryMessage, sealed); err != nil {
		werr := fmt.Errorf("agentchan: write frame: %w", err)
		// A failed write means the socket is broken (or the peer vanished): the
		// channel is dead either way, so tear it down instead of returning an
		// error while leaving the caller (and everything else on this channel)
		// believing it might still be usable.
		c.closeWith(werr)
		return werr
	}
	return nil
}

// readLoop is the single reader. Every post-handshake frame arrives as an
// AEAD-sealed BINARY message; it is opened under the session key before being
// dispatched. It routes resp frames to waiting Sends and req/push frames to
// registered handlers. A failed AEAD open (tamper / injection / reorder /
// replay), a non-binary message, a malformed decrypted frame, or a transport
// error all tear the channel down (returns, never panics) — no frame that did
// not authenticate is ever processed.
func (c *Channel) readLoop() {
	c.conn.SetReadLimit(maxFrameBytes)
	for {
		// The read deadline is refreshed every iteration to pongWait: an
		// arriving data frame renews it here on the next loop just like before,
		// and Start's SetPongHandler renews it the instant a PONG lands even
		// mid-read (gorilla processes control frames inside ReadMessage without
		// returning them). Either way, a peer that has gone truly silent — no
		// data, no PONG, the channel-stability wave 1 NAT-idle-drop scenario —
		// has its dead socket detected and torn down within pongWait instead of
		// hanging forever. A live Send still separately bounds itself via ctx.
		if err := c.conn.SetReadDeadline(time.Now().Add(c.pongWait)); err != nil {
			c.closeWith(fmt.Errorf("agentchan: set read deadline: %w", err))
			return
		}
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			c.closeWith(fmt.Errorf("agentchan: read: %w", err))
			return
		}
		if mt != websocket.BinaryMessage {
			// Post-handshake every frame is AEAD-sealed and sent BINARY. A TEXT
			// (or any non-binary) message on the sealed stream is a protocol
			// violation — fail closed rather than process an unsealed frame.
			c.closeWith(errors.New("agentchan: unexpected non-binary message on sealed channel"))
			return
		}
		plain, err := c.sealer.open(data)
		if err != nil {
			// Bad AEAD tag = tamper / injection / reorder / replay (or nonce
			// exhaustion). Never process a frame that did not authenticate: tear
			// the channel down immediately.
			c.closeWith(err)
			return
		}
		var f Frame
		if err := json.Unmarshal(plain, &f); err != nil {
			// Malformed control frame is a protocol violation: tear down with
			// an error instead of panicking or silently looping.
			c.closeWith(fmt.Errorf("agentchan: malformed frame: %w", err))
			return
		}
		c.dispatch(&f)
	}
}

func (c *Channel) dispatch(f *Frame) {
	switch f.Type {
	case FrameResp:
		c.inflightMu.Lock()
		ch := c.inflight[f.ID]
		c.inflightMu.Unlock()
		if ch != nil {
			// buffered(1); non-blocking so a duplicate/late resp can't wedge
			// the read loop.
			select {
			case ch <- f:
			default:
			}
		}
	case FrameReq:
		go c.serveRequest(f)
	case FramePush:
		go c.servePush(f)
	case FrameStream:
		// Reserved for R3. No-op in R2.
	default:
		// Unknown type: ignore (forward-compat), do not tear down.
	}
}

func (c *Channel) lookupHandler(method string) Handler {
	c.handlersMu.RLock()
	h := c.handlers[method]
	c.handlersMu.RUnlock()
	return h
}

func (c *Channel) serveRequest(f *Frame) {
	h := c.lookupHandler(f.Method)
	if h == nil {
		_ = c.writeFrame(&Frame{Type: FrameResp, ID: f.ID, Error: &FrameError{
			Code:    "unknown_method",
			Message: "no handler registered for method " + f.Method,
		}})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Tie the handler's context to channel teardown.
	go func() {
		select {
		case <-c.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	out, err := h(ctx, f.Method, f.Payload)
	if err != nil {
		_ = c.writeFrame(&Frame{Type: FrameResp, ID: f.ID, Error: &FrameError{Message: err.Error()}})
		return
	}
	_ = c.writeFrame(&Frame{Type: FrameResp, ID: f.ID, Payload: out})
}

func (c *Channel) servePush(f *Frame) {
	h := c.lookupHandler(f.Method)
	if h == nil {
		return // pushes to unknown methods are silently dropped
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = h(ctx, f.Method, f.Payload)
}
