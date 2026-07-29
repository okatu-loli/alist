package utils

import (
	"strings"
	"testing"
)

// TestHashTokenIrreversible: the stored value must not contain the plain
// token as a substring — a naive "salt+token" concatenation without hashing
// would leak it.
func TestHashTokenIrreversible(t *testing.T) {
	token := "alist-super-secret-token-value"
	stored, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken error: %v", err)
	}
	if strings.Contains(stored, token) {
		t.Fatalf("stored value must not contain the plaintext token, got %q", stored)
	}
}

// TestVerifyTokenCorrectAndIncorrect: verification must accept the exact
// token that was hashed and reject anything else.
func TestVerifyTokenCorrectAndIncorrect(t *testing.T) {
	token := "alist-correct-token"
	stored, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken error: %v", err)
	}
	if !VerifyToken(stored, token) {
		t.Fatalf("VerifyToken must accept the token that produced the stored hash")
	}
	if VerifyToken(stored, "alist-wrong-token") {
		t.Fatalf("VerifyToken must reject a different token")
	}
	if VerifyToken(stored, "") {
		t.Fatalf("VerifyToken must reject an empty token")
	}
}

// TestHashTokenSaltUnique: hashing the same token twice must produce
// different stored values (random salt per call), and each must still
// independently verify.
func TestHashTokenSaltUnique(t *testing.T) {
	token := "alist-same-token-twice"
	stored1, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken error: %v", err)
	}
	stored2, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken error: %v", err)
	}
	if stored1 == stored2 {
		t.Fatalf("two HashToken calls for the same token must yield different salts/output, got identical %q", stored1)
	}
	if !VerifyToken(stored1, token) || !VerifyToken(stored2, token) {
		t.Fatalf("both independently-salted hashes must verify against the original token")
	}
}

// TestVerifyTokenMalformedStored: garbage or empty stored values must never
// match — VerifyToken degrades to false rather than panicking or erroring.
func TestVerifyTokenMalformedStored(t *testing.T) {
	cases := []string{
		"",
		"no-dollar-separator",
		"$emptysalt",
		"zz$aabbcc", // salt not valid hex
		"aabbcc$zz", // digest not valid hex
		"aabbcc$",   // empty digest
	}
	for _, stored := range cases {
		if VerifyToken(stored, "anything") {
			t.Fatalf("VerifyToken(%q, ...) must be false for malformed stored value", stored)
		}
	}
}

// TestVerifyTokenPlaintextStoredNeverMatches: a plaintext-mode stored value
// (the raw token, no "$" salt separator in general) must not accidentally
// verify via the hash path — callers must pick the branch (auth.go does, by
// conf.TokenPlaintext) rather than rely on VerifyToken to auto-detect mode.
func TestVerifyTokenPlaintextStoredNeverMatches(t *testing.T) {
	token := "alist-plaintext-token-no-dollar"
	if VerifyToken(token, token) {
		t.Fatalf("a plaintext stored value without the salt$digest shape must not verify")
	}
}
