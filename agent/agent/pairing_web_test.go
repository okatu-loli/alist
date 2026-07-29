package agent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AlistGo/alist-agent/proto"
)

// newTestPairingWeb builds a PairingWeb around a fresh acceptor (its identity in
// a temp dir), plus two httptest servers: accSrv serves the PUBLIC pairing
// acceptor (the server dials this) and webSrv serves the LOCAL control page (the
// operator pastes T here). NewPairingWeb starts the acceptor DISARMED.
func newTestPairingWeb(t *testing.T) (pw *PairingWeb, acc *PairingAcceptor, accSrv, webSrv *httptest.Server, id *Identity, dir string) {
	t.Helper()
	dir = t.TempDir()
	var err error
	id, err = LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	acc = NewPairingAcceptor(id, dir, ClientConfig{Host: "http://example:5244"})
	pw = NewPairingWeb(acc)
	accSrv = httptest.NewServer(acc)
	webSrv = httptest.NewServer(pw)
	t.Cleanup(func() { accSrv.Close(); webSrv.Close() })
	return pw, acc, accSrv, webSrv, id, dir
}

// armViaWeb POSTs a pasted token to the web /arm endpoint and returns the HTTP
// status and decoded body.
func armViaWeb(t *testing.T, webURL, token string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token})
	resp, err := http.Post(webURL+"/arm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /arm: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// ---------------------------------------------------------------------------
// Arm a valid pasted T → acceptor arms → a token-matching handshake pins + goes
// live, and the web status reports success with agent_id + fingerprint.
// ---------------------------------------------------------------------------

func TestPairingWebArmAndPairGoesLive(t *testing.T) {
	pw, acc, accSrv, webSrv, id, _ := newTestPairingWeb(t)
	serverPub, _ := genPairKey(t)

	// The cmd-layer go-live hook.
	var live int32
	pw.OnPaired = func(cfg ClientConfig, sp []byte, fp string) { atomic.AddInt32(&live, 1) }

	tokStr, err := proto.GenerateReversePairToken()
	if err != nil {
		t.Fatalf("gen token: %v", err)
	}
	raw, err := proto.CanonicalizeReversePairToken(tokStr)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}

	// Before arming, the acceptor is DISARMED (a dial must be refused).
	if acc.Armed() {
		t.Fatal("acceptor must start disarmed before a token is pasted")
	}

	// Paste T + arm.
	code, _ := armViaWeb(t, webSrv.URL, tokStr)
	if code != http.StatusOK {
		t.Fatalf("arm with a valid token: status = %d, want 200", code)
	}
	if !acc.Armed() {
		t.Fatal("a valid pasted token must ARM the acceptor")
	}
	if pw.Status().Phase != PhaseWaiting {
		t.Fatalf("after arm, phase = %q, want waiting", pw.Status().Phase)
	}

	// The server dials in with the matching token → pins + goes live.
	agentPub, agentID, dialErr, legErr := dialAndRunServerLeg(t, accSrv.URL, serverPub, raw)
	if dialErr != nil || legErr != nil {
		t.Fatalf("pairing handshake failed: dial=%v leg=%v", dialErr, legErr)
	}
	if !ed25519.PublicKey(agentPub).Equal(id.PubKey) {
		t.Fatal("server got the wrong agent pubkey")
	}

	waitFor(t, func() bool { return pw.Status().Phase == PhaseSuccess })
	st := pw.Status()
	if st.AgentID != proto.FingerprintPubKey(id.PubKey) || st.AgentID != agentID {
		t.Fatalf("status agent_id = %q, want %q", st.AgentID, proto.FingerprintPubKey(id.PubKey))
	}
	if st.Fingerprint != proto.ReversePairFingerprint(serverPub) {
		t.Fatalf("status fingerprint = %q, want %q", st.Fingerprint, proto.ReversePairFingerprint(serverPub))
	}
	if atomic.LoadInt32(&live) != 1 {
		t.Fatalf("OnPaired go-live hook must fire exactly once, got %d", live)
	}
	if acc.Armed() {
		t.Fatal("acceptor must disarm after the single successful pairing (single-use burn)")
	}
}

// ---------------------------------------------------------------------------
// Wrong/short T rejected at the WEB layer: no arm.
// ---------------------------------------------------------------------------

func TestPairingWebRejectsInvalidTokenNoArm(t *testing.T) {
	_, acc, _, webSrv, _, _ := newTestPairingWeb(t)

	for _, bad := range []string{
		"",              // empty
		"   ",           // whitespace only
		"AAAA",          // valid base32 but decodes to too few bytes
		"not base32 !!", // not base32 at all
		"810",           // '8'/'1'/'0' are not in the base32 alphabet
	} {
		code, body := armViaWeb(t, webSrv.URL, bad)
		if code != http.StatusBadRequest {
			t.Fatalf("arm with %q: status = %d, want 400", bad, code)
		}
		if _, ok := body["error"]; !ok {
			t.Fatalf("arm with %q: expected an error message, got %v", bad, body)
		}
		if acc.Armed() {
			t.Fatalf("arm with invalid token %q must NOT arm the acceptor", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Wrong T at the handshake: pins nothing, stays armed, web status = failed.
// ---------------------------------------------------------------------------

func TestPairingWebWrongTokenHandshakeStaysArmed(t *testing.T) {
	pw, acc, accSrv, webSrv, _, dir := newTestPairingWeb(t)
	serverPub, _ := genPairKey(t)

	tokStr, _ := proto.GenerateReversePairToken()
	code, _ := armViaWeb(t, webSrv.URL, tokStr)
	if code != http.StatusOK {
		t.Fatalf("arm: status %d", code)
	}

	// The dialing server presents a DIFFERENT token than was armed.
	_, _, dialErr, _ := dialAndRunServerLeg(t, accSrv.URL, serverPub, mustTokenRaw(t))
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}

	waitFor(t, func() bool { return pw.Status().Phase == PhaseFailed })
	if !acc.Armed() {
		t.Fatal("a wrong-token handshake must leave the acceptor ARMED for a retry")
	}
	if _, err := os.Stat(filepath.Join(dir, "agent_config.json")); !os.IsNotExist(err) {
		t.Fatalf("a wrong-token handshake must persist NO config, stat err = %v", err)
	}
	if pw.Status().Reason == "" {
		t.Fatal("a failed attempt must report a reason in the status")
	}
}

// ---------------------------------------------------------------------------
// TTL expiry auto-disarms + clears T.
// ---------------------------------------------------------------------------

func TestPairingWebTokenTTLExpiryAutoDisarms(t *testing.T) {
	pw, acc, accSrv, webSrv, _, _ := newTestPairingWeb(t)
	serverPub, _ := genPairKey(t)

	pw.ttl = 60 * time.Millisecond // shrink the TTL for the test
	tokStr, _ := proto.GenerateReversePairToken()
	if code, _ := armViaWeb(t, webSrv.URL, tokStr); code != http.StatusOK {
		t.Fatalf("arm: status %d", code)
	}
	if !acc.Armed() {
		t.Fatal("precondition: armed after paste")
	}

	// After the TTL the acceptor auto-disarms and T is cleared.
	waitFor(t, func() bool { return !acc.Armed() })
	if acc.Token != nil {
		t.Fatal("TTL expiry must CLEAR the armed token from memory")
	}
	if pw.Status().Phase != PhaseExpired {
		t.Fatalf("after TTL, phase = %q, want expired", pw.Status().Phase)
	}

	// A dial after expiry is refused (nothing to pair against).
	raw, _ := proto.CanonicalizeReversePairToken(tokStr)
	_, _, dialErr, _ := dialAndRunServerLeg(t, accSrv.URL, serverPub, raw)
	if dialErr == nil {
		t.Fatal("a dial after TTL expiry must be refused (acceptor disarmed)")
	}
}

// ---------------------------------------------------------------------------
// A dropped attempt within the TTL still allows a retry (single-use burns on
// success-or-TTL, NOT on one failed/dropped attempt).
// ---------------------------------------------------------------------------

func TestPairingWebDroppedAttemptWithinTTLAllowsRetry(t *testing.T) {
	pw, acc, accSrv, webSrv, id, _ := newTestPairingWeb(t)
	serverPub, _ := genPairKey(t)

	tokStr, _ := proto.GenerateReversePairToken()
	raw, _ := proto.CanonicalizeReversePairToken(tokStr)
	if code, _ := armViaWeb(t, webSrv.URL, tokStr); code != http.StatusOK {
		t.Fatalf("arm: status %d", code)
	}

	// FIRST attempt: connect the WS then DROP it immediately without completing
	// the handshake — the acceptor's handshake read fails → failed, stays armed.
	wsURL := "ws" + strings.TrimPrefix(accSrv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	_ = conn.Close()

	waitFor(t, func() bool { return pw.Status().Phase == PhaseFailed })
	if !acc.Armed() {
		t.Fatal("a dropped attempt must leave the acceptor ARMED for a retry within the TTL")
	}

	// SECOND attempt within the TTL: the same armed token, no re-paste → success.
	agentPub, _, dialErr, legErr := dialAndRunServerLeg(t, accSrv.URL, serverPub, raw)
	if dialErr != nil || legErr != nil {
		t.Fatalf("retry handshake failed: dial=%v leg=%v", dialErr, legErr)
	}
	if !ed25519.PublicKey(agentPub).Equal(id.PubKey) {
		t.Fatal("retry: server got the wrong agent pubkey")
	}
	waitFor(t, func() bool { return pw.Status().Phase == PhaseSuccess })
	if acc.Armed() {
		t.Fatal("acceptor must disarm after the successful retry")
	}
}

// ---------------------------------------------------------------------------
// Single-use burns on success: a SECOND dial after a successful pairing is
// refused even with the correct token.
// ---------------------------------------------------------------------------

func TestPairingWebSingleUseBurnsOnSuccess(t *testing.T) {
	pw, _, accSrv, webSrv, _, _ := newTestPairingWeb(t)
	serverPub, _ := genPairKey(t)

	tokStr, _ := proto.GenerateReversePairToken()
	raw, _ := proto.CanonicalizeReversePairToken(tokStr)
	if code, _ := armViaWeb(t, webSrv.URL, tokStr); code != http.StatusOK {
		t.Fatalf("arm: status %d", code)
	}

	if _, _, dialErr, legErr := dialAndRunServerLeg(t, accSrv.URL, serverPub, raw); dialErr != nil || legErr != nil {
		t.Fatalf("first pairing failed: dial=%v leg=%v", dialErr, legErr)
	}
	waitFor(t, func() bool { return pw.Status().Phase == PhaseSuccess })

	// Second dial with the same (correct) token is refused — the token was burned.
	if _, _, dialErr, _ := dialAndRunServerLeg(t, accSrv.URL, serverPub, raw); dialErr == nil {
		t.Fatal("a second dial after a successful pairing must be refused (single-use burn)")
	}
}

// ---------------------------------------------------------------------------
// Secret-handling headers on the page + the strict CSP, and a functional page.
// ---------------------------------------------------------------------------

func TestPairingWebSecretHeadersAndPage(t *testing.T) {
	_, _, _, webSrv, _, _ := newTestPairingWeb(t)

	resp, err := http.Get(webSrv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	h := resp.Header
	if !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Fatalf("Cache-Control must be no-store, got %q", h.Get("Cache-Control"))
	}
	if h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("Referrer-Policy must be no-referrer, got %q", h.Get("Referrer-Policy"))
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options must be nosniff, got %q", h.Get("X-Content-Type-Options"))
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("X-Frame-Options must be DENY, got %q", h.Get("X-Frame-Options"))
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP must lock default-src to 'none', got %q", csp)
	}
	body := make([]byte, 8192)
	n, _ := resp.Body.Read(body)
	page := string(body[:n])
	if !strings.Contains(page, "布防") {
		t.Fatal("page must render the arm button copy")
	}
}

// The status endpoint returns the JSON the page polls.
func TestPairingWebStatusEndpoint(t *testing.T) {
	_, _, _, webSrv, _, _ := newTestPairingWeb(t)
	resp, err := http.Get(webSrv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	var st reversePairStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.Phase != PhaseIdle {
		t.Fatalf("fresh web status phase = %q, want idle", st.Phase)
	}
	if resp.Header.Get("Cache-Control") == "" {
		t.Fatal("status response must also carry secret-handling headers")
	}
}

// ---------------------------------------------------------------------------
// ResolvePairWebBind: default-localhost + non-loopback plain-HTTP refusal.
// ---------------------------------------------------------------------------

func TestResolvePairWebBind(t *testing.T) {
	cases := []struct {
		name         string
		addr         string
		allowPublic  bool
		wantResolved string
		wantErr      bool
		wantWarn     bool
	}{
		{"bare port defaults to loopback", "5344", false, "127.0.0.1:5344", false, false},
		{"empty host defaults to loopback", ":5344", false, "127.0.0.1:5344", false, false},
		{"explicit loopback ok", "127.0.0.1:5344", false, "127.0.0.1:5344", false, false},
		{"127/8 loopback ok", "127.0.0.5:5344", false, "127.0.0.5:5344", false, false},
		{"localhost ok", "localhost:5344", false, "localhost:5344", false, false},
		{"ipv6 loopback ok", "[::1]:5344", false, "[::1]:5344", false, false},
		{"routable ip refused", "192.168.1.9:5344", false, "", true, false},
		{"all-interfaces refused", "0.0.0.0:5344", false, "", true, false},
		{"ipv6 unspecified refused", "[::]:5344", false, "", true, false},
		{"hostname refused", "agent.example.com:5344", false, "", true, false},
		{"public with ack allowed + warns", "0.0.0.0:5344", true, "0.0.0.0:5344", false, true},
		{"routable with ack allowed + warns", "192.168.1.9:5344", true, "192.168.1.9:5344", false, true},
		{"empty addr errors", "", false, "", true, false},
		{"missing port errors", "127.0.0.1:", false, "", true, false},
		{"non-numeric port errors", "127.0.0.1:http", false, "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resolved, warn, err := ResolvePairWebBind(c.addr, c.allowPublic)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ResolvePairWebBind(%q, %v) must error", c.addr, c.allowPublic)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePairWebBind(%q, %v) unexpected error: %v", c.addr, c.allowPublic, err)
			}
			if resolved != c.wantResolved {
				t.Fatalf("resolved = %q, want %q", resolved, c.wantResolved)
			}
			if c.wantWarn && warn == "" {
				t.Fatal("a public bind with ack must return a cleartext warning")
			}
			if !c.wantWarn && warn != "" {
				t.Fatalf("a loopback bind must NOT warn, got %q", warn)
			}
		})
	}
}

// TestPairingWebNoInnerHTMLSink guards the DOM-XSS fix: the pairing page renders
// dynamic status (reason/agent_id/fingerprint — reason can carry peer-influenced
// bytes) via createElement + textContent, never by assigning interpolated markup.
// A reintroduced innerHTML sink would reopen the XSS on this secret-handling page.
func TestPairingWebNoInnerHTMLSink(t *testing.T) {
	if strings.Contains(reversePairWebHTML, "innerHTML") {
		t.Fatal("pairing web must not use innerHTML (DOM-XSS sink); render dynamic status via textContent/createElement")
	}
}
