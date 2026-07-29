package passkey

import (
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// ChallengeStore holds the server-side SessionData of in-flight ceremonies,
// keyed by an opaque challenge id handed to the client. It is the anti-replay
// spine of both flows: entries are single-use (Take deletes) and expire after a
// TTL, so a challenge can never be redeemed twice nor long after it was issued.
//
// The store is safe for concurrent use.
type ChallengeStore struct {
	mu  sync.Mutex
	now func() time.Time // injectable clock for tests
	m   map[string]challengeEntry
}

type challengeEntry struct {
	data    *webauthn.SessionData
	expires time.Time
}

// NewChallengeStore returns an empty, ready-to-use store using the wall clock.
func NewChallengeStore() *ChallengeStore {
	return &ChallengeStore{
		now: time.Now,
		m:   make(map[string]challengeEntry),
	}
}

// Put stores session under id, expiring after ttl. Re-putting the same id
// overwrites the prior entry.
func (s *ChallengeStore) Put(id string, session *webauthn.SessionData, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = challengeEntry{data: session, expires: s.now().Add(ttl)}
}

// Take returns the session for id and removes it, so the challenge can be used
// exactly once. A missing or expired id returns (nil, false); an expired entry
// is also evicted.
func (s *ChallengeStore) Take(id string) (*webauthn.SessionData, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return nil, false
	}
	delete(s.m, id)
	if s.now().After(e.expires) {
		return nil, false
	}
	return e.data, true
}

// Len reports the number of live entries (including any not-yet-swept expired
// ones). Primarily for tests and diagnostics.
func (s *ChallengeStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
