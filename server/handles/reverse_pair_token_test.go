package handles

import (
	"github.com/AlistGo/alist-agent/proto"
	"sync"
	"testing"
	"time"
)

// Server-side one-time reverse-pairing token store tests (design F, wave F3).
// These pin the lifecycle invariants: mint→lookup works, TTL expiry kills a
// lookup, single-use (burn) makes the second lookup fail, and concurrent access
// is race-free.

// newTestTokenStore builds a store with the real generator but an INJECTABLE
// clock, so TTL expiry can be exercised deterministically by advancing "now"
// rather than sleeping.
func newTestTokenStore(ttl time.Duration) (*reversePairTokenStore, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return &reversePairTokenStore{
		ttl: ttl,
		now: clk.Now,
		gen: proto.GenerateReversePairToken,
		m:   make(map[string]reversePairTokenEntry),
	}, clk
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestReversePairTokenStoreMintLookup pins the happy path: a minted token is
// looked up by its id and decodes to the fixed 32 raw bytes, and the expiry is
// one TTL out.
func TestReversePairTokenStoreMintLookup(t *testing.T) {
	s, _ := newTestTokenStore(10 * time.Minute)

	token, id, exp, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if token == "" || id == "" {
		t.Fatalf("mint must return a non-empty token and id, got token=%q id=%q", token, id)
	}
	if !exp.Equal(s.now().Add(10 * time.Minute)) {
		t.Fatalf("expiry = %v, want now+ttl", exp)
	}

	got, ok := s.lookup(id)
	if !ok {
		t.Fatal("lookup of a freshly minted id must succeed")
	}
	if got != token {
		t.Fatalf("lookup returned %q, want the minted token %q", got, token)
	}
	// The looked-up display token must canonicalize to exactly 32 raw bytes.
	raw, err := proto.CanonicalizeReversePairToken(got)
	if err != nil {
		t.Fatalf("stored token must canonicalize: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("token must decode to 32 raw bytes, got %d", len(raw))
	}
}

// TestReversePairTokenStoreUnknownID pins that an id that was never minted is a
// clean miss.
func TestReversePairTokenStoreUnknownID(t *testing.T) {
	s, _ := newTestTokenStore(10 * time.Minute)
	if _, ok := s.lookup("deadbeef"); ok {
		t.Fatal("lookup of an unknown id must fail")
	}
}

// TestReversePairTokenStoreTTLExpiry pins that once the TTL passes, the token is
// gone (and lookup evicts it).
func TestReversePairTokenStoreTTLExpiry(t *testing.T) {
	s, clk := newTestTokenStore(10 * time.Minute)

	_, id, _, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Just before expiry it is still there.
	clk.advance(10*time.Minute - time.Second)
	if _, ok := s.lookup(id); !ok {
		t.Fatal("token must survive until its expiry")
	}
	// At/after expiry it is gone.
	clk.advance(2 * time.Second)
	if _, ok := s.lookup(id); ok {
		t.Fatal("token must be evicted at TTL expiry")
	}
	// And the entry is actually removed from the map, not merely hidden.
	s.mu.Lock()
	_, present := s.m[id]
	s.mu.Unlock()
	if present {
		t.Fatal("expired token must be evicted from the store")
	}
}

// TestReversePairTokenStoreSingleUse pins single-use: after burn, a second
// lookup of the same id fails.
func TestReversePairTokenStoreSingleUse(t *testing.T) {
	s, _ := newTestTokenStore(10 * time.Minute)

	_, id, _, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := s.lookup(id); !ok {
		t.Fatal("first lookup must succeed")
	}
	// A failed dial does NOT burn — lookup still works (retryable within TTL).
	if _, ok := s.lookup(id); !ok {
		t.Fatal("lookup without burn must remain valid (retry within TTL)")
	}
	// Success burns it; the next lookup fails.
	s.burn(id)
	if _, ok := s.lookup(id); ok {
		t.Fatal("a burned token must not be looked up again (single-use)")
	}
	// burn is idempotent.
	s.burn(id)
}

// TestReversePairTokenStoreEvictsOnMint pins that minting sweeps already-expired
// entries so the map cannot grow without bound.
func TestReversePairTokenStoreEvictsOnMint(t *testing.T) {
	s, clk := newTestTokenStore(10 * time.Minute)

	_, oldID, _, _ := s.mint()
	clk.advance(11 * time.Minute) // old token now expired

	_, _, _, err := s.mint() // this mint should sweep the expired old entry
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	s.mu.Lock()
	_, present := s.m[oldID]
	n := len(s.m)
	s.mu.Unlock()
	if present {
		t.Fatal("mint must sweep expired entries")
	}
	if n != 1 {
		t.Fatalf("store must hold only the 1 live token after a sweep, got %d", n)
	}
}

// TestReversePairTokenStoreConcurrent runs mint/lookup/burn from many goroutines
// to prove the store is race-free under -race.
func TestReversePairTokenStoreConcurrent(t *testing.T) {
	s := newReversePairTokenStore()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, id, _, err := s.mint()
			if err != nil {
				t.Errorf("mint: %v", err)
				return
			}
			if _, ok := s.lookup(id); !ok {
				t.Errorf("concurrent lookup of freshly minted id failed")
			}
			s.burn(id)
			if _, ok := s.lookup(id); ok {
				t.Errorf("burned id still present")
			}
		}()
	}
	wg.Wait()
}
