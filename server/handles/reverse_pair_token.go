package handles

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/AlistGo/alist-agent/proto"
	"sync"
	"time"
)

// Server-side one-time reverse-pairing token store (design subproject F, wave
// F3). This is the SERVER end of the token lifecycle: an admin mints a
// high-entropy one-time token T here, copies it out of band to the agent's
// pairing web (F2), and the server later dials the agent presenting T to prove —
// cryptographically, over the handshake transcript — that both sides hold the
// SAME secret (see internal/agent/pairing_handshake.go reversePairTag). This
// store is the "server internally stashes T (TTL, single-use)" the design spec
//「服务器侧」calls for.
//
// 🔒 SECURITY / LIFECYCLE INVARIANTS:
//   - T NEVER touches the DB or disk — it is a short-lived secret held only in
//     process memory. A process restart drops all armed tokens (the operator
//     re-mints), which is the correct fail-closed behavior for a pairing secret.
//   - TTL: every token expires reversePairTokenTTL (10 min) after mint and is
//     evicted — a leaked/forgotten token cannot linger as a standing MITM key.
//   - SINGLE-USE: a token is burned on SUCCESSFUL pairing (burn) so it can never
//     be reused. It is deliberately NOT burned on a FAILED dial/verify, so the
//     operator can retry within the TTL (design spec Important #5: single-use
//     means success-or-TTL, not "one dial attempt").
//   - The lookup handle is an opaque, high-entropy token_id — NOT the secret T.
//     The secret token bytes are never compared for lookup; only the random id is
//     matched (a map key), so no timing side channel on lookup can leak T.

const (
	// reversePairTokenTTL is how long a minted token stays valid before it is
	// evicted (design spec: TTL ≤ 10min). FIXED — a longer window would leave a
	// standing pairing secret usable, a shorter one would rush the operator.
	reversePairTokenTTL = 10 * time.Minute

	// reversePairTokenIDBytes is the entropy of the opaque token_id handle (128
	// bits). The id is only a lookup key into the in-memory store, never the
	// secret, but it is still generated from crypto/rand so it is unguessable and
	// carries no structure a caller could enumerate.
	reversePairTokenIDBytes = 16
)

// reversePairTokenEntry is one armed token: the base32 DISPLAY token string the
// operator copied to the agent (canonicalized to raw bytes only at dial time)
// and its absolute expiry. Held in memory only — never persisted.
type reversePairTokenEntry struct {
	token   string
	expires time.Time
}

// reversePairTokenStore is the thread-safe, in-memory, single-use+TTL store of
// armed reverse-pairing tokens keyed by opaque token_id. All fields behind mu.
// ttl / now / gen are struct fields (not package constants/functions) purely so
// tests can inject a short TTL, a fake clock, or a deterministic generator; the
// package-global store (reversePairTokens) uses the real 10-min TTL, time.Now,
// and proto.GenerateReversePairToken.
type reversePairTokenStore struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	gen func() (string, error)
	m   map[string]reversePairTokenEntry
}

// newReversePairTokenStore builds a store wired to the production token
// generator, the real clock, and the fixed 10-min TTL.
func newReversePairTokenStore() *reversePairTokenStore {
	return &reversePairTokenStore{
		ttl: reversePairTokenTTL,
		now: time.Now,
		gen: proto.GenerateReversePairToken,
		m:   make(map[string]reversePairTokenEntry),
	}
}

// reversePairTokens is the process-wide armed-token store. In-memory only.
var reversePairTokens = newReversePairTokenStore()

// mint generates a fresh high-entropy token (via gen, i.e.
// GenerateReversePairToken which ENFORCES 256-bit entropy in code) and a random
// opaque token_id, stores the token under the id with a fresh TTL, and returns
// the display token, its id, and the absolute expiry. Expired entries are swept
// on every mint so the map never grows without bound. The token STRING is the
// base32 the operator copies to the agent; token_id is the handle the later dial
// looks it up by.
func (s *reversePairTokenStore) mint() (token, id string, expiresAt time.Time, err error) {
	token, err = s.gen()
	if err != nil {
		return "", "", time.Time{}, err
	}
	idBytes := make([]byte, reversePairTokenIDBytes)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", time.Time{}, fmt.Errorf("agentpair: read random token id: %w", err)
	}
	id = hex.EncodeToString(idBytes)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	expiresAt = s.now().Add(s.ttl)
	s.m[id] = reversePairTokenEntry{token: token, expires: expiresAt}
	return token, id, expiresAt, nil
}

// lookup returns the armed DISPLAY token for id WITHOUT burning it (a peek), so
// a failed dial/verify can be retried within the TTL. A missing id, or one whose
// entry has passed its expiry, returns ok=false; an expired entry is evicted in
// passing. Burning on SUCCESS is the caller's separate burn() step.
func (s *reversePairTokenStore) lookup(id string) (token string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return "", false
	}
	if !s.now().Before(e.expires) {
		delete(s.m, id)
		return "", false
	}
	return e.token, true
}

// burn removes id from the store — the SINGLE-USE step, called only after a
// token has successfully paired an agent so it can never be reused. Idempotent:
// burning an already-gone id is a no-op.
func (s *reversePairTokenStore) burn(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// evictExpiredLocked drops every entry at/after its expiry. Caller holds mu.
// Cheap: the armed set is a handful of tokens at most, so an O(n) sweep on mint
// is fine and keeps eviction deterministic without a background goroutine.
func (s *reversePairTokenStore) evictExpiredLocked() {
	now := s.now()
	for id, e := range s.m {
		if !now.Before(e.expires) {
			delete(s.m, id)
		}
	}
}
