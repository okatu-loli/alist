package proto

import (
	"errors"
	"io"
	"time"

	"github.com/gorilla/websocket"
)

// This file holds the WebSocket transport adapter the token-authenticated
// first-contact pairing handshake (pairing_handshake.go) rides over. It was
// extracted out of internal/agent/pairing_acceptor.go in the W3 proto-split
// (design analysis §6 W3, §7 R1): both ends of the handshake need the exact
// same io.ReadWriter(+deadline) adapter over a *websocket.Conn — the AGENT
// acceptor (internal/agent/pairing_acceptor.go, still the agent-side listener)
// and the SERVER dialer (internal/agentsrv/pairing_dial.go) — and before this
// split the server side could only reach it through a throwaway exported
// wrapper (NewWsPairConn) bolted onto the agent-side file. Now both sides
// import this package directly and there is exactly one implementation.

// pairingDeadliner is the optional deadline slice of a conn (net.Conn
// satisfies it). The handshake (pairing_handshake.go) applies
// PairingStepTimeout when the conn supports it and silently skips deadlines
// otherwise (e.g. a bare pipe in a unit test) — the frame-size cap still
// bounds memory regardless. wsPairConn below always supports it.
type pairingDeadliner interface {
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// wsPairConn adapts a gorilla *websocket.Conn to the io.ReadWriter (+
// deadline) contract the TRANSPORT-AGNOSTIC pairing handshake
// (pairing_handshake.go) expects, so the reveal+tag length-prefixed JSON
// framing runs UNCHANGED over a WebSocket. Each Write is sent as one binary
// WS message; Read serves bytes from an internal buffer, refilling from the
// next binary message when empty (so an io.ReadFull that spans the header
// frame then the payload frame is satisfied across message boundaries). A
// per-message read limit mirrors maxPairingHandshakeBytes so an oversized WS
// frame is rejected by gorilla before it is buffered.
type wsPairConn struct {
	conn *websocket.Conn
	buf  []byte
}

// NewWSPairConn wraps conn for use as the pairing handshake's transport: pass
// the result to RunAgentPairingHandshake or RunServerPairingHandshake. It
// applies the handshake's own oversized-frame read limit at construction, so
// a peer that sends a too-large WS frame is rejected by gorilla itself before
// this adapter ever sees the bytes.
func NewWSPairConn(conn *websocket.Conn) io.ReadWriter {
	conn.SetReadLimit(maxPairingHandshakeBytes)
	return &wsPairConn{conn: conn}
}

func (c *wsPairConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			return 0, err
		}
		if mt != websocket.BinaryMessage {
			return 0, errors.New("agentpair: expected a binary websocket frame")
		}
		c.buf = data
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *wsPairConn) Write(p []byte) (int, error) {
	if err := c.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsPairConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *wsPairConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// Compile-time assertion that wsPairConn satisfies the handshake's needs.
var _ io.ReadWriter = (*wsPairConn)(nil)
var _ pairingDeadliner = (*wsPairConn)(nil)
