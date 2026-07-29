package proto

import "testing"

// TestSasAlphabetIsStableAndUnbiased pins the two properties DeriveSAS's
// index math depends on: exactly 64 entries (so h[i]%64 has zero modulo
// bias over a byte's 256 values) and no duplicate emoji (a repeated symbol
// would make two DIFFERENT underlying bytes render identically, quietly
// shrinking the SAS's real distinguishing power below what its length
// suggests).
func TestSasAlphabetIsStableAndUnbiased(t *testing.T) {
	if len(sasAlphabet) != 64 {
		t.Fatalf("sasAlphabet must have exactly 64 entries (power-of-two, unbiased mod), got %d", len(sasAlphabet))
	}
	seen := make(map[string]bool, len(sasAlphabet))
	for _, e := range sasAlphabet {
		if e == "" {
			t.Fatal("sasAlphabet must not contain an empty entry")
		}
		if seen[e] {
			t.Fatalf("sasAlphabet contains a duplicate entry: %q", e)
		}
		seen[e] = true
	}
}

// TestDeriveSASIsDeterministic pins that the SAME (code, pubKey) always
// renders the SAME sequence — this is the property that lets the server and
// the agent, computing independently, ever agree at all.
func TestDeriveSASIsDeterministic(t *testing.T) {
	pub := []byte("0123456789abcdef0123456789abcdef")
	a := DeriveSAS("the-pairing-code", pub)
	b := DeriveSAS("the-pairing-code", pub)
	if len(a) != sasLength {
		t.Fatalf("expected %d emoji, got %d: %v", sasLength, len(a), a)
	}
	if !equalSAS(a, b) {
		t.Fatalf("DeriveSAS is not deterministic: %v vs %v", a, b)
	}
}

// TestDeriveSASEveryEmojiFromAlphabet pins that every rendered symbol
// actually comes from sasAlphabet — a caller must never see a stray value
// that couldn't have been rendered by the other side too.
func TestDeriveSASEveryEmojiFromAlphabet(t *testing.T) {
	valid := make(map[string]bool, len(sasAlphabet))
	for _, e := range sasAlphabet {
		valid[e] = true
	}
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i * 7)
	}
	sas := DeriveSAS("some-code", pub)
	for _, e := range sas {
		if !valid[e] {
			t.Fatalf("DeriveSAS rendered %q, which is not in sasAlphabet", e)
		}
	}
}

// TestDeriveSASSensitiveToPubKey pins the load-bearing MITM-detection
// property: changing even a single byte of the public key — exactly what a
// network-position attacker substituting the agent's real key for one they
// control would do — must change the rendered SAS. sha256's avalanche
// effect makes this true with overwhelming probability for any real key
// pair; this test pins it against a concrete fixture so a future change to
// the derivation can't silently weaken it without a test going red.
func TestDeriveSASSensitiveToPubKey(t *testing.T) {
	pubA := make([]byte, 32)
	pubB := make([]byte, 32)
	copy(pubB, pubA)
	pubB[0] ^= 0x01 // flip a single bit

	sasA := DeriveSAS("the-pairing-code", pubA)
	sasB := DeriveSAS("the-pairing-code", pubB)
	if equalSAS(sasA, sasB) {
		t.Fatalf("a 1-byte pubkey change must change the SAS (MITM pubkey-swap would go undetected), got identical %v", sasA)
	}
}

// TestDeriveSASSensitiveToCode pins that the code is bound into the SAS
// too, not just the pubkey — two pairing attempts using the SAME pubkey
// (e.g. an agent re-pairing) but different codes render differently, so a
// stale SAS from a previous attempt can never be mistaken for the current
// one's.
func TestDeriveSASSensitiveToCode(t *testing.T) {
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i)
	}
	sasA := DeriveSAS("code-one", pub)
	sasB := DeriveSAS("code-two", pub)
	if equalSAS(sasA, sasB) {
		t.Fatalf("a different pairing code must change the SAS, got identical %v", sasA)
	}
}

func equalSAS(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
