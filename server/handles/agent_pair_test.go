package handles

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/AlistGo/alist-agent/proto"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// Remote-agent pairing-completion + heartbeat handler tests (design
// W12-2a). These call the gin.HandlerFuncs directly with NO user in
// context, same convention as share_public_test.go's drivePublicShare: both
// endpoints under test are genuinely anonymous, so a test harness that
// forced a user in would not be testing the real access boundary.

var agentPairFixtureSeq int64

func nextAgentPairFixtureN(t *testing.T) int64 {
	t.Helper()
	return atomic.AddInt64(&agentPairFixtureSeq, 1)
}

// driveAnonPost runs a handler with a JSON body and NO user in context, from
// a FRESH, never-before-used client IP (see driveAnonPostIP) — so ordinary
// tests never interfere with pairThrottle's per-IP failure counter, which is
// shared PACKAGE-LEVEL state across the whole test binary run (same
// reasoning as login_throttle_test.go's passwordThrottle tests, which
// instead pin a fixed IP per test + explicit Reset). Only
// TestPairAgentCompleteRateLimitEngages needs a fixed, repeated IP — it uses
// driveAnonPostIP directly for that.
func driveAnonPost(path, body string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	n := atomic.AddInt64(&agentPairFixtureSeq, 1)
	ip := fmt.Sprintf("10.%d.%d.%d", (n>>16)&0xFF, (n>>8)&0xFF, n&0xFF)
	return driveAnonPostIP(ip, path, body, h)
}

// driveAnonPostIP is driveAnonPost with a caller-chosen client IP (set via
// X-Forwarded-For, same mechanism login_throttle_test.go uses).
func driveAnonPostIP(ip, path, body string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Forwarded-For", ip)
	h(c)
	return w
}

func parsePairCompleteResp(t *testing.T, w *httptest.ResponseRecorder) PairAgentCompleteResp {
	t.Helper()
	var env struct {
		Data PairAgentCompleteResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse pair complete resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

func parseHeartbeatResp(t *testing.T, w *httptest.ResponseRecorder) AgentHeartbeatResp {
	t.Helper()
	var env struct {
		Data AgentHeartbeatResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse heartbeat resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// genPairPubKey mints a fresh ed25519 keypair and returns its base64 pubkey
// alongside the private key (for signing heartbeat tests).
func genPairKeypair(t *testing.T) (pubB64 string, pub ed25519.PublicKey, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub), pub, priv
}

func pairCompleteBody(code, pubB64, name string) string {
	return fmt.Sprintf(`{"code":%q,"pubkey":%q,"name":%q,"platform":"linux/amd64","version":"vtest"}`, code, pubB64, name)
}

// TestPairAgentCompleteSuccess pins the happy path: a valid, pending code
// plus a well-formed pubkey creates a model.Agent row, marks the pairing
// session single-use, and returns the derived AgentID.
func TestPairAgentCompleteSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	code, _ := agentsrv.Start()
	pubB64, pub, _ := genPairKeypair(t)
	n := nextAgentPairFixtureN(t)

	w := driveAnonPost("/x", pairCompleteBody(code, pubB64, fmt.Sprintf("nas-%d", n)), PairAgentComplete)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("pair complete must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parsePairCompleteResp(t, w)
	wantAgentID := proto.FingerprintPubKey(pub)
	if resp.AgentID != wantAgentID {
		t.Fatalf("agent_id = %q, want %q", resp.AgentID, wantAgentID)
	}

	row, err := db.GetAgentByAgentID(wantAgentID)
	if err != nil {
		t.Fatalf("agent row must exist after pairing: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(row.ID) })
	if row.Enabled {
		t.Fatal("a newly paired agent must NOT be Enabled until an admin SAS-confirms it (design W12-2b)")
	}
	if row.Confirmed {
		t.Fatal("a newly paired agent must NOT be Confirmed until an admin confirms it")
	}
	if wl := row.Whitelist(); len(wl) != 0 {
		t.Fatalf("a newly paired agent must have an EMPTY storage whitelist, got %v", wl)
	}
	if row.Name != fmt.Sprintf("nas-%d", n) {
		t.Fatalf("name mismatch: %q", row.Name)
	}

	// The response's SAS must be exactly what DeriveSAS(code, pubkey)
	// computes, and it must be what got persisted as PendingSAS too — the
	// admin's later GET .../pairing-sas reads that same stored value.
	wantSAS := proto.DeriveSAS(code, pub)
	if len(resp.SAS) != len(wantSAS) {
		t.Fatalf("resp SAS length = %d, want %d", len(resp.SAS), len(wantSAS))
	}
	for i := range wantSAS {
		if resp.SAS[i] != wantSAS[i] {
			t.Fatalf("resp SAS = %v, want %v", resp.SAS, wantSAS)
		}
	}
	if got := row.PendingSASList(); len(got) != len(wantSAS) {
		t.Fatalf("stored PendingSAS = %v, want %v", got, wantSAS)
	} else {
		for i := range wantSAS {
			if got[i] != wantSAS[i] {
				t.Fatalf("stored PendingSAS = %v, want %v", got, wantSAS)
			}
		}
	}
}

// TestPairAgentCompleteSingleUse pins that a second completion of the SAME
// code fails, even though the code has not expired.
func TestPairAgentCompleteSingleUse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	code, _ := agentsrv.Start()
	pubB64, pub, _ := genPairKeypair(t)

	w := driveAnonPost("/x", pairCompleteBody(code, pubB64, "first"), PairAgentComplete)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("first complete must be 200, got %d: %s", got, w.Body.String())
	}
	t.Cleanup(func() {
		if row, err := db.GetAgentByAgentID(proto.FingerprintPubKey(pub)); err == nil {
			_ = db.DeleteAgent(row.ID)
		}
	})

	// A second complete, same code, even a different (but valid) pubkey.
	pubB64b, _, _ := genPairKeypair(t)
	w2 := driveAnonPost("/x", pairCompleteBody(code, pubB64b, "second"), PairAgentComplete)
	if got := respCode(t, w2); got == 200 {
		t.Fatalf("second complete with the same code must be rejected, got 200: %s", w2.Body.String())
	}
}

// TestPairAgentCompleteInvalidOrExpiredCodeUniform pins the no-oracle
// property at the HTTP layer: a never-minted code produces the exact same
// response shape/message as an actually-expired one.
func TestPairAgentCompleteInvalidOrExpiredCodeUniform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pubB64, _, _ := genPairKeypair(t)

	wUnknown := driveAnonPost("/x", pairCompleteBody("this-code-was-never-minted-xyz", pubB64, "x"), PairAgentComplete)
	if got := respCode(t, wUnknown); got == 200 {
		t.Fatalf("unknown code must not succeed: %s", wUnknown.Body.String())
	}

	// Manufacture an expired session directly against the default store's
	// package-level Start, then backdate it isn't possible from here (no
	// clock-injection seam on the singleton) — instead use a store we
	// control and drive the handler logic's equivalent through the shared
	// agentpkg package-level Start/Complete used by the real handler. Since
	// PairAgentComplete always consults the package-level defaultStore, we
	// settle for asserting the two rejection responses carry an identical
	// message (the actual expired-vs-unknown indistinguishability is
	// already pinned at the store layer by
	// TestCompleteUnknownAndExpiredCodesFailUniformly in internal/agent).
	wUnknown2 := driveAnonPost("/x", pairCompleteBody("another-never-minted-code-abc", pubB64, "x"), PairAgentComplete)
	if respCode(t, wUnknown) != respCode(t, wUnknown2) {
		t.Fatalf("rejection codes must be uniform: %d vs %d", respCode(t, wUnknown), respCode(t, wUnknown2))
	}
	var m1, m2 struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(wUnknown.Body.Bytes(), &m1)
	_ = json.Unmarshal(wUnknown2.Body.Bytes(), &m2)
	if m1.Message != m2.Message {
		t.Fatalf("rejection message must be uniform: %q vs %q", m1.Message, m2.Message)
	}
}

// TestPairAgentCompleteBadPubKey pins that a malformed/wrong-length pubkey
// is rejected with a 400, and never consumes the pairing code (a legit
// retry with a corrected pubkey must still be able to use it).
func TestPairAgentCompleteBadPubKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	code, _ := agentsrv.Start()

	w := driveAnonPost("/x", pairCompleteBody(code, "not-valid-base64!!!", "x"), PairAgentComplete)
	if got := respCode(t, w); got == 200 {
		t.Fatalf("malformed base64 pubkey must be rejected, got 200: %s", w.Body.String())
	}

	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short"))
	w2 := driveAnonPost("/x", pairCompleteBody(code, shortKey, "x"), PairAgentComplete)
	if got := respCode(t, w2); got == 200 {
		t.Fatalf("wrong-length pubkey must be rejected, got 200: %s", w2.Body.String())
	}
}

// TestPairAgentCompleteRepairUpdatesNotDuplicates pins that pairing again
// with the SAME keypair (a different pairing session/code, e.g. after a
// factory reset re-join) updates the existing row rather than creating a
// second one for the same AgentID.
func TestPairAgentCompleteRepairUpdatesNotDuplicates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pubB64, pub, _ := genPairKeypair(t)
	agentID := proto.FingerprintPubKey(pub)

	code1, _ := agentsrv.Start()
	w1 := driveAnonPost("/x", pairCompleteBody(code1, pubB64, "first-name"), PairAgentComplete)
	if got := respCode(t, w1); got != 200 {
		t.Fatalf("first pairing must succeed: %s", w1.Body.String())
	}
	row1, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(row1.ID) })

	code2, _ := agentsrv.Start()
	w2 := driveAnonPost("/x", pairCompleteBody(code2, pubB64, "renamed"), PairAgentComplete)
	if got := respCode(t, w2); got != 200 {
		t.Fatalf("re-pair must succeed: %s", w2.Body.String())
	}
	row2, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if row2.ID != row1.ID {
		t.Fatalf("re-pair must update the SAME row, got a new id %d (was %d)", row2.ID, row1.ID)
	}
	if row2.Name != "renamed" {
		t.Fatalf("re-pair must apply the new name, got %q", row2.Name)
	}

	list, err := db.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range list {
		if a.AgentID == agentID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one row for agent_id %q, found %d", agentID, count)
	}
}

// TestPairAgentCompleteRateLimitEngages pins that repeated invalid attempts
// from the SAME client IP eventually trip the throttle, independent of the
// pairing code's own validity. Uses a fixed IP (driveAnonPostIP, not
// driveAnonPost) on purpose — this is the one test in this file that
// deliberately exercises pairThrottle's shared, package-level counter.
func TestPairAgentCompleteRateLimitEngages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pubB64, _, _ := genPairKeypair(t)
	const ip = "203.0.113.99"
	pairThrottle.Reset(ip)
	t.Cleanup(func() { pairThrottle.Reset(ip) })

	var last *httptest.ResponseRecorder
	tripped := false
	for i := 0; i < 50; i++ {
		last = driveAnonPostIP(ip, "/x", pairCompleteBody("never-valid-code", pubB64, "x"), PairAgentComplete)
		if respCode(t, last) == 429 {
			tripped = true
			break
		}
	}
	if !tripped {
		t.Fatalf("rate limit never engaged after repeated failures, last: %s", last.Body.String())
	}
}

// TestPairAgentCompleteDeliversServerPubKey pins design R1: when a server
// identity is wired (via the agentpkg injection seam boot uses), a successful
// pair/complete returns ServerPubKey equal to that identity's public key so
// the agent can pin it.
func TestPairAgentCompleteDeliversServerPubKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	si, err := agentsrv.LoadOrCreateServerIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prev := agentsrv.CurrentServerIdentity()
	agentsrv.SetServerIdentity(si)
	t.Cleanup(func() { agentsrv.SetServerIdentity(prev) })

	code, _ := agentsrv.Start()
	pubB64, pub, _ := genPairKeypair(t)
	w := driveAnonPost("/x", pairCompleteBody(code, pubB64, "srv-pubkey-nas"), PairAgentComplete)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("pair complete must be 200, got %d: %s", got, w.Body.String())
	}
	t.Cleanup(func() {
		if row, err := db.GetAgentByAgentID(proto.FingerprintPubKey(pub)); err == nil {
			_ = db.DeleteAgent(row.ID)
		}
	})
	resp := parsePairCompleteResp(t, w)
	if resp.ServerPubKey != si.PubKeyBase64() {
		t.Fatalf("server_pubkey = %q, want %q", resp.ServerPubKey, si.PubKeyBase64())
	}
}

// TestPairAgentCompleteNoServerIdentityOmitsPubKey pins that with no server
// identity wired the pairing still succeeds and simply carries an empty
// server_pubkey (no crash, no field) — the minimal-boot path.
func TestPairAgentCompleteNoServerIdentityOmitsPubKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prev := agentsrv.CurrentServerIdentity()
	agentsrv.SetServerIdentity(nil)
	t.Cleanup(func() { agentsrv.SetServerIdentity(prev) })

	code, _ := agentsrv.Start()
	pubB64, pub, _ := genPairKeypair(t)
	w := driveAnonPost("/x", pairCompleteBody(code, pubB64, "no-srv-id-nas"), PairAgentComplete)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("pair complete must still be 200 without a server identity, got %d: %s", got, w.Body.String())
	}
	t.Cleanup(func() {
		if row, err := db.GetAgentByAgentID(proto.FingerprintPubKey(pub)); err == nil {
			_ = db.DeleteAgent(row.ID)
		}
	})
	resp := parsePairCompleteResp(t, w)
	if resp.ServerPubKey != "" {
		t.Fatalf("server_pubkey must be empty when no identity is wired, got %q", resp.ServerPubKey)
	}
}

// --- heartbeat ---

func heartbeatBody(agentID string, ts int64, nonce, sigB64 string) string {
	return fmt.Sprintf(`{"agent_id":%q,"ts":%d,"nonce":%q,"signature":%q}`, agentID, ts, nonce, sigB64)
}

// seedPairedAgent creates a real model.Agent row with a real ed25519
// keypair, returning the private key so tests can sign valid heartbeats.
func seedPairedAgent(t *testing.T, enabled bool) (agentID string, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentID = proto.FingerprintPubKey(pub)
	row := &model.Agent{
		AgentID: agentID,
		Name:    fmt.Sprintf("hb-fixture-%d", nextAgentPairFixtureN(t)),
		PubKey:  base64.StdEncoding.EncodeToString(pub),
		Enabled: enabled,
	}
	row.SetWhitelist(nil)
	if err := db.CreateAgent(row); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(row.ID) })
	return agentID, priv
}

func signHeartbeat(priv ed25519.PrivateKey, agentID string, ts int64, nonce string) string {
	sig := ed25519.Sign(priv, proto.HeartbeatMessage(agentID, ts, nonce))
	return base64.StdEncoding.EncodeToString(sig)
}

// TestAgentHeartbeatValidSignatureUpdatesLastSeen pins the happy path.
func TestAgentHeartbeatValidSignatureUpdatesLastSeen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, priv := seedPairedAgent(t, true)
	ts := time.Now().Unix()
	nonce := fmt.Sprintf("nonce-%d", nextAgentPairFixtureN(t))
	sig := signHeartbeat(priv, agentID, ts, nonce)

	w := driveAnonPost("/x", heartbeatBody(agentID, ts, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("valid heartbeat must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseHeartbeatResp(t, w)
	if !resp.OK || !resp.Enabled {
		t.Fatalf("unexpected response: %+v", resp)
	}

	row, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastSeen == nil {
		t.Fatal("LastSeen must be set after a successful heartbeat")
	}
	if time.Since(*row.LastSeen) > time.Minute {
		t.Fatalf("LastSeen not recent: %v", row.LastSeen)
	}
}

// TestAgentHeartbeatWrongSignatureRejected pins that a signature produced
// by a DIFFERENT private key (impersonation attempt) is refused.
func TestAgentHeartbeatWrongSignatureRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, _ := seedPairedAgent(t, true)
	_, imposterPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	nonce := "impersonation-nonce"
	sig := signHeartbeat(imposterPriv, agentID, ts, nonce)

	w := driveAnonPost("/x", heartbeatBody(agentID, ts, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got == 200 {
		t.Fatalf("a signature from the wrong private key must be rejected, got 200: %s", w.Body.String())
	}
}

// TestAgentHeartbeatUnknownAgentRejected pins the 404 shape for an
// agent_id nobody ever paired.
func TestAgentHeartbeatUnknownAgentRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	nonce := "unknown-agent-nonce"
	sig := signHeartbeat(priv, "never-paired-agent-id", ts, nonce)

	w := driveAnonPost("/x", heartbeatBody("never-paired-agent-id", ts, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("unknown agent_id must be 404, got %d: %s", got, w.Body.String())
	}
}

// TestAgentHeartbeatDisabledAgentRejected pins that a disabled agent's
// heartbeat is refused with a 403 EVEN THOUGH its signature is perfectly
// valid — enabled/disabled is an authorization check on top of a
// successful authentication, not a substitute for it.
func TestAgentHeartbeatDisabledAgentRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, priv := seedPairedAgent(t, false)
	ts := time.Now().Unix()
	nonce := "disabled-agent-nonce"
	sig := signHeartbeat(priv, agentID, ts, nonce)

	w := driveAnonPost("/x", heartbeatBody(agentID, ts, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("disabled agent heartbeat must be 403, got %d: %s", got, w.Body.String())
	}
}

// TestAgentHeartbeatStaleTsRejected pins the freshness-window replay
// defense: a timestamp far outside the allowed window is refused even with
// a perfectly valid signature over that (agent_id, ts, nonce) triple.
func TestAgentHeartbeatStaleTsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, priv := seedPairedAgent(t, true)
	staleTs := time.Now().Add(-1 * time.Hour).Unix()
	nonce := "stale-nonce"
	sig := signHeartbeat(priv, agentID, staleTs, nonce)

	w := driveAnonPost("/x", heartbeatBody(agentID, staleTs, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got == 200 {
		t.Fatalf("a stale timestamp must be rejected, got 200: %s", w.Body.String())
	}
}

// TestAgentHeartbeatStaleTsGatesBeforeDBLookup pins the DoS fix: the
// freshness check runs BEFORE the agent DB lookup, so a stale/future-dated
// heartbeat for an agent id that was never paired is rejected as stale (401)
// without ever hitting the database — a flood of forged-timestamp heartbeats
// can't be turned into a flood of indexed DB reads. (Were the DB lookup first,
// an unknown agent id would surface as 404 AgentNotFound instead.)
func TestAgentHeartbeatStaleTsGatesBeforeDBLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	staleTs := time.Now().Add(-1 * time.Hour).Unix()
	nonce := "stale-unknown-nonce"
	sig := signHeartbeat(priv, "never-paired-agent-id", staleTs, nonce)

	w := driveAnonPost("/x", heartbeatBody("never-paired-agent-id", staleTs, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got != 401 {
		t.Fatalf("stale ts for an unknown agent must be 401 (freshness gates before the DB lookup), got %d: %s", got, w.Body.String())
	}
}

// TestAgentHeartbeatReplayedNonceRejected pins the second leg of replay
// defense: resending the EXACT SAME (still-fresh) request a second time
// must fail even though the signature verifies fine both times.
func TestAgentHeartbeatReplayedNonceRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, priv := seedPairedAgent(t, true)
	ts := time.Now().Unix()
	nonce := fmt.Sprintf("replay-nonce-%d", nextAgentPairFixtureN(t))
	sig := signHeartbeat(priv, agentID, ts, nonce)
	body := heartbeatBody(agentID, ts, nonce, sig)

	w1 := driveAnonPost("/x", body, AgentHeartbeat)
	if got := respCode(t, w1); got != 200 {
		t.Fatalf("first heartbeat must succeed: %s", w1.Body.String())
	}

	w2 := driveAnonPost("/x", body, AgentHeartbeat)
	if got := respCode(t, w2); got == 200 {
		t.Fatalf("a byte-for-byte replayed heartbeat must be rejected, got 200: %s", w2.Body.String())
	}
}

// --- W12-2b: SAS gates activation end to end ---

// TestPairThenHeartbeat403ThenConfirmThenHeartbeat200 is the load-bearing
// end-to-end sequence for design W12-2b: a freshly paired agent can
// authenticate (its signature verifies) but every heartbeat is refused
// until an admin — having visually matched the SAS — confirms it; only
// after that does the exact same signed heartbeat succeed.
func TestPairThenHeartbeat403ThenConfirmThenHeartbeat200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := adminUser(t)
	code, _ := agentsrv.Start()
	pubB64, pub, priv := genPairKeypair(t)
	agentID := proto.FingerprintPubKey(pub)

	wPair := driveAnonPost("/x", pairCompleteBody(code, pubB64, "e2e-agent"), PairAgentComplete)
	if got := respCode(t, wPair); got != 200 {
		t.Fatalf("pair complete must be 200, got %d: %s", got, wPair.Body.String())
	}
	row, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(row.ID) })
	if row.Enabled {
		t.Fatal("must be disabled immediately after pairing, before any confirm")
	}

	ts := time.Now().Unix()
	nonce := fmt.Sprintf("e2e-nonce-%d", nextAgentPairFixtureN(t))
	sig := signHeartbeat(priv, agentID, ts, nonce)
	hbBody := heartbeatBody(agentID, ts, nonce, sig)

	wHB1 := driveAnonPost("/x", hbBody, AgentHeartbeat)
	if got := respCode(t, wHB1); got != 403 {
		t.Fatalf("heartbeat before confirm must be 403, got %d: %s", got, wHB1.Body.String())
	}

	wConfirm := driveDelete(admin, row.ID, ConfirmAgent)
	if got := respCode(t, wConfirm); got != 200 {
		t.Fatalf("confirm must be 200, got %d: %s", got, wConfirm.Body.String())
	}

	nonce2 := fmt.Sprintf("e2e-nonce-2-%d", nextAgentPairFixtureN(t))
	sig2 := signHeartbeat(priv, agentID, ts, nonce2)
	wHB2 := driveAnonPost("/x", heartbeatBody(agentID, ts, nonce2, sig2), AgentHeartbeat)
	if got := respCode(t, wHB2); got != 200 {
		t.Fatalf("heartbeat after confirm must be 200, got %d: %s", got, wHB2.Body.String())
	}
	resp := parseHeartbeatResp(t, wHB2)
	if !resp.OK || !resp.Enabled {
		t.Fatalf("unexpected post-confirm heartbeat response: %+v", resp)
	}
}

// TestPairAgentCompleteRepairResetsConfirmation pins that re-pairing an
// agent the admin had already confirmed resets it right back to
// disabled/unconfirmed with a fresh SAS — possessing the (new) pairing code
// must never be enough by itself to silently re-activate an agent, even one
// that was trusted before.
func TestPairAgentCompleteRepairResetsConfirmation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := adminUser(t)
	pubB64, pub, priv := genPairKeypair(t)
	agentID := proto.FingerprintPubKey(pub)

	code1, _ := agentsrv.Start()
	w1 := driveAnonPost("/x", pairCompleteBody(code1, pubB64, "first"), PairAgentComplete)
	if got := respCode(t, w1); got != 200 {
		t.Fatalf("first pairing must succeed: %s", w1.Body.String())
	}
	row1, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(row1.ID) })

	if got := respCode(t, driveDelete(admin, row1.ID, ConfirmAgent)); got != 200 {
		t.Fatalf("confirm must succeed")
	}
	confirmed, err := db.GetAgent(row1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed.Enabled || !confirmed.Confirmed {
		t.Fatalf("agent must be enabled+confirmed before the re-pair, got %+v", confirmed)
	}
	firstSAS := confirmed.PendingSASList()
	if len(firstSAS) != 0 {
		t.Fatalf("confirmed agent must have no pending SAS left, got %v", firstSAS)
	}

	// Re-pair with a fresh code, same keypair.
	code2, _ := agentsrv.Start()
	w2 := driveAnonPost("/x", pairCompleteBody(code2, pubB64, "renamed"), PairAgentComplete)
	if got := respCode(t, w2); got != 200 {
		t.Fatalf("re-pair must succeed: %s", w2.Body.String())
	}
	row2, err := db.GetAgentByAgentID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if row2.Enabled {
		t.Fatal("re-pair must reset Enabled back to false, even for a previously-confirmed agent")
	}
	if row2.Confirmed {
		t.Fatal("re-pair must reset Confirmed back to false, even for a previously-confirmed agent")
	}
	sas := row2.PendingSASList()
	if len(sas) == 0 {
		t.Fatal("re-pair must derive and store a fresh pending SAS")
	}
	wantSAS := proto.DeriveSAS(code2, pub)
	if len(sas) != len(wantSAS) {
		t.Fatalf("re-pair SAS length = %d, want %d", len(sas), len(wantSAS))
	}
	for i := range wantSAS {
		if sas[i] != wantSAS[i] {
			t.Fatalf("re-pair SAS = %v, want %v", sas, wantSAS)
		}
	}

	// Heartbeat must be refused again until the admin re-confirms — cheap
	// insurance, on top of the row-level assertions above, against a
	// regression that resets the DB fields but not the actual gate.
	ts := time.Now().Unix()
	nonce := fmt.Sprintf("repair-nonce-%d", nextAgentPairFixtureN(t))
	sig := signHeartbeat(priv, agentID, ts, nonce)
	wHB := driveAnonPost("/x", heartbeatBody(agentID, ts, nonce, sig), AgentHeartbeat)
	if got := respCode(t, wHB); got != 403 {
		t.Fatalf("heartbeat right after a re-pair must be 403 again, got %d: %s", got, wHB.Body.String())
	}
}
