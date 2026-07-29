package agentsrv

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"github.com/AlistGo/alist-agent/proto"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/AlistGo/alist-agent/agent"
)

// DialAndPairAgent tests (design subproject F): the server-side pairing dialer
// drives the token server leg against a real in-process acceptor over a
// WebSocket; both sides key the SAME token, so the tags verify and the server
// learns the real agent key.

// newTestAcceptor, genPairKey and mustTokenRaw are local re-implementations of
// internal/agent's own unexported test scaffolding
// (pairing_acceptor_test.go's newTestAcceptor, pairing_handshake_test.go's
// genPairKey/mustTokenRaw): those are package-private to internal/agent and
// unreachable from here after the W1 server/agent split moved this test out,
// so this file rebuilds the same fixtures against internal/agent's exported
// API. Test-only scaffolding — no production behavior depends on it.
func newTestAcceptor(t *testing.T) (*agentpkg.PairingAcceptor, *agentpkg.Identity, string) {
	t.Helper()
	dir := t.TempDir()
	id, err := agentpkg.LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	acc := agentpkg.NewPairingAcceptor(id, dir, agentpkg.ClientConfig{Host: "http://example:5244"})
	acc.Token = mustTokenRaw(t)
	// Default the operator confirm gate to AUTO-CONFIRM so the existing happy-
	// path tests exercise the pin+go-live flow.
	acc.Confirm = func(string) bool { return true }
	return acc, id, dir
}

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

// TestDialAndPairAgentAgainstAcceptor stands up a real PairingAcceptor, dials it
// with DialAndPairAgent using the acceptor's token, and asserts the dialer
// returns the agent's actual public key + agentID + the server-key fingerprint.
func TestDialAndPairAgentAgainstAcceptor(t *testing.T) {
	acc, id, _ := newTestAcceptor(t)
	serverPub, _ := genPairKey(t)

	done := make(chan struct{})
	acc.OnPaired = func(_ agentpkg.ClientConfig, _ []byte, _ string) { close(done) }

	srv := httptest.NewServer(acc)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	agentPub, agentID, fingerprint, err := DialAndPairAgent(context.Background(), wsURL, serverPub, acc.Token)
	if err != nil {
		t.Fatalf("DialAndPairAgent: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("acceptor OnPaired never fired")
	}

	// The dialer received the REAL agent pubkey ...
	if !ed25519.PublicKey(agentPub).Equal(id.PubKey) {
		t.Fatal("dialer got the wrong agent pubkey")
	}
	// ... the agentID matches its fingerprint ...
	if agentID != proto.FingerprintPubKey(id.PubKey) {
		t.Fatalf("agentID mismatch: %q vs %q", agentID, proto.FingerprintPubKey(id.PubKey))
	}
	// ... and the returned display fingerprint is the server key's.
	if fingerprint != proto.ReversePairFingerprint(serverPub) {
		t.Fatalf("fingerprint mismatch: %q vs %q", fingerprint, proto.ReversePairFingerprint(serverPub))
	}
}

// A wrong token makes the dial fail (the acceptor's tagS verify rejects it) and
// returns a clean error — nothing is pinned.
func TestDialAndPairAgentWrongTokenFails(t *testing.T) {
	acc, _, _ := newTestAcceptor(t)
	serverPub, _ := genPairKey(t)

	srv := httptest.NewServer(acc)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	_, _, _, err := DialAndPairAgent(context.Background(), wsURL, serverPub, mustTokenRaw(t))
	if err == nil {
		t.Fatal("DialAndPairAgent must fail when the token does not match the acceptor's")
	}
}

// TestDialAndPairAgentUnreachable pins a clean error (and no panic) when the
// pairing endpoint cannot be reached.
func TestDialAndPairAgentUnreachable(t *testing.T) {
	serverPub, _ := genPairKey(t)
	_, _, _, err := DialAndPairAgent(context.Background(), "ws://127.0.0.1:1/agentpair", serverPub, mustTokenRaw(t))
	if err == nil {
		t.Fatal("DialAndPairAgent to an unreachable endpoint must return an error")
	}
}

// TestDialAndPairAgentRejectsBadInput pins the up-front guards (no dial attempted
// when the server identity or token is malformed).
func TestDialAndPairAgentRejectsBadInput(t *testing.T) {
	serverPub, _ := genPairKey(t)
	if _, _, _, err := DialAndPairAgent(context.Background(), "ws://127.0.0.1:1/agentpair", []byte("short"), mustTokenRaw(t)); err == nil {
		t.Fatal("a malformed server pubkey must be rejected before dialing")
	}
	if _, _, _, err := DialAndPairAgent(context.Background(), "ws://127.0.0.1:1/agentpair", serverPub, []byte("short-token")); err == nil {
		t.Fatal("a malformed token must be rejected before dialing")
	}
}
