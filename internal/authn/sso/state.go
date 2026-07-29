package sso

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

// State is the server-side context of one in-flight federated login, parked
// under an opaque state id while the user is away at the IdP. It is the spine of
// the three-secret defense: it remembers the nonce (to reject a replayed
// id_token) and the PKCE verifier (to prove this callback owns the code), plus
// which provider the round-trip belongs to.
//
//   - ProviderKey names the configured provider the flow started from.
//   - Nonce must equal the id_token's nonce claim on callback.
//   - PKCEVerifier is presented to the token endpoint to redeem the code.
//   - Remember carries the "remember me" choice through the redirect.
//   - LinkUserID is the already-authenticated user this flow links to when in
//     account-linking mode; 0 means a plain login flow.
//   - ClientID is the frontend's per-browser Client-Id, captured from the begin
//     request (an XHR that carries the header) and replayed at callback time. The
//     callback is a bare browser redirect with no Client-Id header, so without
//     this the login session would be minted under an empty client id and every
//     subsequent authed XHR (which sends the real Client-Id) would fail the
//     device-session check. Carrying it here keeps the session key consistent.
//   - CreatedAt is when the flow began (diagnostics; expiry is enforced by TTL).
type State struct {
	ProviderKey  string
	Nonce        string
	PKCEVerifier string
	Remember     bool
	LinkUserID   uint
	ClientID     string
	CreatedAt    time.Time
}

// StateStore parks in-flight login States keyed by an opaque state id handed to
// the IdP as the OAuth2 `state` parameter. Like passkey's challenge store it is
// the anti-CSRF/anti-replay spine: entries are single-use (Take deletes) and
// expire after a TTL, so a state can never be redeemed twice nor long after it
// was issued. The store is safe for concurrent use.
type StateStore struct {
	mu  sync.Mutex
	now func() time.Time // injectable clock for tests
	m   map[string]stateEntry
}

type stateEntry struct {
	state   State
	expires time.Time
}

// NewStateStore returns an empty, ready-to-use store using the wall clock.
func NewStateStore() *StateStore {
	return &StateStore{
		now: time.Now,
		m:   make(map[string]stateEntry),
	}
}

// Put parks st under id, expiring after ttl. Re-putting the same id overwrites
// the prior entry.
func (s *StateStore) Put(id string, st State, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = stateEntry{state: st, expires: s.now().Add(ttl)}
}

// Take returns the State for id and removes it, so a state can be redeemed
// exactly once. A missing or expired id returns (zero, false); an expired entry
// is also evicted.
func (s *StateStore) Take(id string) (State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return State{}, false
	}
	delete(s.m, id)
	if s.now().After(e.expires) {
		return State{}, false
	}
	return e.state, true
}

// Len reports the number of live entries (including any not-yet-swept expired
// ones). Primarily for tests and diagnostics.
func (s *StateStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// GenerateState returns a fresh, unguessable state id (256 bits, URL-safe).
func GenerateState() (string, error) { return randomToken(32) }

// GenerateNonce returns a fresh, unguessable nonce (256 bits, URL-safe) for the
// OIDC id_token replay defense.
func GenerateNonce() (string, error) { return randomToken(32) }

// GeneratePKCE returns a fresh PKCE (RFC 7636) verifier and its S256 challenge.
// The verifier is a high-entropy secret kept server-side (in State) and revealed
// only to the token endpoint; the challenge is sent through the browser in the
// authorization request. The token endpoint accepts the code only if
// SHA256(verifier) base64url-equals the challenge, which defeats an attacker who
// intercepts the authorization code but never saw the verifier.
func GeneratePKCE() (verifier, challenge string, err error) {
	verifier, err = randomToken(32)
	if err != nil {
		return "", "", err
	}
	return verifier, pkceChallenge(verifier), nil
}

// pkceChallenge computes the S256 code challenge for a verifier:
// base64url(SHA256(verifier)), no padding, per RFC 7636 §4.2.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomToken returns nBytes of cryptographically-random data as an unpadded
// URL-safe base64 string.
func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
