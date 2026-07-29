package agentsrv

import (
	"testing"
	"time"
)

// TestStartMintsDistinctHighEntropyCodesWith15MinTTL pins the two load-
// bearing properties of a freshly minted session: the code is long enough to
// be brute-force-resistant and never repeats across calls, and the expiry is
// (approximately) 15 minutes out.
func TestStartMintsDistinctHighEntropyCodesWith15MinTTL(t *testing.T) {
	s := NewPairingStore()

	code1, exp1 := s.Start()
	code2, exp2 := s.Start()

	if len(code1) < 32 || len(code2) < 32 {
		t.Fatalf("pairing codes must be high-entropy (>=32 chars), got %q (%d) and %q (%d)",
			code1, len(code1), code2, len(code2))
	}
	if code1 == code2 {
		t.Fatalf("two Start() calls minted the same code: %q", code1)
	}

	wantExp := time.Now().Add(ttl)
	if d := wantExp.Sub(exp1); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("expiresAt not ~15min out: got %v, want ~%v", exp1, wantExp)
	}
	if d := wantExp.Sub(exp2); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("expiresAt not ~15min out: got %v, want ~%v", exp2, wantExp)
	}
}

// TestGetReturnsPendingSession pins that a just-minted session is retrievable
// and starts life as StatePending (no channel/SAS confirmation exists yet
// this wave, so nothing ever advances it past pending).
func TestGetReturnsPendingSession(t *testing.T) {
	s := NewPairingStore()
	code, expiresAt := s.Start()

	sess, ok := s.Get(code)
	if !ok {
		t.Fatal("Get must find a session it just Start()ed")
	}
	if sess.Code != code {
		t.Errorf("session code mismatch: got %q, want %q", sess.Code, code)
	}
	if sess.State != StatePending {
		t.Errorf("freshly started session must be StatePending, got %q", sess.State)
	}
	if !sess.ExpiresAt.Equal(expiresAt) {
		t.Errorf("session expiry mismatch: got %v, want %v", sess.ExpiresAt, expiresAt)
	}
}

// TestGetUnknownCodeNotFound pins the never-existed shape: no panic, no zero-
// value session mistaken for real.
func TestGetUnknownCodeNotFound(t *testing.T) {
	s := NewPairingStore()
	if _, ok := s.Get("this-code-was-never-minted"); ok {
		t.Fatal("Get on an unknown code must report not-ok")
	}
}

// TestGetExpiredSessionNotFoundAndEvicted pins expiry: a session past its
// ExpiresAt is reported not-ok by Get (never silently treated as still
// pending), and the lazy sweep actually removes it from the store rather
// than leaking it forever.
func TestGetExpiredSessionNotFoundAndEvicted(t *testing.T) {
	s := NewPairingStore()
	code, _ := s.Start()

	// Reach past the mutex to backdate the session's expiry — the store has
	// no clock injection seam (deliberately simple for this wave), so the
	// test manipulates the map directly under the same lock discipline.
	s.mu.Lock()
	s.sessions[code].ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()

	if _, ok := s.Get(code); ok {
		t.Fatal("Get on an expired session must report not-ok")
	}

	s.mu.Lock()
	_, stillThere := s.sessions[code]
	s.mu.Unlock()
	if stillThere {
		t.Fatal("Get must evict the expired session from the store, not just hide it")
	}
}

// TestPackageLevelSingletonRoundTrips pins that the package-level Start/Get
// convenience functions (the ones the HTTP handler actually calls) work end
// to end against the shared defaultStore.
func TestPackageLevelSingletonRoundTrips(t *testing.T) {
	code, _ := Start()
	if _, ok := Get(code); !ok {
		t.Fatal("package-level Get must find a session Start() just minted")
	}
}

// TestCompletePendingSessionTransitionsToConfirmed pins the happy path: a
// freshly started, still-pending code completes once and its State reads
// back as StateConfirmed through Get.
func TestCompletePendingSessionTransitionsToConfirmed(t *testing.T) {
	s := NewPairingStore()
	code, _ := s.Start()

	sess, ok := s.Complete(code)
	if !ok {
		t.Fatal("Complete must succeed for a fresh pending session")
	}
	if sess.State != StateConfirmed {
		t.Fatalf("returned session state = %q, want %q", sess.State, StateConfirmed)
	}

	// Get no longer reports this code as available for a NEW completion
	// attempt to distinguish from "pending" — but the store-internal state
	// (reached the same way TestGetExpiredSessionNotFoundAndEvicted does)
	// must actually read StateConfirmed, not have silently reverted.
	s.mu.Lock()
	got := s.sessions[code].State
	s.mu.Unlock()
	if got != StateConfirmed {
		t.Fatalf("stored session state = %q, want %q", got, StateConfirmed)
	}
}

// TestCompleteIsSingleUse pins the core security property: a second
// Complete call for the same code — even though the code has not expired
// and clearly still exists — must fail. This is what makes a pairing code
// single-use.
func TestCompleteIsSingleUse(t *testing.T) {
	s := NewPairingStore()
	code, _ := s.Start()

	if _, ok := s.Complete(code); !ok {
		t.Fatal("first Complete must succeed")
	}
	if _, ok := s.Complete(code); ok {
		t.Fatal("second Complete for the same code must fail (single-use)")
	}
}

// TestCompleteUnknownAndExpiredCodesFailUniformly pins the no-oracle
// property demanded by the design: Complete's boolean result for an
// unknown code and an expired code must be indistinguishable (both just
// "not ok") so a guesser cannot use the response to learn which is true.
func TestCompleteUnknownAndExpiredCodesFailUniformly(t *testing.T) {
	s := NewPairingStore()

	if _, ok := s.Complete("never-minted-code"); ok {
		t.Fatal("Complete on an unknown code must report not-ok")
	}

	code, _ := s.Start()
	s.mu.Lock()
	s.sessions[code].ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if _, ok := s.Complete(code); ok {
		t.Fatal("Complete on an expired code must report not-ok")
	}
}

// TestPackageLevelCompleteRoundTrips pins that the package-level Complete
// convenience function (the one the HTTP handler actually calls) reaches
// the shared defaultStore.
func TestPackageLevelCompleteRoundTrips(t *testing.T) {
	code, _ := Start()
	if _, ok := Complete(code); !ok {
		t.Fatal("package-level Complete must succeed for a code Start() just minted")
	}
	if _, ok := Complete(code); ok {
		t.Fatal("package-level Complete must be single-use too")
	}
}
