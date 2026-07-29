package passkey

import (
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestChallengeStore_PutTake(t *testing.T) {
	s := NewChallengeStore()
	sd := &webauthn.SessionData{Challenge: "abc"}
	s.Put("id1", sd, time.Minute)

	got, ok := s.Take("id1")
	if !ok {
		t.Fatal("expected to find the challenge")
	}
	if got.Challenge != "abc" {
		t.Fatalf("wrong session: %q", got.Challenge)
	}
}

func TestChallengeStore_SingleUse(t *testing.T) {
	s := NewChallengeStore()
	s.Put("id1", &webauthn.SessionData{Challenge: "abc"}, time.Minute)

	if _, ok := s.Take("id1"); !ok {
		t.Fatal("first take should succeed")
	}
	if _, ok := s.Take("id1"); ok {
		t.Fatal("second take of same challenge must be rejected (replay)")
	}
	if s.Len() != 0 {
		t.Fatalf("store should be empty after take, len=%d", s.Len())
	}
}

func TestChallengeStore_Expiry(t *testing.T) {
	s := NewChallengeStore()
	base := time.Now()
	s.now = func() time.Time { return base }
	s.Put("id1", &webauthn.SessionData{Challenge: "abc"}, time.Second)

	s.now = func() time.Time { return base.Add(2 * time.Second) }
	if _, ok := s.Take("id1"); ok {
		t.Fatal("expired challenge must not be redeemable")
	}
	if s.Len() != 0 {
		t.Fatalf("expired entry should be evicted on take, len=%d", s.Len())
	}
}

func TestChallengeStore_Missing(t *testing.T) {
	s := NewChallengeStore()
	if _, ok := s.Take("nope"); ok {
		t.Fatal("unknown id should not be found")
	}
}
