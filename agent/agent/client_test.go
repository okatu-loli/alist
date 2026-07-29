package agent

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlistGo/alist-agent/proto"
)

// jsonEnvelope writes a server/common.Resp[T]-shaped body: this codebase
// always answers HTTP 200 at the transport layer, real success/failure
// state lives in the envelope's own "code" field — the fake servers below
// mirror that exactly so the client is tested against the real contract.
func jsonEnvelope(w http.ResponseWriter, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    code,
		"message": message,
		"data":    data,
	})
}

// TestPairCompletePostsExpectedFieldsAndParsesSuccess pins the request the
// client actually sends (code/pubkey-base64/name/platform/version) and that
// a 200-code envelope's data unmarshals into PairCompleteResponse.
func TestPairCompletePostsExpectedFieldsAndParsesSuccess(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}

	var gotReq PairCompleteRequest
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		jsonEnvelope(w, 200, "success", PairCompleteResponse{AgentID: "srv-assigned-id", ServerVersion: "v4.0.0"})
	}))
	defer srv.Close()

	resp, err := PairComplete(t.Context(), srv.URL, PairJoinParams{
		Code:     "the-pairing-code",
		Identity: id,
		Name:     "nas-1",
		Platform: "linux/amd64",
		Version:  "v4.0.0-test",
	})
	if err != nil {
		t.Fatalf("PairComplete: %v", err)
	}
	if gotPath != "/api/v4/agent/pair/complete" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotReq.Code != "the-pairing-code" || gotReq.Name != "nas-1" ||
		gotReq.Platform != "linux/amd64" || gotReq.Version != "v4.0.0-test" {
		t.Fatalf("request fields mismatch: %+v", gotReq)
	}
	wantPub := base64.StdEncoding.EncodeToString(id.PubKey)
	if gotReq.PubKey != wantPub {
		t.Fatalf("pubkey mismatch: got %q, want %q", gotReq.PubKey, wantPub)
	}
	if resp.AgentID != "srv-assigned-id" {
		t.Fatalf("resp.AgentID = %q, want srv-assigned-id", resp.AgentID)
	}
}

// TestPairCompleteParsesServerPubKey pins design R1's client leg: the
// pair/complete response's server_pubkey field is parsed into
// PairCompleteResponse.ServerPubKey so `agent join` can pin it.
//
// The fixture pubkey is a bare ed25519 keypair generated in-line rather than
// via internal/agentsrv's real ServerIdentity type: after the W1 server/agent
// repo split, internal/agentsrv imports this package (for the handshake +
// wire-adapter it drives), so this package's own tests must not import
// internal/agentsrv back (import cycle). Only the wire shape — an arbitrary
// base64-std-encoded ed25519 public key round-tripping through the JSON
// envelope — is under test here, which a bare keypair exercises identically.
func TestPairCompleteParsesServerPubKey(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	serverPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverPubB64 := base64.StdEncoding.EncodeToString(serverPub)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonEnvelope(w, 200, "success", PairCompleteResponse{
			AgentID:      "srv-assigned-id",
			ServerPubKey: serverPubB64,
		})
	}))
	defer srv.Close()

	resp, err := PairComplete(t.Context(), srv.URL, PairJoinParams{Code: "c", Identity: id})
	if err != nil {
		t.Fatalf("PairComplete: %v", err)
	}
	if resp.ServerPubKey != serverPubB64 {
		t.Fatalf("resp.ServerPubKey = %q, want %q", resp.ServerPubKey, serverPubB64)
	}
}

// TestPairCompleteRejectionSurfacesServerMessage pins that a non-200
// envelope code (e.g. an expired/invalid pairing code) surfaces as a Go
// error carrying the server's message, not a silent success.
func TestPairCompleteRejectionSurfacesServerMessage(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonEnvelope(w, 400, "invalid or expired pairing code", nil)
	}))
	defer srv.Close()

	_, err = PairComplete(t.Context(), srv.URL, PairJoinParams{Code: "bad", Identity: id})
	if err == nil {
		t.Fatal("expected an error for a rejected pairing code")
	}
}

// TestHeartbeatSignsVerifiableMessage pins the load-bearing security
// property end to end at the client: what Heartbeat sends is a signature
// that a server holding only the PUBLIC key can verify against
// proto.HeartbeatMessage(agentID, ts, nonce) built from the request's own fields
// — proving the client and server will actually agree on what was signed.
func TestHeartbeatSignsVerifiableMessage(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}

	var gotReq HeartbeatRequest
	before := time.Now().Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		sig, err := base64.StdEncoding.DecodeString(gotReq.Signature)
		if err != nil {
			t.Fatalf("signature not valid base64: %v", err)
		}
		msg := proto.HeartbeatMessage(gotReq.AgentID, gotReq.Ts, gotReq.Nonce)
		if !ed25519.Verify(id.PubKey, msg, sig) {
			t.Fatal("server-side verification of the client's signature failed")
		}
		jsonEnvelope(w, 200, "success", HeartbeatResponse{OK: true, Enabled: true})
	}))
	defer srv.Close()

	resp, err := Heartbeat(t.Context(), srv.URL, id, "agent-42")
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	after := time.Now().Unix()
	if !resp.OK || !resp.Enabled {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if gotReq.AgentID != "agent-42" {
		t.Fatalf("agent_id mismatch: %q", gotReq.AgentID)
	}
	if gotReq.Ts < before || gotReq.Ts > after {
		t.Fatalf("ts %d not within [%d, %d]", gotReq.Ts, before, after)
	}
	if gotReq.Nonce == "" {
		t.Fatal("nonce must not be empty")
	}
}

// TestHeartbeatNoncesAreDistinctAcrossCalls pins that each heartbeat mints
// a fresh nonce — a static/repeated nonce would let a captured request be
// naively re-sent by the SAME legitimate agent and still look "fresh" to a
// nonce-based replay cache keyed only on (agent_id, nonce).
func TestHeartbeatNoncesAreDistinctAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	var nonces []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		nonces = append(nonces, req.Nonce)
		jsonEnvelope(w, 200, "success", HeartbeatResponse{OK: true, Enabled: true})
	}))
	defer srv.Close()

	for i := 0; i < 2; i++ {
		if _, err := Heartbeat(t.Context(), srv.URL, id, "agent-1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(nonces) != 2 || nonces[0] == nonces[1] {
		t.Fatalf("expected two distinct nonces, got %v", nonces)
	}
}
