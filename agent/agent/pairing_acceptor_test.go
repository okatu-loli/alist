package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AlistGo/alist-agent/proto"
)

// genPairKey and mustTokenRaw are small local fixtures (duplicated rather than
// imported: proto.pairing_handshake_test.go's identically-named helpers live in
// package proto now, after the W3 proto-split, and this file's tests exercise
// PairingAcceptor — package agent — which cannot reach another package's
// unexported test helpers).
func genPairKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return pub, priv
}

// mustTokenRaw returns a fresh 32-byte raw reverse-pairing token (what both
// legs key their HMAC tags with).
func mustTokenRaw(t *testing.T) []byte {
	t.Helper()
	tok, err := proto.GenerateReversePairToken()
	if err != nil {
		t.Fatalf("GenerateReversePairToken: %v", err)
	}
	raw, err := proto.CanonicalizeReversePairToken(tok)
	if err != nil {
		t.Fatalf("CanonicalizeReversePairToken: %v", err)
	}
	return raw
}

// newTestAcceptor builds an acceptor backed by a freshly-generated identity
// persisted under a temp dir, with a fresh shared reverse-pairing token armed.
// It returns the acceptor, its identity, and the temp dir. The server/dialer
// side of every test must key its leg with acc.Token to complete the handshake.
func newTestAcceptor(t *testing.T) (*PairingAcceptor, *Identity, string) {
	t.Helper()
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	acc := NewPairingAcceptor(id, dir, ClientConfig{Host: "http://example:5244"})
	acc.Token = mustTokenRaw(t)
	// Default the operator confirm gate to AUTO-CONFIRM so the existing happy-
	// path tests exercise the pin+go-live flow. Tests that exercise the gate
	// itself (decline, hostile-first-then-legit) override Confirm explicitly.
	acc.Confirm = func(string) bool { return true }
	return acc, id, dir
}

// dialAndRunServerLeg dials the acceptor's httptest server as the SERVER/dialer
// and runs the token pairing server leg over the WS keyed with token. It returns
// the leg's outputs (or the dial error if the acceptor refused the upgrade).
func dialAndRunServerLeg(t *testing.T, url string, serverPub ed25519.PublicKey, token []byte) (agentPub []byte, agentID string, dialErr, legErr error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(url, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, "", err, nil
	}
	defer conn.Close()
	sc := proto.NewWSPairConn(conn)
	agentPub, agentID, legErr = proto.RunServerPairingHandshake(sc, serverPub, token)
	return agentPub, agentID, nil, legErr
}

// ---------------------------------------------------------------------------
// Happy path: acceptor pins the server key, sets AgentID, fires OnPaired.
// ---------------------------------------------------------------------------

func TestPairingAcceptorHappyPath(t *testing.T) {
	acc, id, dir := newTestAcceptor(t)
	serverPub, _ := genPairKey(t)

	var (
		gotCfg ClientConfig
		gotFP  string
		fired  int32
	)
	acc.OnPaired = func(cfg ClientConfig, sp []byte, fp string) {
		gotCfg = cfg
		gotFP = fp
		atomic.AddInt32(&fired, 1)
	}

	srv := httptest.NewServer(acc)
	defer srv.Close()

	agentPub, agentID, dialErr, legErr := dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}
	if legErr != nil {
		t.Fatalf("server pairing leg: %v", legErr)
	}

	waitFor(t, func() bool { return atomic.LoadInt32(&fired) == 1 })

	if atomic.LoadInt32(&fired) != 1 {
		t.Fatalf("OnPaired should fire exactly once, got %d", fired)
	}
	// The acceptor reported the (display) server fingerprint both sides can
	// compute — non-load-bearing, but it must match.
	if gotFP != proto.ReversePairFingerprint(serverPub) {
		t.Fatalf("OnPaired fingerprint mismatch: got %q want %q", gotFP, proto.ReversePairFingerprint(serverPub))
	}
	// The server received the REAL agent key ...
	if !ed25519.PublicKey(agentPub).Equal(id.PubKey) {
		t.Fatalf("server got the wrong agent pubkey")
	}
	// ... and both sides agree on the AgentID.
	if agentID != proto.FingerprintPubKey(id.PubKey) {
		t.Fatalf("agentID mismatch: %q vs %q", agentID, proto.FingerprintPubKey(id.PubKey))
	}
	if gotCfg.AgentID != proto.FingerprintPubKey(id.PubKey) {
		t.Fatalf("pinned cfg AgentID wrong: %q", gotCfg.AgentID)
	}
	if gotCfg.ServerPubKey != base64.StdEncoding.EncodeToString(serverPub) {
		t.Fatalf("pinned cfg ServerPubKey wrong")
	}
	// Host preserved from the base config.
	if gotCfg.Host != "http://example:5244" {
		t.Fatalf("Host not preserved: %q", gotCfg.Host)
	}
	// The acceptor DISARMED after the single success.
	if acc.Armed() {
		t.Fatal("acceptor should be disarmed after one successful pairing")
	}
	_ = dir
}

// A wrong token on the dialing server must fail the pairing and pin NOTHING —
// the agent leg's tagS verify fails, so the acceptor stays armed and unpinned.
func TestPairingAcceptorWrongTokenPinsNothing(t *testing.T) {
	acc, _, dir := newTestAcceptor(t)
	serverPub, _ := genPairKey(t)

	var paired int32
	acc.OnPaired = func(ClientConfig, []byte, string) { atomic.AddInt32(&paired, 1) }

	srv := httptest.NewServer(acc)
	defer srv.Close()

	// Dial with a DIFFERENT token than the acceptor armed.
	_, _, dialErr, _ := dialAndRunServerLeg(t, srv.URL, serverPub, mustTokenRaw(t))
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}

	// Give the acceptor a beat; nothing must be pinned/persisted and it stays armed.
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&paired) != 0 {
		t.Fatal("a wrong-token pairing must NOT pin")
	}
	if _, err := os.Stat(filepath.Join(dir, "agent_config.json")); !os.IsNotExist(err) {
		t.Fatalf("a wrong-token pairing must persist NO config, stat err = %v", err)
	}
	if !acc.Armed() {
		t.Fatal("a wrong-token pairing must leave the acceptor ARMED")
	}
}

// ---------------------------------------------------------------------------
// Config persist: pinned serverPub + AgentID land in agent_config.json (0600).
// ---------------------------------------------------------------------------

func TestPairingAcceptorPersistsConfig0600(t *testing.T) {
	acc, id, dir := newTestAcceptor(t)
	serverPub, _ := genPairKey(t)

	done := make(chan struct{})
	acc.OnPaired = func(ClientConfig, []byte, string) { close(done) }

	srv := httptest.NewServer(acc)
	defer srv.Close()

	_, _, dialErr, legErr := dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
	if dialErr != nil || legErr != nil {
		t.Fatalf("pairing failed: dial=%v leg=%v", dialErr, legErr)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnPaired never fired")
	}

	path := filepath.Join(dir, "agent_config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("agent_config.json not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("agent_config.json mode = %o, want 0600", perm)
	}
	cfg, err := LoadClientConfig(dir)
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if cfg.ServerPubKey != base64.StdEncoding.EncodeToString(serverPub) {
		t.Fatalf("persisted ServerPubKey wrong")
	}
	if cfg.AgentID != proto.FingerprintPubKey(id.PubKey) {
		t.Fatalf("persisted AgentID wrong: %q", cfg.AgentID)
	}
}

// ---------------------------------------------------------------------------
// Disarm: after one successful pairing a SECOND attempt is refused.
// ---------------------------------------------------------------------------

func TestPairingAcceptorDisarmsAfterSuccess(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	serverPub, _ := genPairKey(t)

	done := make(chan struct{})
	acc.OnPaired = func(ClientConfig, []byte, string) { close(done) }

	srv := httptest.NewServer(acc)
	defer srv.Close()

	// First pairing succeeds.
	_, _, dialErr, legErr := dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
	if dialErr != nil || legErr != nil {
		t.Fatalf("first pairing failed: dial=%v leg=%v", dialErr, legErr)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("first pairing OnPaired never fired")
	}

	// Second pairing must be refused at the upgrade (acceptor disarmed).
	_, _, dialErr2, _ := dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
	if dialErr2 == nil {
		t.Fatal("second pairing must be refused after the acceptor disarmed")
	}
}

// ---------------------------------------------------------------------------
// Not armed: an explicitly-disarmed acceptor refuses pairing.
// ---------------------------------------------------------------------------

func TestPairingAcceptorNotArmedRefuses(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	acc.Disarm()
	serverPub, _ := genPairKey(t)

	srv := httptest.NewServer(acc)
	defer srv.Close()

	_, _, dialErr, _ := dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
	if dialErr == nil {
		t.Fatal("a disarmed acceptor must refuse the pairing upgrade")
	}
}

// ---------------------------------------------------------------------------
// WS-only: a non-upgrade request is rejected with 400 and never pairs.
// ---------------------------------------------------------------------------

func TestPairingAcceptorRejectsNonWebSocket(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	srv := httptest.NewServer(acc)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-WS request: status = %d, want 400", resp.StatusCode)
	}
	if !acc.Armed() {
		t.Fatal("a rejected non-WS request must not disarm the acceptor")
	}
}

// ---------------------------------------------------------------------------
// Throttle: a per-IP block returns 429.
// ---------------------------------------------------------------------------

func TestPairingAcceptorThrottleUnit(t *testing.T) {
	th := newPairThrottle()
	ip := "203.0.113.7"
	if th.blocked(ip) {
		t.Fatal("fresh IP must not be blocked")
	}
	for i := 0; i < th.max; i++ {
		th.record(ip)
	}
	if !th.blocked(ip) {
		t.Fatalf("IP must be blocked after %d records", th.max)
	}
}

func TestPairingAcceptorThrottleRejects429(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	for i := 0; i < acc.throttle.max; i++ {
		acc.throttle.record("192.0.2.55")
	}

	req := httptest.NewRequest(http.MethodGet, "/agentpair", nil)
	req.RemoteAddr = "192.0.2.55:40000"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	acc.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled request: status = %d, want 429", rec.Code)
	}
	if !acc.Armed() {
		t.Fatal("a throttled request must not disarm the acceptor")
	}
}

// ---------------------------------------------------------------------------
// Single in-flight / no double-pin under -race: many concurrent honest dials,
// exactly ONE succeeds.
// ---------------------------------------------------------------------------

func TestPairingAcceptorSingleInFlightNoDoublePin(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	serverPub, _ := genPairKey(t)

	var pinned int32
	acc.OnPaired = func(ClientConfig, []byte, string) { atomic.AddInt32(&pinned, 1) }

	srv := httptest.NewServer(acc)
	defer srv.Close()

	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _, _ = dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
		}()
	}
	wg.Wait()

	waitFor(t, func() bool { return atomic.LoadInt32(&pinned) >= 1 })
	if got := atomic.LoadInt32(&pinned); got != 1 {
		t.Fatalf("expected exactly ONE successful pin, got %d (double-pin / race)", got)
	}
	if acc.Armed() {
		t.Fatal("acceptor must be disarmed after the single success")
	}
}

// The in-flight state machine directly. begin() now also hands back a private
// COPY of the armed token; here we only assert the ok gate.
func TestPairingAcceptorBeginFinishStateMachine(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)

	if _, ok := acc.begin(); !ok {
		t.Fatal("first begin must succeed on an armed acceptor")
	}
	if _, ok := acc.begin(); ok {
		t.Fatal("second begin must be refused while one is in flight")
	}
	acc.finish(false)
	if !acc.Armed() {
		t.Fatal("finish(false) must leave the acceptor armed")
	}
	if _, ok := acc.begin(); !ok {
		t.Fatal("begin must succeed again after a failed attempt released the slot")
	}
	acc.finish(true)
	if acc.Armed() {
		t.Fatal("finish(true) must disarm")
	}
	if _, ok := acc.begin(); ok {
		t.Fatal("begin must be refused after a successful pairing disarmed the acceptor")
	}
}

// ---------------------------------------------------------------------------
// Operator DECLINE gate (retained SP-E "pin ≠ live" state machine; in F1 the
// gate receives the display fingerprint). A declined pairing pins NOTHING and
// leaves the acceptor ARMED.
// ---------------------------------------------------------------------------

func TestPairingAcceptorDeclineLeavesUnpinnedAndArmed(t *testing.T) {
	acc, _, dir := newTestAcceptor(t)
	acc.Confirm = func(string) bool { return false } // operator declines
	serverPub, _ := genPairKey(t)

	var (
		paired   int32
		declined int32
	)
	acc.OnPaired = func(ClientConfig, []byte, string) { atomic.AddInt32(&paired, 1) }
	acc.OnDeclined = func(string) { atomic.AddInt32(&declined, 1) }

	srv := httptest.NewServer(acc)
	defer srv.Close()

	_, _, dialErr, legErr := dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
	if dialErr != nil || legErr != nil {
		t.Fatalf("handshake should complete before the decline: dial=%v leg=%v", dialErr, legErr)
	}

	waitFor(t, func() bool { return atomic.LoadInt32(&declined) == 1 })

	if atomic.LoadInt32(&paired) != 0 {
		t.Fatal("OnPaired must NOT fire on a declined pairing")
	}
	if _, err := os.Stat(filepath.Join(dir, "agent_config.json")); !os.IsNotExist(err) {
		t.Fatalf("a declined pairing must persist NO config, stat err = %v", err)
	}
	if !acc.Armed() {
		t.Fatal("a declined pairing must leave the acceptor ARMED")
	}
}

func TestPairingAcceptorHostileFirstThenLegit(t *testing.T) {
	acc, id, dir := newTestAcceptor(t)

	// Confirm declines the first successful handshake, confirms the second.
	var calls int32
	acc.Confirm = func(string) bool { return atomic.AddInt32(&calls, 1) >= 2 }

	var (
		paired    int32
		declined  int32
		pinnedCfg ClientConfig
	)
	acc.OnPaired = func(cfg ClientConfig, _ []byte, _ string) {
		pinnedCfg = cfg
		atomic.AddInt32(&paired, 1)
	}
	acc.OnDeclined = func(string) { atomic.AddInt32(&declined, 1) }

	srv := httptest.NewServer(acc)
	defer srv.Close()

	// FIRST: a server key the operator declines at the gate.
	firstPub, _ := genPairKey(t)
	_, _, dialErr, legErr := dialAndRunServerLeg(t, srv.URL, firstPub, acc.Token)
	if dialErr != nil || legErr != nil {
		t.Fatalf("first handshake should complete: dial=%v leg=%v", dialErr, legErr)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&declined) == 1 })
	if atomic.LoadInt32(&paired) != 0 {
		t.Fatal("declined pairing must NOT pin")
	}
	if !acc.Armed() {
		t.Fatal("acceptor must stay armed after the decline")
	}
	if _, err := os.Stat(filepath.Join(dir, "agent_config.json")); !os.IsNotExist(err) {
		t.Fatal("declined pairing must persist NO config")
	}

	// SECOND: the legitimate server key — confirmed at the gate → pinned + live.
	legitPub, _ := genPairKey(t)
	_, _, dialErr2, legErr2 := dialAndRunServerLeg(t, srv.URL, legitPub, acc.Token)
	if dialErr2 != nil || legErr2 != nil {
		t.Fatalf("legit handshake failed: dial=%v leg=%v", dialErr2, legErr2)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&paired) == 1 })

	wantLegit := base64.StdEncoding.EncodeToString(legitPub)
	wantFirst := base64.StdEncoding.EncodeToString(firstPub)
	if pinnedCfg.ServerPubKey != wantLegit {
		t.Fatalf("pinned key must be the legit server's; got %q want %q", pinnedCfg.ServerPubKey, wantLegit)
	}
	if pinnedCfg.ServerPubKey == wantFirst {
		t.Fatal("pinned key must NEVER be the declined server's")
	}
	persisted, err := LoadClientConfig(dir)
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if persisted.ServerPubKey != wantLegit {
		t.Fatalf("persisted key must be the legit server's; got %q", persisted.ServerPubKey)
	}
	if persisted.AgentID != proto.FingerprintPubKey(id.PubKey) {
		t.Fatalf("persisted AgentID wrong: %q", persisted.AgentID)
	}
	if acc.Armed() {
		t.Fatal("acceptor must disarm after the confirmed (legit) pairing")
	}
}

// A nil Confirm FAILS CLOSED.
func TestPairingAcceptorNilConfirmFailsClosed(t *testing.T) {
	acc, _, dir := newTestAcceptor(t)
	acc.Confirm = nil // no gate wired
	serverPub, _ := genPairKey(t)

	var declined int32
	acc.OnDeclined = func(string) { atomic.AddInt32(&declined, 1) }

	srv := httptest.NewServer(acc)
	defer srv.Close()

	_, _, dialErr, legErr := dialAndRunServerLeg(t, srv.URL, serverPub, acc.Token)
	if dialErr != nil || legErr != nil {
		t.Fatalf("handshake should complete: dial=%v leg=%v", dialErr, legErr)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&declined) == 1 })

	if _, err := os.Stat(filepath.Join(dir, "agent_config.json")); !os.IsNotExist(err) {
		t.Fatal("a nil confirm gate must pin NOTHING (fail closed)")
	}
	if !acc.Armed() {
		t.Fatal("a nil confirm gate must leave the acceptor ARMED")
	}
}

// Disarm must CLEAR the armed token from memory (design F Important #5), not
// merely flip armed=false — so a pasted T is never left in memory after the
// acceptor closes / the TTL expires.
func TestPairingAcceptorDisarmClearsToken(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	if len(acc.Token) == 0 {
		t.Fatal("precondition: acceptor must start with an armed token")
	}
	acc.Disarm()
	if acc.Token != nil {
		t.Fatalf("Disarm must clear the armed token, got %d bytes", len(acc.Token))
	}
	if acc.Armed() {
		t.Fatal("Disarm must leave the acceptor unarmed")
	}
}

// Arm re-arms a disarmed acceptor with a fresh token (the agent-side hook the
// pairing web calls on paste).
func TestPairingAcceptorArmRearms(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	acc.Disarm()
	if acc.Armed() {
		t.Fatal("precondition: disarmed")
	}
	tok := mustTokenRaw(t)
	acc.Arm(tok)
	if !acc.Armed() {
		t.Fatal("Arm must re-arm the acceptor")
	}
	// Arm keeps its OWN copy — zeroing the caller's slice must not affect it.
	for i := range tok {
		tok[i] = 0
	}
	got, ok := acc.begin()
	if !ok {
		t.Fatal("begin must succeed on a re-armed acceptor")
	}
	const reversePairTokenBytes = 32 // proto.pairing_token.go's fixed raw token length
	if len(got) != reversePairTokenBytes {
		t.Fatalf("begin token copy wrong length: %d", len(got))
	}
	var nonZero bool
	for _, b := range got {
		if b != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Fatal("Arm must keep an independent copy (zeroing the caller's slice must not clear it)")
	}
}

// waitFor polls cond up to 3s; fails the test if it never holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met within timeout")
	}
}
