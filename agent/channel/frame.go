// Package channel is the transport-agnostic, authenticated, persistent
// request/response+push message channel over a single WebSocket connection
// used by the remote-agent reverse-connection feature (design R2). It knows
// NOTHING about tasks/capabilities/storage — it is a generic authenticated
// pipe. A later wave (R3) registers the domain handlers (dispatchTask,
// heartbeat, putContent, ...) onto a live Channel; this package only proves
// the two ends are who they claim to be (see handshake.go) and then carries
// opaque JSON frames both directions.
//
// The channel is symmetric: once live, BOTH ends can Send requests and both
// receive them. Which end DIALED (the server, side "s") and which LISTENED
// (the agent, side "a") matters only for the handshake and for the ID prefix
// that keeps their request IDs from ever colliding.
package channel

import "encoding/json"

// FrameType tags a frame's role on the wire. Only "req"/"resp"/"push" carry
// logic in R2; "stream" is reserved for R3's binary streaming (put-content)
// and is defined here only so the tag space is fixed now — R2 processes no
// stream frames.
type FrameType string

const (
	// FrameReq is a request expecting exactly one matching FrameResp with the
	// same ID.
	FrameReq FrameType = "req"
	// FrameResp is the single reply to a FrameReq, correlated by ID.
	FrameResp FrameType = "resp"
	// FramePush is a fire-and-forget message: no ID correlation, no reply.
	FramePush FrameType = "push"
	// FrameStream is RESERVED for R3 binary streaming. Defined so the tag is
	// stable, but R2 registers no stream logic and treats an inbound stream
	// frame as an unsupported no-op rather than crashing.
	FrameStream FrameType = "stream"
)

// FrameError is the error half of a FrameResp: a peer's handler failed. It is
// a structured value (not just a string) so R3 can attach a stable Code
// without reparsing Message. Kept minimal in R2.
type FrameError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func (e *FrameError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// Frame is the JSON control envelope. As of R2.5 it is AEAD-sealed
// (ChaCha20-Poly1305) under the handshake-derived session key and sent as a
// BINARY WebSocket message (the plaintext JSON never touches the wire); see
// channel.go / crypto.go. ID correlates a FrameReq with its FrameResp; it is
// empty for FramePush.
// Method names the operation for FrameReq/FramePush and is empty on a
// FrameResp (the requester already knows the method from its in-flight map).
// Payload is the opaque per-method body; Error is set only on a failed
// FrameResp (and is mutually exclusive with a meaningful Payload).
type Frame struct {
	Type    FrameType       `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *FrameError     `json:"error,omitempty"`
}
