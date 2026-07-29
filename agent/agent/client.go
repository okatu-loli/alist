package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AlistGo/alist-agent/proto"
)

// This file holds the agent-side client helpers for the two HTTP calls the
// design's bounded scope needs: pairing completion and the authenticated
// heartbeat. cmd/agent.go's `join`/`run` commands are thin wrappers around
// these — kept here so the request/response building and signing logic is
// unit-testable without spawning a process (see client_test.go).

// PairCompleteRequest is the wire body for POST /api/v4/agent/pair/complete.
// PubKey is the raw 32-byte ed25519 public key, base64 (std) encoded — the
// private key never appears anywhere in this struct or on the wire.
type PairCompleteRequest struct {
	Code     string `json:"code"`
	PubKey   string `json:"pubkey"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// PairCompleteResponse is the server's reply to a successful pairing
// completion. SAS is the server's OWN derivation (proto.DeriveSAS(code, pubkey))
// from exactly what it received — useful as a cross-check, but design
// W12-2b's actual MITM defense requires the CALLER to derive its own SAS
// locally (proto.DeriveSAS(code, identity.PubKey), no network round trip needed)
// and display THAT as the value to visually compare, never this field
// as-is: an on-path attacker able to substitute the pubkey in the request
// controls this response too and could simply relay it back unmodified,
// which would make a tampered pairing look consistent with itself. See
// cmd/agent.go's `agent join` and sas.go's top-of-file doc.
type PairCompleteResponse struct {
	AgentID       string   `json:"agent_id"`
	ServerVersion string   `json:"server_version,omitempty"`
	SAS           []string `json:"sas"`
	// ServerPubKey is the server's ed25519 public key (base64), delivered so
	// the agent can PIN it for a later reverse-connection wave's signature
	// verification (design R1). A public key — safe to carry on this
	// anonymous response. Empty if the server has no reverse identity wired.
	// This wave only pins it (persisted into ClientConfig); nothing verifies
	// against it yet (R2).
	ServerPubKey string `json:"server_pubkey,omitempty"`
}

// PairJoinParams bundles what `alist agent join` gathers locally (its own
// generated identity, the operator-supplied pairing code, and self-reported
// metadata) into one call to PairComplete.
type PairJoinParams struct {
	Code     string
	Identity *Identity
	Name     string
	Platform string
	Version  string
}

// PairComplete POSTs a pairing-completion request to host (a base URL like
// "http://localhost:5244", with or without a trailing slash) and returns
// the server-assigned AgentID on success. On a 4xx/expired/invalid-code
// rejection it returns a plain error describing the server's message.
func PairComplete(ctx context.Context, host string, p PairJoinParams) (*PairCompleteResponse, error) {
	if p.Identity == nil {
		return nil, errors.New("agent: identity is required")
	}
	req := PairCompleteRequest{
		Code:     p.Code,
		PubKey:   base64.StdEncoding.EncodeToString(p.Identity.PubKey),
		Name:     p.Name,
		Platform: p.Platform,
		Version:  p.Version,
	}
	var resp PairCompleteResponse
	if err := postJSON(ctx, host, "/api/v4/agent/pair/complete", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// HeartbeatRequest is the wire body for POST /api/v4/agent/heartbeat.
// Signature is base64(ed25519.Sign(privkey, proto.HeartbeatMessage(AgentID, Ts,
// Nonce))) — see that function's doc for exactly what is authenticated.
type HeartbeatRequest struct {
	AgentID   string `json:"agent_id"`
	Ts        int64  `json:"ts"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

// HeartbeatResponse is the server's reply to an accepted heartbeat. Task is
// nil (omitted entirely from the JSON) unless this agent has a task to work
// on right now — design W12-3a's dispatch step; see HeartbeatTask (task.go)
// and RunCopyTask, which is what cmd/agent.go's `agent run` loop calls when
// this is non-nil.
type HeartbeatResponse struct {
	OK      bool           `json:"ok"`
	Enabled bool           `json:"enabled"`
	Task    *HeartbeatTask `json:"task,omitempty"`
}

// heartbeatNonceLen matches the entropy budget of pairing codes' random
// source (randString, crypto/rand-backed): 16 chars from a 62-symbol
// alphabet is ~95 bits, far more than needed to make a nonce collision
// within the server's replay window practically impossible, while staying
// short on the wire.
const heartbeatNonceLen = 16

// Heartbeat builds and sends one authenticated liveness proof: a fresh
// timestamp + random nonce, signed with id's long-term private key over
// proto.HeartbeatMessage(agentID, ts, nonce), then POSTs it to host. The private
// key itself never leaves this function — only the signature does.
func Heartbeat(ctx context.Context, host string, id *Identity, agentID string) (*HeartbeatResponse, error) {
	if id == nil {
		return nil, errors.New("agent: identity is required")
	}
	if agentID == "" {
		return nil, errors.New("agent: agentID is required")
	}
	ts := time.Now().Unix()
	nonce := randString(heartbeatNonceLen)
	sig := ed25519.Sign(id.PrivKey, proto.HeartbeatMessage(agentID, ts, nonce))
	req := HeartbeatRequest{
		AgentID:   agentID,
		Ts:        ts,
		Nonce:     nonce,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
	var resp HeartbeatResponse
	if err := postJSON(ctx, host, "/api/v4/agent/heartbeat", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// envelope mirrors server/common.Resp[T] just enough to unwrap it: this
// codebase always answers HTTP 200 at the transport layer and signals
// success/failure through the envelope's own Code/Message (see
// server/common.ErrorResp) — a client must inspect Code, not the HTTP
// status, to know whether a call succeeded.
type envelope[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// RejectedError is what postJSON returns for a non-200 envelope Code — it
// preserves that code (an HTTP-status-shaped value, see
// server/common.ErrorResp) alongside the server's message, so a caller that
// needs to distinguish WHY a call was rejected (e.g. `agent run`
// telling a "waiting for admin confirmation" 403 apart from a genuine
// transport/server error) can do so with errors.As instead of matching on
// Message text, which errs.go deliberately keeps generic/uniform for
// several rejection reasons.
type RejectedError struct {
	Code    int
	Message string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("agent: server rejected request (code %d): %s", e.Code, e.Message)
}

// postJSON POSTs body as JSON to host+path, decodes the server's envelope,
// and reports a non-200 envelope Code as a *RejectedError carrying the
// server's own code and message (e.g. "invalid or expired pairing code").
func postJSON[T any](ctx context.Context, host, path string, body any, out *T) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("agent: encode request: %w", err)
	}
	url := strings.TrimRight(host, "/") + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("agent: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("agent: request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("agent: read response: %w", err)
	}
	var env envelope[T]
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("agent: decode response (http %d): %s", resp.StatusCode, string(respBody))
	}
	if env.Code != 200 {
		return &RejectedError{Code: env.Code, Message: env.Message}
	}
	*out = env.Data
	return nil
}
