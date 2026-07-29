package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// TestFingerprintPubKeyDeterministicAndDistinct pins the two load-bearing
// properties of the AgentID derivation: the same public key always
// fingerprints to the same id (so a re-pair or a restart recognizes an
// already-known agent), and two different keys practically never collide.
func TestFingerprintPubKeyDeterministicAndDistinct(t *testing.T) {
	pub1, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub2, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	id1a := FingerprintPubKey(pub1)
	id1b := FingerprintPubKey(pub1)
	id2 := FingerprintPubKey(pub2)

	if id1a != id1b {
		t.Fatalf("fingerprint must be deterministic: %q vs %q", id1a, id1b)
	}
	if id1a == id2 {
		t.Fatalf("two distinct pubkeys must not fingerprint to the same id: %q", id1a)
	}
	if len(id1a) != 32 {
		t.Fatalf("fingerprint must be 32 hex chars (128 bits), got %d: %q", len(id1a), id1a)
	}
}

// TestHeartbeatMessageDeterministicAndFieldSensitive pins that the signed
// message construction is a pure, deterministic function of its inputs (so
// client and server, built from the same function, can never disagree) and
// that every field actually participates (changing any one of agentID/ts/
// nonce changes the message) — otherwise that field would not really be
// authenticated by the signature.
func TestHeartbeatMessageDeterministicAndFieldSensitive(t *testing.T) {
	base := HeartbeatMessage("agent-1", 1000, "nonce-a")
	again := HeartbeatMessage("agent-1", 1000, "nonce-a")
	if string(base) != string(again) {
		t.Fatalf("HeartbeatMessage must be deterministic: %q vs %q", base, again)
	}

	diffID := HeartbeatMessage("agent-2", 1000, "nonce-a")
	diffTs := HeartbeatMessage("agent-1", 1001, "nonce-a")
	diffNonce := HeartbeatMessage("agent-1", 1000, "nonce-b")

	for _, variant := range []struct {
		name string
		msg  []byte
	}{
		{"agentID", diffID},
		{"ts", diffTs},
		{"nonce", diffNonce},
	} {
		if string(variant.msg) == string(base) {
			t.Fatalf("changing %s must change the signed message", variant.name)
		}
	}
}
