package channel

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"math"
	"testing"
)

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, aeadKeyBytes)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

// The two directions' nonce spaces are PROVABLY disjoint: the direction tag in
// the top byte differs, so no server→agent nonce can ever equal an agent→server
// nonce, for ANY pair of counters. This is the load-bearing guarantee that one
// key + two directions never reuses a nonce.
func TestNonceDirectionSeparation(t *testing.T) {
	if dirServerToAgent == dirAgentToServer {
		t.Fatal("direction tags must differ")
	}
	// Spot-check a spread of counters, including where the counters coincide
	// (which without the tag would collide).
	counters := []uint64{0, 1, 2, 255, 256, 1 << 20, math.MaxUint64 - 1, math.MaxUint64}
	for _, cs := range counters {
		for _, ca := range counters {
			ns := makeNonce(dirServerToAgent, cs)
			na := makeNonce(dirAgentToServer, ca)
			if bytes.Equal(ns, na) {
				t.Fatalf("nonce collision across directions: s@%d == a@%d", cs, ca)
			}
			if ns[0] != dirServerToAgent || na[0] != dirAgentToServer {
				t.Fatal("direction tag not in the top nonce byte")
			}
		}
	}
	if len(makeNonce(dirServerToAgent, 0)) != aeadNonceBytes {
		t.Fatalf("nonce must be %d bytes", aeadNonceBytes)
	}
}

// Within one direction the counter makes every nonce unique and monotonically
// increasing, so no nonce repeats.
func TestNonceCounterMonotonicUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := uint64(0); i < 1000; i++ {
		n := string(makeNonce(dirServerToAgent, i))
		if seen[n] {
			t.Fatalf("duplicate nonce at counter %d", i)
		}
		seen[n] = true
	}
}

// A frame sealed by one side opens on the other and round-trips the bytes.
func TestSealOpenRoundTrip(t *testing.T) {
	key := randKey(t)
	s, err := newFrameSealer(key, "s")
	if err != nil {
		t.Fatalf("sealer s: %v", err)
	}
	a, err := newFrameSealer(key, "a")
	if err != nil {
		t.Fatalf("sealer a: %v", err)
	}
	msgs := [][]byte{[]byte(`{"type":"req","id":"s-1"}`), []byte("second frame"), []byte("")}
	for i, m := range msgs {
		sealed, err := s.seal(m)
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		if bytes.Contains(sealed, m) && len(m) > 0 {
			t.Fatalf("sealed frame %d leaks plaintext", i)
		}
		got, err := a.open(sealed)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if !bytes.Equal(got, m) {
			t.Fatalf("round-trip %d mismatch: %q != %q", i, got, m)
		}
	}
}

// A single bit-flip anywhere in the ciphertext fails the AEAD tag → open errors
// (tamper detected). No plaintext is returned.
func TestBitFlippedCiphertextFailsOpen(t *testing.T) {
	key := randKey(t)
	s, _ := newFrameSealer(key, "s")
	a, _ := newFrameSealer(key, "a")
	sealed, err := s.seal([]byte(`{"type":"push","method":"x"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for _, pos := range []int{0, len(sealed) / 2, len(sealed) - 1} {
		tampered := append([]byte(nil), sealed...)
		tampered[pos] ^= 0x01
		if _, err := a.open(tampered); err == nil {
			t.Fatalf("open must fail on a bit-flip at byte %d", pos)
		}
	}
}

// A frame sealed under a DIFFERENT (wrong/guessed) key — an injected frame — is
// rejected: the receiver opens under its own key and the tag fails.
func TestInjectedFrameWrongKeyFailsOpen(t *testing.T) {
	realKey := randKey(t)
	wrongKey := randKey(t)
	attacker, _ := newFrameSealer(wrongKey, "s")
	victim, _ := newFrameSealer(realKey, "a")
	injected, err := attacker.seal([]byte(`{"type":"req","id":"evil"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := victim.open(injected); err == nil {
		t.Fatal("a frame sealed under the wrong key must fail to open")
	}
}

// A REPLAYED earlier frame is rejected: after the recv counter advances, the
// same ciphertext is opened against a DIFFERENT (later) nonce, so the tag fails.
func TestReplayedFrameFailsOpen(t *testing.T) {
	key := randKey(t)
	s, _ := newFrameSealer(key, "s")
	a, _ := newFrameSealer(key, "a")

	f0, _ := s.seal([]byte("frame-0"))
	f1, _ := s.seal([]byte("frame-1"))

	if _, err := a.open(f0); err != nil {
		t.Fatalf("open f0: %v", err)
	}
	if _, err := a.open(f1); err != nil {
		t.Fatalf("open f1: %v", err)
	}
	// Re-send f0: the recv counter is now at 2, so f0 (nonce 0) no longer
	// matches — a replay is refused.
	if _, err := a.open(f0); err == nil {
		t.Fatal("a replayed frame must fail to open (nonce counter advanced)")
	}
	// An out-of-order (reordered) delivery likewise fails.
	s2, _ := newFrameSealer(key, "s")
	a2, _ := newFrameSealer(key, "a")
	g0, _ := s2.seal([]byte("g0"))
	g1, _ := s2.seal([]byte("g1"))
	if _, err := a2.open(g1); err == nil {
		t.Fatal("an out-of-order frame must fail to open")
	}
	_ = g0
}

// Overflow guard: once a direction's counter reaches its maximum, seal and open
// both abort rather than wrap and reuse a nonce.
func TestNonceCounterOverflowGuard(t *testing.T) {
	key := randKey(t)
	s, _ := newFrameSealer(key, "s")
	s.sendCtr = math.MaxUint64
	if _, err := s.seal([]byte("x")); err == nil {
		t.Fatal("seal must abort when the send counter would overflow")
	}

	a, _ := newFrameSealer(key, "a")
	a.recvCtr = math.MaxUint64
	if _, err := a.open([]byte("whatever")); err == nil {
		t.Fatal("open must abort when the recv counter would overflow")
	}
}

// Both sides of an X25519 exchange derive the SAME session key, and it is
// exactly aeadKeyBytes long.
func TestDeriveSessionKeyAgrees(t *testing.T) {
	serverEph, _ := generateEphemeral()
	agentEph, _ := generateEphemeral()
	nonceS := base64.StdEncoding.EncodeToString([]byte("server-nonce-32-bytes-0123456789"))
	nonceA := base64.StdEncoding.EncodeToString([]byte("agent-nonce-32-bytes-01234567890"))

	serverPeer, err := parseEphemeral(ephemeralPublicB64(agentEph))
	if err != nil {
		t.Fatalf("parse agent eph: %v", err)
	}
	agentPeer, err := parseEphemeral(ephemeralPublicB64(serverEph))
	if err != nil {
		t.Fatalf("parse server eph: %v", err)
	}

	kServer, err := deriveSessionKey(serverEph, serverPeer, nonceS, nonceA)
	if err != nil {
		t.Fatalf("server derive: %v", err)
	}
	kAgent, err := deriveSessionKey(agentEph, agentPeer, nonceS, nonceA)
	if err != nil {
		t.Fatalf("agent derive: %v", err)
	}
	if len(kServer) != aeadKeyBytes {
		t.Fatalf("key must be %d bytes, got %d", aeadKeyBytes, len(kServer))
	}
	if !bytes.Equal(kServer, kAgent) {
		t.Fatal("both sides must derive the same session key")
	}

	// Same ephemerals, DIFFERENT nonce salt → independent key (the salt binds the
	// key to a specific handshake).
	otherNonceA := base64.StdEncoding.EncodeToString([]byte("other-nonce-32-bytes-01234567890"))
	kOther, err := deriveSessionKey(serverEph, serverPeer, nonceS, otherNonceA)
	if err != nil {
		t.Fatalf("derive other: %v", err)
	}
	if bytes.Equal(kServer, kOther) {
		t.Fatal("a different nonce salt must yield a different key")
	}
}

// A zero/low-order peer point (small-subgroup attack forcing an all-zero shared
// secret) is rejected: crypto/ecdh's X25519 ECDH returns an error and we fail
// closed (no key).
func TestDeriveSessionKeyRejectsLowOrderPoint(t *testing.T) {
	ownEph, _ := generateEphemeral()
	// The all-zero X25519 point: NewPublicKey accepts the length, but ECDH
	// yields the all-zero shared secret and returns an error.
	zeroPub, err := ecdh.X25519().NewPublicKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewPublicKey(zero): %v", err)
	}
	nonceS := base64.StdEncoding.EncodeToString([]byte("server-nonce-32-bytes-0123456789"))
	nonceA := base64.StdEncoding.EncodeToString([]byte("agent-nonce-32-bytes-01234567890"))
	if _, err := deriveSessionKey(ownEph, zeroPub, nonceS, nonceA); err == nil {
		t.Fatal("deriveSessionKey must reject a zero/low-order shared secret")
	}
}

// A malformed peer ephemeral (wrong length) is rejected at parse time.
func TestParseEphemeralRejectsMalformed(t *testing.T) {
	if _, err := parseEphemeral("not-base64!!!"); err == nil {
		t.Fatal("malformed base64 must be rejected")
	}
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := parseEphemeral(short); err == nil {
		t.Fatal("a wrong-length X25519 point must be rejected")
	}
}
