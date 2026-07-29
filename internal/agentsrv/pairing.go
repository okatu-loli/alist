// Package agentsrv holds the SERVER-side state for the remote-agent feature
// (design W12; relocated out of internal/agent in the W1 server/agent repo
// split). This wave (W12-1) is the bounded data-layer + admin-endpoint
// foundation: PairingStore only mints and reads pairing sessions. An actual
// agent connecting, deriving a SAS over the channel, and mutually confirming
// the pairing — the part that would flip a session to StateConfirmed and
// land a model.Agent row — is W12-2.
package agentsrv

import (
	"sync"
	"time"

	"github.com/alist-org/alist/v3/pkg/utils/random"
)

// State is a pairing session's lifecycle stage.
type State string

const (
	StatePending   State = "pending"
	StateConfirmed State = "confirmed"
	StateExpired   State = "expired"
)

const (
	// codeLen is the character count handed to random.String, drawing from a
	// 62-symbol alphabet (crypto/rand-backed, see pkg/utils/random.String) —
	// 32 chars is ~190 bits of entropy, far beyond brute-force range for a
	// 15-minute window.
	codeLen = 32
	// ttl is how long a minted pairing code stays valid before Get treats it
	// as gone.
	ttl = 15 * time.Minute
)

// Session is one pairing attempt: a code an admin displays (QR/manual entry)
// for a not-yet-paired agent to consume. This wave never transitions State
// away from StatePending (no channel to confirm over yet); the field exists
// now so W12-2 does not need a storage-shape migration to add it.
type Session struct {
	Code      string
	ExpiresAt time.Time
	State     State
}

// PairingStore is an in-memory, mutex-guarded set of live pairing sessions.
// In-memory is deliberate for this wave: a pairing code is a short-lived
// (15min), single-admin-initiated secret with no cross-restart durability
// requirement — losing it on a process restart just means the admin starts
// pairing again.
type PairingStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewPairingStore builds an empty store. Exposed (rather than only the
// package-level singleton below) so callers that want isolation — tests, or
// a future multi-tenant host — can inject their own.
func NewPairingStore() *PairingStore {
	return &PairingStore{sessions: make(map[string]*Session)}
}

// Start mints a fresh pairing session and returns its code and expiry. Each
// call performs a lazy sweep of already-expired sessions first, so the map
// never grows unbounded from abandoned pairing attempts even without a
// background goroutine.
func (s *PairingStore) Start() (code string, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	code = random.String(codeLen)
	expiresAt = time.Now().Add(ttl)
	s.sessions[code] = &Session{Code: code, ExpiresAt: expiresAt, State: StatePending}
	return code, expiresAt
}

// Get looks up a session by code. An expired session is evicted on this
// access (lazy sweep) and reported as not-found — callers never observe
// StateExpired through Get, only a bare "not ok".
func (s *PairingStore) Get(code string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[code]
	if !ok {
		return Session{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, code)
		return Session{}, false
	}
	return *sess, true
}

// Complete atomically transitions a pending, unexpired session to
// StateConfirmed and returns it. This is the single point that makes a
// pairing code single-use, and does so without an oracle: a code that was
// never minted, one that has expired, and one that is already confirmed
// (a second complete for the same code) all collapse into the exact same
// (Session{}, false) result. A caller must never try to distinguish them —
// see errs.InvalidPairingCode.
func (s *PairingStore) Complete(code string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	sess, ok := s.sessions[code]
	if !ok {
		return Session{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, code)
		return Session{}, false
	}
	if sess.State != StatePending {
		return Session{}, false
	}
	sess.State = StateConfirmed
	return *sess, true
}

// sweepLocked evicts every session past its ExpiresAt. Caller must hold mu.
func (s *PairingStore) sweepLocked() {
	now := time.Now()
	for code, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, code)
		}
	}
}

// defaultStore is the process-wide singleton the HTTP handlers use.
var defaultStore = NewPairingStore()

// Start mints a session on the package-level default store.
func Start() (code string, expiresAt time.Time) {
	return defaultStore.Start()
}

// Get reads a session from the package-level default store.
func Get(code string) (Session, bool) {
	return defaultStore.Get(code)
}

// Complete completes a session on the package-level default store.
func Complete(code string) (Session, bool) {
	return defaultStore.Complete(code)
}
