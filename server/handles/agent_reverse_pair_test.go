package handles

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/AlistGo/alist-agent/proto"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/gin-gonic/gin"
)

// Reverse-pairing admin endpoint tests (design subproject F, wave F3). These pin
// the SERVER side of the TWO-STEP token-authenticated reverse-pairing flow:
//
//	STEP 1 mint  — POST .../pairing/reverse/token returns {token, token_id,
//	               expires_at}; token canonicalizes to the fixed 32 raw bytes.
//	STEP 2 dial  — POST .../pairing/reverse {reverse_addr, token_id} looks the
//	               token up, dials (via the reversePairDial seam), and on a
//	               passing verify persists an AUTO-ENABLED (Enabled=true,
//	               Confirmed=true) reverse row and BURNS the token.
//
// The dial is factored behind the reversePairDial seam so the handler test
// injects a fake returning canned (agentPub, agentID, fingerprint) — no live WS
// acceptor needed. Admin-gating itself is exercised end to end in
// server/router_v4_test.go.

// withServerIdentity installs a fresh server identity for the duration of a
// test and restores whatever was there before.
func withServerIdentity(t *testing.T) {
	t.Helper()
	si, err := agentsrv.LoadOrCreateServerIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	prev := agentsrv.CurrentServerIdentity()
	agentsrv.SetServerIdentity(si)
	t.Cleanup(func() { agentsrv.SetServerIdentity(prev) })
}

// swapReversePairDial replaces the pairing-dial seam and returns a restore func.
// The seam signature is (ctx, wsURL, serverPub, tokenRaw) → (agentPub, agentID,
// fingerprint, err).
func swapReversePairDial(f func(context.Context, string, []byte, []byte) ([]byte, string, string, error)) func() {
	prev := reversePairDial
	reversePairDial = f
	return func() { reversePairDial = prev }
}

func parseReversePairResp(t *testing.T, w *httptest.ResponseRecorder) ReverseStartAgentPairingResp {
	t.Helper()
	var env struct {
		Data ReverseStartAgentPairingResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse reverse-pair resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

func parseReverseMintResp(t *testing.T, w *httptest.ResponseRecorder) ReverseMintPairingTokenResp {
	t.Helper()
	var env struct {
		Data ReverseMintPairingTokenResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse reverse-mint resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// mintTokenForTest mints a token into the process-wide store and registers a
// cleanup that burns it, so the store never leaks across tests.
func mintTokenForTest(t *testing.T) (token, tokenID string) {
	t.Helper()
	token, tokenID, _, err := reversePairTokens.mint()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	t.Cleanup(func() { reversePairTokens.burn(tokenID) })
	return token, tokenID
}

// TestReverseMintPairingToken pins STEP 1: the mint endpoint returns a
// 32-byte-decodable base32 token, an opaque token_id, and a future expiry.
func TestReverseMintPairingToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	withServerIdentity(t)

	w := driveJSON(user, "POST", "", ReverseMintPairingToken)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("mint must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseReverseMintResp(t, w)
	if resp.TokenID == "" {
		t.Fatal("mint must return a token_id")
	}
	t.Cleanup(func() { reversePairTokens.burn(resp.TokenID) })

	raw, err := proto.CanonicalizeReversePairToken(resp.Token)
	if err != nil {
		t.Fatalf("minted token must canonicalize: %v (%q)", err, resp.Token)
	}
	if len(raw) != 32 {
		t.Fatalf("minted token must decode to 32 raw bytes, got %d", len(raw))
	}
	if !resp.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at must be in the future, got %v", resp.ExpiresAt)
	}
	// The minted token is actually armed in the store under its id.
	if _, ok := reversePairTokens.lookup(resp.TokenID); !ok {
		t.Fatal("a minted token must be armed in the store")
	}
}

// TestReverseMintPairingTokenNoServerIdentity pins the 400 when no server
// identity is wired — minting a token the dial could never use is refused early.
func TestReverseMintPairingTokenNoServerIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)

	prev := agentsrv.CurrentServerIdentity()
	agentsrv.SetServerIdentity(nil)
	t.Cleanup(func() { agentsrv.SetServerIdentity(prev) })

	w := driveJSON(user, "POST", "", ReverseMintPairingToken)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("mint with no server identity must be 400, got %d: %s", got, w.Body.String())
	}
}

// TestReverseStartAgentPairingAutoEnables is the happy path (STEP 2): with a
// token armed in the store and a dial that verifies the SAME token, the row is
// persisted AUTO-ENABLED (Enabled=true, Confirmed=true), the response carries
// {id, agent_id, fingerprint}, and the token is BURNED (single-use).
func TestReverseStartAgentPairingAutoEnables(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	withServerIdentity(t)

	agentPub, _ := genEd25519(t)
	agentID := proto.FingerprintPubKey(agentPub)
	fingerprint := proto.ReversePairFingerprint(agentPub)

	token, tokenID := mintTokenForTest(t)
	wantRaw, _ := proto.CanonicalizeReversePairToken(token)

	restore := swapReversePairDial(func(_ context.Context, wsURL string, serverPub []byte, tokenRaw []byte) ([]byte, string, string, error) {
		if wsURL != "wss://vps.example:5343/agentpair" {
			t.Errorf("dial URL = %q, want the base with path replaced by /agentpair", wsURL)
		}
		if len(serverPub) != 32 {
			t.Errorf("server pubkey not passed through: pub=%d", len(serverPub))
		}
		// The dial must be keyed with the SAME token the store armed — i.e. the
		// raw bytes of the token_id we passed. A stubbed agent holding this T
		// verifies.
		if !bytes.Equal(tokenRaw, wantRaw) {
			t.Errorf("dial keyed with the wrong token bytes")
		}
		return agentPub, agentID, fingerprint, nil
	})
	defer restore()

	body := `{"reverse_addr":"wss://vps.example:5343","token_id":"` + tokenID + `","name":"nas-rev"}`
	w := driveJSON(user, "POST", body, ReverseStartAgentPairing)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("reverse pairing must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseReversePairResp(t, w)
	if resp.AgentID != agentID {
		t.Fatalf("resp agent_id = %q, want %q", resp.AgentID, agentID)
	}
	if resp.ID == 0 {
		t.Fatal("resp must carry the created row id")
	}
	if resp.Fingerprint != fingerprint {
		t.Fatalf("resp fingerprint = %q, want %q", resp.Fingerprint, fingerprint)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(resp.ID) })

	// The token is burned (single-use) — a second lookup fails.
	if _, ok := reversePairTokens.lookup(tokenID); ok {
		t.Fatal("a successful pairing must burn the token (single-use)")
	}

	// Persisted shape: reverse, AUTO-ENABLED, /agentchannel data URL, fingerprint.
	row, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		t.Fatalf("row not persisted: %v", err)
	}
	if !row.IsReverse() {
		t.Fatalf("row mode = %q, want reverse", row.Mode)
	}
	if !row.Enabled {
		t.Fatal("token verify passed ⇒ row must be AUTO-ENABLED (Enabled=true)")
	}
	if !row.Confirmed {
		t.Fatal("token verify passed ⇒ row must be Confirmed=true (nothing left to compare)")
	}
	if row.ReverseAddr != "wss://vps.example:5343/agentchannel" {
		t.Fatalf("ReverseAddr = %q, want the /agentchannel data URL", row.ReverseAddr)
	}
	if row.Name != "nas-rev" {
		t.Fatalf("Name = %q, want nas-rev", row.Name)
	}
	if len(row.Whitelist()) != 0 {
		t.Fatalf("a new row must start with an empty whitelist, got %v", row.Whitelist())
	}
	if row.PubKey == "" {
		t.Fatal("PubKey must be persisted")
	}
	// PendingSAS is repurposed to the non-load-bearing display fingerprint.
	if got := strings.Join(row.PendingSASList(), " "); got != fingerprint {
		t.Fatalf("PendingSAS = %q, want the display fingerprint %q", got, fingerprint)
	}
}

// TestReverseStartAgentPairingUnknownToken pins that a missing / already-used
// token_id is a 400 and never dials or persists.
func TestReverseStartAgentPairingUnknownToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	withServerIdentity(t)

	dialed := false
	restore := swapReversePairDial(func(context.Context, string, []byte, []byte) ([]byte, string, string, error) {
		dialed = true
		return nil, "", "", nil
	})
	defer restore()

	// Case A: an id that was never minted.
	before, _ := db.ListAgents()
	w := driveJSON(user, "POST", `{"reverse_addr":"wss://vps.example:5343","token_id":"never-minted"}`, ReverseStartAgentPairing)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("unknown token_id must be 400, got %d: %s", got, w.Body.String())
	}

	// Case B: a burned (already-used) id — mint then burn, then dial with it.
	_, tokenID := mintTokenForTest(t)
	reversePairTokens.burn(tokenID)
	w = driveJSON(user, "POST", `{"reverse_addr":"wss://vps.example:5343","token_id":"`+tokenID+`"}`, ReverseStartAgentPairing)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("used token_id must be 400, got %d: %s", got, w.Body.String())
	}

	if dialed {
		t.Fatal("an unknown/used token_id must never reach the dialer")
	}
	after, _ := db.ListAgents()
	if len(after) != len(before) {
		t.Fatalf("a rejected token must create no row: before=%d after=%d", len(before), len(after))
	}
}

// TestReverseStartAgentPairingVerifyFailureRetryable pins that a dial/verify
// failure (e.g. the agent holds a DIFFERENT T, or is unreachable) is a clean
// error, creates NO row, and does NOT burn the token — the operator can retry
// within the TTL.
func TestReverseStartAgentPairingVerifyFailureRetryable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	withServerIdentity(t)

	_, tokenID := mintTokenForTest(t)

	restore := swapReversePairDial(func(context.Context, string, []byte, []byte) ([]byte, string, string, error) {
		// Simulate the handshake tag verify failing (wrong token on the agent) —
		// the same shape as an unreachable agent from the handler's view.
		return nil, "", "", errUnreachableTestAgent
	})
	defer restore()

	before, _ := db.ListAgents()
	w := driveJSON(user, "POST", `{"reverse_addr":"wss://vps.example:5343","token_id":"`+tokenID+`"}`, ReverseStartAgentPairing)
	if got := respCode(t, w); got == 200 {
		t.Fatalf("a verify/dial failure must not be 200: %s", w.Body.String())
	}
	after, _ := db.ListAgents()
	if len(after) != len(before) {
		t.Fatalf("a failed verify must create no row: before=%d after=%d", len(before), len(after))
	}
	// The token is NOT burned — still retryable within the TTL.
	if _, ok := reversePairTokens.lookup(tokenID); !ok {
		t.Fatal("a failed dial must NOT burn the token (retryable within TTL)")
	}
}

// TestReverseStartAgentPairingBadReverseAddr pins that a junk / non-ws address
// is a 400 and never dials or persists — even with a valid token_id present.
func TestReverseStartAgentPairingBadReverseAddr(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	withServerIdentity(t)

	_, tokenID := mintTokenForTest(t)

	dialed := false
	restore := swapReversePairDial(func(context.Context, string, []byte, []byte) ([]byte, string, string, error) {
		dialed = true
		return nil, "", "", nil
	})
	defer restore()

	for _, tc := range []struct{ name, body string }{
		{"http scheme", `{"reverse_addr":"http://vps.example:5343","token_id":"` + tokenID + `"}`},
		{"no host", `{"reverse_addr":"ws:///agentpair","token_id":"` + tokenID + `"}`},
		{"junk", `{"reverse_addr":"not a url at all","token_id":"` + tokenID + `"}`},
		{"missing addr", `{"token_id":"` + tokenID + `"}`},
		{"missing token", `{"reverse_addr":"wss://vps.example:5343"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := driveJSON(user, "POST", tc.body, ReverseStartAgentPairing)
			if got := respCode(t, w); got != 400 {
				t.Fatalf("%s must be 400, got %d: %s", tc.name, got, w.Body.String())
			}
		})
	}
	if dialed {
		t.Fatal("a rejected request must never reach the dialer")
	}
	// The token survives rejected requests (never consumed).
	if _, ok := reversePairTokens.lookup(tokenID); !ok {
		t.Fatal("a rejected request must not consume the token")
	}
}

// TestReverseStartAgentPairingNoServerIdentity pins the 400 when no server
// identity is wired this run — reverse pairing is unavailable.
func TestReverseStartAgentPairingNoServerIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)

	prev := agentsrv.CurrentServerIdentity()
	agentsrv.SetServerIdentity(nil)
	t.Cleanup(func() { agentsrv.SetServerIdentity(prev) })

	dialed := false
	restore := swapReversePairDial(func(context.Context, string, []byte, []byte) ([]byte, string, string, error) {
		dialed = true
		return nil, "", "", nil
	})
	defer restore()

	w := driveJSON(user, "POST", `{"reverse_addr":"wss://vps.example:5343","token_id":"x"}`, ReverseStartAgentPairing)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("no server identity must be 400, got %d: %s", got, w.Body.String())
	}
	if dialed {
		t.Fatal("must never dial when there is no server identity")
	}
}

var errUnreachableTestAgent = errStr("dial tcp: connection refused")

type errStr string

func (e errStr) Error() string { return string(e) }
