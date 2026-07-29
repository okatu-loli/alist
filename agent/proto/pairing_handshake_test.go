package proto

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func genPairKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return pub, priv
}

func mustNonce(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, pairingNonceBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return b
}

// mustTokenRaw returns a fresh 32-byte raw reverse-pairing token (what both
// legs key their HMAC tags with).
func mustTokenRaw(t *testing.T) []byte {
	t.Helper()
	tok, err := GenerateReversePairToken()
	if err != nil {
		t.Fatalf("GenerateReversePairToken: %v", err)
	}
	raw, err := CanonicalizeReversePairToken(tok)
	if err != nil {
		t.Fatalf("CanonicalizeReversePairToken: %v", err)
	}
	return raw
}

// b32 returns a fixed 32-byte slice seeded from s (padded/truncated), for the
// DeriveReverseSAS unit-property tests that require exactly-32-byte inputs.
func b32(s string) []byte {
	b := make([]byte, 32)
	copy(b, s)
	return b
}

// pairResult captures both legs' outputs from a single handshake run over an
// in-memory pipe.
type pairResult struct {
	// server leg
	srvAgentPub []byte
	srvAgentID  string
	srvErr      error
	// agent leg
	agtServerPub []byte
	agtErr       error
}

// runPairing runs the server leg and the agent leg on the two ends of a net.Pipe
// concurrently, each keyed with the token it was handed (srvToken / agtToken —
// pass the same token for an honest run, different tokens for the wrong-token
// case), and returns both legs' results.
func runPairing(t *testing.T, agentPub, serverPub, srvToken, agtToken []byte) pairResult {
	t.Helper()
	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	var res pairResult
	done := make(chan struct{})
	go func() {
		res.agtServerPub, res.agtErr = RunAgentPairingHandshake(agentConn, agentPub, agtToken)
		close(done)
	}()
	res.srvAgentPub, res.srvAgentID, res.srvErr = RunServerPairingHandshake(serverConn, serverPub, srvToken)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent leg did not return")
	}
	return res
}

// ---------------------------------------------------------------------------
// Happy path: same token → both tags verify, both sides pin the peer key.
// ---------------------------------------------------------------------------

func TestReversePairingHappyPath(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	res := runPairing(t, agentPub, serverPub, token, token)

	if res.srvErr != nil {
		t.Fatalf("server leg failed: %v", res.srvErr)
	}
	if res.agtErr != nil {
		t.Fatalf("agent leg failed: %v", res.agtErr)
	}
	// Server pinned the agent's REAL public key ...
	if !ed25519.PublicKey(res.srvAgentPub).Equal(agentPub) {
		t.Fatalf("server pinned the wrong agent pubkey")
	}
	// ... and the agent pinned the server's REAL public key.
	if !ed25519.PublicKey(res.agtServerPub).Equal(serverPub) {
		t.Fatalf("agent pinned the wrong server pubkey")
	}
	// agentID matches FingerprintPubKey of the real agent key.
	if res.srvAgentID != FingerprintPubKey(agentPub) {
		t.Fatalf("agentID mismatch: got %q want %q", res.srvAgentID, FingerprintPubKey(agentPub))
	}
}

// ---------------------------------------------------------------------------
// SECURITY: wrong token — agent side rejects (tagS verify) and pins NOTHING.
// ---------------------------------------------------------------------------

// The agent holds T; the server holds T' != T. The agent's constant-time verify
// of the server's tagS FAILS, so the agent errors and pins nothing (returns a
// nil serverPub) — and, crucially, never sends M3, so the server (waiting for
// M3) also fails and pins nothing.
func TestReversePairingWrongTokenAgentRejectsAndPinsNothing(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	agtToken := mustTokenRaw(t)
	srvToken := mustTokenRaw(t) // different token

	res := runPairing(t, agentPub, serverPub, srvToken, agtToken)

	if res.agtErr == nil {
		t.Fatal("agent must reject a server tag computed under a different token")
	}
	if res.agtServerPub != nil {
		t.Fatal("agent must pin NOTHING on a failed tag verify")
	}
	// The server, having sent M2 and then waiting for M3 the agent never sends,
	// must also fail and pin nothing.
	if res.srvErr == nil {
		t.Fatal("server must fail when the agent aborts after a failed tag verify")
	}
	if res.srvAgentPub != nil {
		t.Fatal("server must pin NOTHING when no valid agent tag arrives")
	}
}

// ---------------------------------------------------------------------------
// SECURITY: wrong token — server side rejects a tagA computed under a wrong
// token, even from an agent that (maliciously) skips its own tagS check.
// ---------------------------------------------------------------------------

// A malicious agent that ignores tagS and answers with a tagA keyed by the WRONG
// token must be rejected by the honest server's constant-time tagA verify.
func TestReversePairingWrongTokenServerRejectsTagA(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	srvToken := mustTokenRaw(t)
	wrongToken := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	go func() {
		defer agentConn.Close()
		// M1: honest reveal.
		nonceA := mustNonce(t)
		if err := writePairingJSON(agentConn, PairAgentHello{
			Type:     pairAgentHelloType,
			AgentPub: base64.StdEncoding.EncodeToString(agentPub),
			NonceA:   base64.StdEncoding.EncodeToString(nonceA),
		}); err != nil {
			return
		}
		// M2: read the server hello (ignore tagS entirely).
		var sh PairServerHello
		if err := readPairingJSON(agentConn, &sh); err != nil {
			return
		}
		nonceS, _ := base64.StdEncoding.DecodeString(sh.NonceS)
		// M3: send a tagA keyed by the WRONG token over the real transcript.
		transcript, err := reversePairTranscript(serverPub, agentPub, nonceS, nonceA)
		if err != nil {
			return
		}
		badTagA := reversePairTag(wrongToken, roleAgent, transcript)
		_ = writePairingJSON(agentConn, PairAgentConfirm{
			Type: pairAgentConfirmType,
			TagA: base64.StdEncoding.EncodeToString(badTagA),
		})
	}()

	_, _, err := RunServerPairingHandshake(serverConn, serverPub, srvToken)
	if err == nil || !strings.Contains(err.Error(), "tag failed verification") {
		t.Fatalf("server must reject a tagA under the wrong token, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SECURITY: every transcript field is bound — flipping any one of serverPub,
// agentPub, nonceS, nonceA changes the tag, so a MITM swap is detectable.
// ---------------------------------------------------------------------------

func TestReversePairTagBindsEveryTranscriptField(t *testing.T) {
	token := mustTokenRaw(t)
	serverPub := mustNonce(t) // 32 bytes each; content is arbitrary for the tag
	agentPub := mustNonce(t)
	nonceS := mustNonce(t)
	nonceA := mustNonce(t)

	tagOf := func(sp, ap, ns, na []byte, role string) []byte {
		tr, err := reversePairTranscript(sp, ap, ns, na)
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		return reversePairTag(token, role, tr)
	}
	flip := func(b []byte) []byte {
		c := append([]byte(nil), b...)
		c[0] ^= 0x01
		return c
	}

	for _, role := range []string{roleServer, roleAgent} {
		base := tagOf(serverPub, agentPub, nonceS, nonceA, role)
		cases := map[string][]byte{
			"serverPub flipped": tagOf(flip(serverPub), agentPub, nonceS, nonceA, role),
			"agentPub flipped":  tagOf(serverPub, flip(agentPub), nonceS, nonceA, role),
			"nonceS flipped":    tagOf(serverPub, agentPub, flip(nonceS), nonceA, role),
			"nonceA flipped":    tagOf(serverPub, agentPub, nonceS, flip(nonceA), role),
		}
		for name, got := range cases {
			if hmac.Equal(got, base) {
				t.Errorf("role %s: %s did not change the tag — field not bound", role, name)
			}
		}
	}
}

// The two role tags MUST differ over an identical transcript (reflection block).
func TestReversePairTagRolesDiffer(t *testing.T) {
	token := mustTokenRaw(t)
	tr, err := reversePairTranscript(mustNonce(t), mustNonce(t), mustNonce(t), mustNonce(t))
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	tagS := reversePairTag(token, roleServer, tr)
	tagA := reversePairTag(token, roleAgent, tr)
	if hmac.Equal(tagS, tagA) {
		t.Fatal("tagS and tagA must differ (distinct role prefixes) to block reflection")
	}
}

// ---------------------------------------------------------------------------
// SECURITY: a tampered serverPub in M2 is rejected by the agent (handshake
// level) — proves the pubkey binding is enforced end to end, not just in the
// pure tag function.
// ---------------------------------------------------------------------------

func TestReversePairingAgentRejectsTamperedServerPub(t *testing.T) {
	agentPub, _ := genPairKey(t)
	realServerPub, _ := genPairKey(t)
	attackerPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	// A MITM relay that keeps the server's REAL tagS but swaps the serverPub in
	// M2 to the attacker's key. The agent computes wantTagS over the attacker key
	// → mismatch → reject.
	go func() {
		defer serverConn.Close()
		var m1 PairAgentHello
		if err := readPairingJSON(serverConn, &m1); err != nil {
			return
		}
		nonceA, _ := base64.StdEncoding.DecodeString(m1.NonceA)
		nonceS := mustNonce(t)
		transcript, err := reversePairTranscript(realServerPub, agentPub, nonceS, nonceA)
		if err != nil {
			return
		}
		realTagS := reversePairTag(token, roleServer, transcript)
		// Swap serverPub → attacker key, keep the real tagS.
		_ = writePairingJSON(serverConn, PairServerHello{
			Type:      pairServerHelloType,
			ServerPub: base64.StdEncoding.EncodeToString(attackerPub),
			NonceS:    base64.StdEncoding.EncodeToString(nonceS),
			TagS:      base64.StdEncoding.EncodeToString(realTagS),
		})
	}()

	serverPub, err := RunAgentPairingHandshake(agentConn, agentPub, token)
	if err == nil || !strings.Contains(err.Error(), "tag failed verification") {
		t.Fatalf("agent must reject a swapped serverPub, got: %v", err)
	}
	if serverPub != nil {
		t.Fatal("agent must pin NOTHING when the serverPub was tampered")
	}
}

// ---------------------------------------------------------------------------
// SECURITY: relay / cross-session — a valid tagS from one session's transcript
// presented in a session with a different nonce is rejected.
// ---------------------------------------------------------------------------

func TestReversePairingRejectsCrossSessionTag(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	// The "server" computes a tagS over a DIFFERENT transcript (a different
	// nonceA than the agent's live one — i.e. a tag lifted from another session)
	// and presents it against the live nonceS. The agent's full-transcript verify
	// must fail.
	go func() {
		defer serverConn.Close()
		var m1 PairAgentHello
		if err := readPairingJSON(serverConn, &m1); err != nil {
			return
		}
		nonceS := mustNonce(t)
		otherNonceA := mustNonce(t) // NOT the agent's live nonceA
		staleTranscript, err := reversePairTranscript(serverPub, agentPub, nonceS, otherNonceA)
		if err != nil {
			return
		}
		staleTagS := reversePairTag(token, roleServer, staleTranscript)
		_ = writePairingJSON(serverConn, PairServerHello{
			Type:      pairServerHelloType,
			ServerPub: base64.StdEncoding.EncodeToString(serverPub),
			NonceS:    base64.StdEncoding.EncodeToString(nonceS),
			TagS:      base64.StdEncoding.EncodeToString(staleTagS),
		})
	}()

	_, err := RunAgentPairingHandshake(agentConn, agentPub, token)
	if err == nil || !strings.Contains(err.Error(), "tag failed verification") {
		t.Fatalf("agent must reject a tag bound to a different session's transcript, got: %v", err)
	}
}

// SYMMETRIC to TestReversePairingRejectsCrossSessionTag but for the SERVER leg
// (folded F1-review MINOR item): a valid tagA computed under a DIFFERENT nonceS
// (a tag lifted from another session) presented against the server's live nonceS
// is rejected. Proves verify-before-pin holds on the server side too — the server
// pins the agent key ONLY after a full-transcript verify over ITS OWN fresh
// nonceS, so a relayed/cross-session tagA can never make it pin.
func TestReversePairingServerRejectsCrossSessionTag(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	// A fake AGENT: sends M1, reads M2 (learns the server's live nonceS), but
	// answers with a tagA computed over a DIFFERENT nonceS (i.e. a tag valid only
	// in another session). The server verifies tagA over its LIVE nonceS, so it
	// must fail and pin NOTHING.
	go func() {
		defer agentConn.Close()
		nonceA := mustNonce(t)
		if err := writePairingJSON(agentConn, PairAgentHello{
			Type:     pairAgentHelloType,
			AgentPub: base64.StdEncoding.EncodeToString(agentPub),
			NonceA:   base64.StdEncoding.EncodeToString(nonceA),
		}); err != nil {
			return
		}
		var m2 PairServerHello
		if err := readPairingJSON(agentConn, &m2); err != nil {
			return
		}
		otherNonceS := mustNonce(t) // NOT the server's live nonceS from m2
		staleTranscript, err := reversePairTranscript(serverPub, agentPub, otherNonceS, nonceA)
		if err != nil {
			return
		}
		staleTagA := reversePairTag(token, roleAgent, staleTranscript)
		_ = writePairingJSON(agentConn, PairAgentConfirm{
			Type: pairAgentConfirmType,
			TagA: base64.StdEncoding.EncodeToString(staleTagA),
		})
	}()

	gotAgentPub, _, err := RunServerPairingHandshake(serverConn, serverPub, token)
	if err == nil || !strings.Contains(err.Error(), "tag failed verification") {
		t.Fatalf("server must reject a tagA bound to a different session's nonceS, got: %v", err)
	}
	if gotAgentPub != nil {
		t.Fatal("server must pin NOTHING when tagA is cross-session")
	}
}

// ---------------------------------------------------------------------------
// SECURITY: a tag over a PARTIAL transcript (the naive "fit tagS into M2 before
// nonceA is known" mistake) is NOT accepted by the honest full-transcript
// verifier — and the transcript builder itself refuses a short field.
// ---------------------------------------------------------------------------

func TestReversePairingPartialTranscriptRejected(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	// Part 1: the transcript builder is a hard error on a non-32-byte field, so a
	// partial/short transcript can never be assembled through the honest path.
	if _, err := reversePairTranscript(serverPub, agentPub, mustNonce(t), []byte("short")); err == nil {
		t.Fatal("reversePairTranscript must reject a short field (partial transcript)")
	}

	// Part 2: a "server" that computes tagS over a PARTIAL preimage OMITTING
	// agentPub + nonceA (domain ‖ serverPub ‖ nonceS only) is rejected by the
	// honest agent, which always verifies over the FULL transcript.
	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	go func() {
		defer serverConn.Close()
		var m1 PairAgentHello
		if err := readPairingJSON(serverConn, &m1); err != nil {
			return
		}
		nonceS := mustNonce(t)
		// PARTIAL preimage: domain ‖ serverPub ‖ nonceS (agentPub + nonceA omitted).
		partial := append([]byte(reversePairTranscriptDomain), serverPub...)
		partial = append(partial, nonceS...)
		mac := hmac.New(sha256.New, token)
		mac.Write([]byte(reversePairServerPrefix))
		mac.Write(partial)
		partialTag := mac.Sum(nil)
		_ = writePairingJSON(serverConn, PairServerHello{
			Type:      pairServerHelloType,
			ServerPub: base64.StdEncoding.EncodeToString(serverPub),
			NonceS:    base64.StdEncoding.EncodeToString(nonceS),
			TagS:      base64.StdEncoding.EncodeToString(partialTag),
		})
	}()

	_, err := RunAgentPairingHandshake(agentConn, agentPub, token)
	if err == nil || !strings.Contains(err.Error(), "tag failed verification") {
		t.Fatalf("agent must reject a tag over a partial transcript, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SECURITY: the verify path uses constant-time comparison — a source-level
// regression guard so a future edit cannot reintroduce == / bytes.Equal on tags.
// ---------------------------------------------------------------------------

func TestReversePairingUsesConstantTimeCompare(t *testing.T) {
	src, err := os.ReadFile("pairing_handshake.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	if strings.Count(s, "subtle.ConstantTimeCompare") < 2 {
		t.Fatal("both verify paths (agent tagS, server tagA) must use subtle.ConstantTimeCompare")
	}
	// The tag comparison must NOT use a non-constant-time equality.
	for _, bad := range []string{"bytes.Equal(gotTag", "bytes.Equal(want", "gotTagS ==", "gotTagA ==", "wantTagS ==", "wantTagA =="} {
		if strings.Contains(s, bad) {
			t.Fatalf("verify path must not use a variable-time equality: found %q", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Token generation + canonicalization
// ---------------------------------------------------------------------------

func TestGenerateReversePairTokenIs32BytesAndUnique(t *testing.T) {
	a, err := GenerateReversePairToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	b, err := GenerateReversePairToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if a == b {
		t.Fatal("two GenerateReversePairToken calls must differ")
	}
	raw, err := CanonicalizeReversePairToken(a)
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	if len(raw) != reversePairTokenBytes {
		t.Fatalf("token must decode to %d bytes, got %d", reversePairTokenBytes, len(raw))
	}
}

func TestCanonicalizeReversePairTokenTolerantAndStrict(t *testing.T) {
	tok, err := GenerateReversePairToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	want, err := CanonicalizeReversePairToken(tok)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	// Cosmetic variants must all canonicalize to the SAME 32 bytes.
	variants := map[string]string{
		"lower-case":          strings.ToLower(tok),
		"surrounding spaces":  "   " + tok + "  ",
		"internal whitespace": tok[:8] + " " + tok[8:16] + "\t" + tok[16:] + "\n",
		"trailing = padding":  tok + "===",
	}
	for name, v := range variants {
		got, err := CanonicalizeReversePairToken(v)
		if err != nil {
			t.Fatalf("%s: canonicalize err: %v", name, err)
		}
		if !hmac.Equal(got, want) {
			t.Fatalf("%s: canonicalized to different bytes", name)
		}
	}

	// A token that does NOT decode to exactly 32 bytes is rejected.
	short := reversePairTokenEncoding.EncodeToString([]byte("only-a-few-bytes")) // 16 bytes
	if _, err := CanonicalizeReversePairToken(short); err == nil {
		t.Fatal("a token decoding to != 32 bytes must be rejected")
	}
	if _, err := CanonicalizeReversePairToken("!!!not base32!!!"); err == nil {
		t.Fatal("a non-base32 token must be rejected")
	}
}

// A token that canonicalizes with cosmetic noise still drives a SUCCESSFUL
// handshake end to end (case/padding/whitespace do not break verification).
func TestReversePairingCanonicalizedTokenVerifies(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	tok, err := GenerateReversePairToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	srvRaw, err := CanonicalizeReversePairToken(tok) // server: clean copy
	if err != nil {
		t.Fatalf("canon srv: %v", err)
	}
	agtRaw, err := CanonicalizeReversePairToken("  " + strings.ToLower(tok) + " ==") // agent: noisy paste
	if err != nil {
		t.Fatalf("canon agt: %v", err)
	}
	res := runPairing(t, agentPub, serverPub, srvRaw, agtRaw)
	if res.srvErr != nil || res.agtErr != nil {
		t.Fatalf("cosmetically-different-but-equal tokens must pair: srv=%v agt=%v", res.srvErr, res.agtErr)
	}
	if !ed25519.PublicKey(res.agtServerPub).Equal(serverPub) {
		t.Fatal("agent must pin the real server key")
	}
}

// ---------------------------------------------------------------------------
// Token boundary: a missing / wrong-length raw token fails closed on both legs.
// ---------------------------------------------------------------------------

func TestReversePairingRejectsBadToken(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	if _, err := RunAgentPairingHandshake(agentConn, agentPub, nil); err == nil {
		t.Fatal("agent leg must reject a nil token")
	}
	if _, err := RunAgentPairingHandshake(agentConn, agentPub, make([]byte, 16)); err == nil {
		t.Fatal("agent leg must reject a short token")
	}
	if _, _, err := RunServerPairingHandshake(serverConn, serverPub, make([]byte, 31)); err == nil {
		t.Fatal("server leg must reject a wrong-length token")
	}
}

// ---------------------------------------------------------------------------
// Framing / malformed input bounds (retained from SP-E, adapted to the flow).
// ---------------------------------------------------------------------------

// The agent rejects an oversized server-hello frame on the header alone.
func TestReversePairingRejectsOversizedFrame(t *testing.T) {
	agentPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	go func() {
		defer serverConn.Close()
		var m1 PairAgentHello
		if err := readPairingJSON(serverConn, &m1); err != nil {
			return
		}
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], maxPairingHandshakeBytes+1)
		_, _ = serverConn.Write(hdr[:])
	}()

	_, err := RunAgentPairingHandshake(agentConn, agentPub, token)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected oversized-frame rejection, got: %v", err)
	}
}

// The server rejects an oversized M1 frame on the header alone.
func TestReversePairingServerRejectsOversizedM1(t *testing.T) {
	serverPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	go func() {
		defer agentConn.Close()
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], maxPairingHandshakeBytes+1)
		_, _ = agentConn.Write(hdr[:])
	}()

	_, _, err := RunServerPairingHandshake(serverConn, serverPub, token)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected oversized-M1 rejection, got: %v", err)
	}
}

// The server rejects an M1 carrying a too-short "public key".
func TestReversePairingServerRejectsShortAgentPub(t *testing.T) {
	serverPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	go func() {
		defer agentConn.Close()
		_ = writePairingJSON(agentConn, PairAgentHello{
			Type:     pairAgentHelloType,
			AgentPub: base64.StdEncoding.EncodeToString([]byte("way-too-short")),
			NonceA:   base64.StdEncoding.EncodeToString(mustNonce(t)),
		})
	}()

	_, _, err := RunServerPairingHandshake(serverConn, serverPub, token)
	if err == nil || !strings.Contains(err.Error(), "wrong length") {
		t.Fatalf("expected short-pubkey rejection, got: %v", err)
	}
}

// The agent rejects an M2 carrying the wrong Type tag.
func TestReversePairingAgentRejectsWrongType(t *testing.T) {
	agentPub, _ := genPairKey(t)
	serverPub, _ := genPairKey(t)
	token := mustTokenRaw(t)

	serverConn, agentConn := net.Pipe()
	defer serverConn.Close()
	defer agentConn.Close()

	go func() {
		defer serverConn.Close()
		var m1 PairAgentHello
		if err := readPairingJSON(serverConn, &m1); err != nil {
			return
		}
		_ = writePairingJSON(serverConn, PairServerHello{
			Type:      "not_the_right_type",
			ServerPub: base64.StdEncoding.EncodeToString(serverPub),
			NonceS:    base64.StdEncoding.EncodeToString(mustNonce(t)),
			TagS:      base64.StdEncoding.EncodeToString(mustNonce(t)),
		})
	}()

	_, err := RunAgentPairingHandshake(agentConn, agentPub, token)
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("expected wrong-type rejection, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DeriveReverseSAS unit properties (function RETAINED, no longer the reverse
// auth mechanism, but kept and still covered).
// ---------------------------------------------------------------------------

func mustReverseSAS(t *testing.T, serverPub, agentPub, nonceS, nonceA []byte) string {
	t.Helper()
	s, err := DeriveReverseSAS(serverPub, agentPub, nonceS, nonceA)
	if err != nil {
		t.Fatalf("DeriveReverseSAS: %v", err)
	}
	return s
}

func TestDeriveReverseSASDeterministic(t *testing.T) {
	sp, ap := b32("server-pub"), b32("agent-pub")
	ns, na := b32("nonce-s"), b32("nonce-a")
	a := mustReverseSAS(t, sp, ap, ns, na)
	b := mustReverseSAS(t, sp, ap, ns, na)
	if a != b {
		t.Fatalf("non-deterministic: %q != %q", a, b)
	}
	if len(strings.Fields(a)) != sasLength {
		t.Fatalf("want %d emoji, got %q", sasLength, a)
	}
}

func TestDeriveReverseSASBindsEveryInput(t *testing.T) {
	sp := b32("server-pub")
	ap := b32("agent-pub")
	ns := b32("nonce-s")
	na := b32("nonce-a")
	base := mustReverseSAS(t, sp, ap, ns, na)

	cases := map[string]string{
		"serverPub changed": mustReverseSAS(t, b32("server-pub-ZZ"), ap, ns, na),
		"agentPub changed":  mustReverseSAS(t, sp, b32("agent-pub-ZZ"), ns, na),
		"nonceS changed":    mustReverseSAS(t, sp, ap, b32("nonce-s-ZZ"), na),
		"nonceA changed":    mustReverseSAS(t, sp, ap, ns, b32("nonce-a-ZZ")),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("%s: SAS did not change (input not bound)", name)
		}
	}
}

func TestDeriveReverseSASDomainSeparated(t *testing.T) {
	sp := b32("overlap-server")
	ap := b32("overlap-agent-key")
	ns := b32("ns")
	na := b32("na")
	reverse := mustReverseSAS(t, sp, ap, ns, na)
	outbound := strings.Join(DeriveSAS(string(sp), ap), " ")
	if reverse == outbound {
		t.Fatalf("reverse SAS collides with outbound DeriveSAS (missing domain separation): %q", reverse)
	}
}

func TestDeriveReverseSASRejectsShortInputs(t *testing.T) {
	good := mustNonce(t) // 32 bytes
	cases := map[string][4][]byte{
		"short serverPub": {[]byte("short"), good, good, good},
		"short agentPub":  {good, []byte("short"), good, good},
		"short nonceS":    {good, good, []byte("short"), good},
		"short nonceA":    {good, good, good, []byte("short")},
		"empty":           {{}, {}, {}, {}},
	}
	for name, in := range cases {
		if _, err := DeriveReverseSAS(in[0], in[1], in[2], in[3]); err == nil {
			t.Errorf("%s: expected length-guard error, got nil", name)
		}
	}
}
