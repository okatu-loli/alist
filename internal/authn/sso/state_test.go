package sso

import (
	"testing"
	"time"
)

func TestStateStore_PutTake(t *testing.T) {
	s := NewStateStore()
	st := State{ProviderKey: "casdoor", Nonce: "n1", PKCEVerifier: "v1", Remember: true, LinkUserID: 7}
	s.Put("id1", st, time.Minute)

	got, ok := s.Take("id1")
	if !ok {
		t.Fatal("expected to find the state")
	}
	if got.ProviderKey != "casdoor" || got.Nonce != "n1" || got.PKCEVerifier != "v1" || !got.Remember || got.LinkUserID != 7 {
		t.Fatalf("round-tripped state mismatch: %+v", got)
	}
}

func TestStateStore_SingleUse(t *testing.T) {
	s := NewStateStore()
	s.Put("id1", State{Nonce: "n"}, time.Minute)

	if _, ok := s.Take("id1"); !ok {
		t.Fatal("first take should succeed")
	}
	if _, ok := s.Take("id1"); ok {
		t.Fatal("second take of same state must be rejected (replay)")
	}
	if s.Len() != 0 {
		t.Fatalf("store should be empty after take, len=%d", s.Len())
	}
}

func TestStateStore_Expiry(t *testing.T) {
	s := NewStateStore()
	base := time.Now()
	s.now = func() time.Time { return base }
	s.Put("id1", State{Nonce: "n"}, time.Second)

	s.now = func() time.Time { return base.Add(2 * time.Second) }
	if _, ok := s.Take("id1"); ok {
		t.Fatal("expired state must not be redeemable")
	}
	if s.Len() != 0 {
		t.Fatalf("expired entry should be evicted on take, len=%d", s.Len())
	}
}

func TestStateStore_Missing(t *testing.T) {
	s := NewStateStore()
	if _, ok := s.Take("nope"); ok {
		t.Fatal("unknown id should not be found")
	}
}

func TestGenerateState_Nonce_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		st, err := GenerateState()
		if err != nil {
			t.Fatalf("GenerateState: %v", err)
		}
		n, err := GenerateNonce()
		if err != nil {
			t.Fatalf("GenerateNonce: %v", err)
		}
		if st == "" || n == "" {
			t.Fatal("state/nonce must be non-empty")
		}
		if seen[st] {
			t.Fatalf("duplicate state %q", st)
		}
		seen[st] = true
	}
}

func TestGeneratePKCE_ChallengeMatchesVerifier(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatal("verifier and challenge must be non-empty")
	}
	if got := pkceChallenge(verifier); got != challenge {
		t.Fatalf("challenge must equal S256(verifier): got %q want %q", got, challenge)
	}
}

func TestPKCE_MismatchedVerifierRejected(t *testing.T) {
	_, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	otherVerifier, _, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if pkceChallenge(otherVerifier) == challenge {
		t.Fatal("a different verifier must not reproduce the challenge")
	}
}
