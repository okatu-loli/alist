// Package totp is the host-side wiring around a per-user TOTP (RFC 6238)
// second factor (Phase C: internal TOTP second factor). It owns three
// in-memory, TTL-bounded stores:
//
//   - a pending-setup store (BeginSetup/ConfirmSetup): a not-yet-confirmed
//     secret parked between "show the QR code" and "user typed back a valid
//     code", never persisted until confirmed;
//   - a principal store (IssuePrincipal): the opaque, host-issued handle the
//     ext.AuthNMethod.Begin contract requires for a step-up factor, minted by
//     the login orchestrator right after a password check succeeds;
//   - a login-challenge store (BeginLogin/FinishLogin): the round-trip state
//     for the actual step-up ceremony, consumed by the /auth/login/2fa
//     handler.
//
// All three are single-use and swept on every mutating call, mirroring
// internal/agentsrv's PairingStore. Service holds no DB handle for its own
// writes: confirming a setup returns the secret for the HTTP handler to
// persist (same Service/handler split as internal/authn/passkey), so this
// package never itself flips a user's OtpSecret.
package totp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/pkg/errors"
	otptotp "github.com/pquerna/otp/totp"
)

// ErrAlreadyEnabled is returned by BeginSetup/ConfirmSetup when the user
// already has a confirmed TOTP secret — unbind first.
var ErrAlreadyEnabled = errors.New("totp: already enabled")

// ErrSetupNotFound is returned by ConfirmSetup when no pending setup exists
// for the user (never begun, already confirmed, or its TTL elapsed).
var ErrSetupNotFound = errors.New("totp: setup not found or expired")

// ErrInvalidCode is returned by ConfirmSetup for a code that does not verify
// against the pending secret (wrong code, or a replayed one — see
// validateCode).
var ErrInvalidCode = errors.New("totp: invalid code")

// ErrPrincipalNotFound is returned by BeginLogin for a principal that is
// unknown, already consumed, or expired.
var ErrPrincipalNotFound = errors.New("totp: principal not found or already used")

const (
	// setupTTL bounds how long a begun-but-unconfirmed enrollment stays
	// redeemable — long enough to scan a QR code and type back a code.
	setupTTL = 10 * time.Minute
	// principalTTL bounds the opaque login-step-up handle. It is consumed
	// synchronously (IssuePrincipal → method.Begin) within the same request,
	// so this only needs to survive that one hop.
	principalTTL = 2 * time.Minute
	// loginChallengeTTL bounds the step-up ceremony itself: long enough for a
	// user to open their authenticator app and type the code back.
	loginChallengeTTL = 5 * time.Minute
	// replayTTL bounds the anti-replay window for an accepted code. otptotp.
	// Validate's ±1 period skew means a code can be valid across up to a
	// ~90s span (the prior, current, and next 30s periods) depending on where
	// "now" lands relative to period boundaries; replayTTL covers the widest
	// case so a captured code cannot be resubmitted anywhere within the
	// window the standard skew would otherwise still accept it.
	replayTTL = 91 * time.Second
)

type pendingSetup struct {
	secret    string
	expiresAt time.Time
}

type principalEntry struct {
	userID    uint
	pwdTS     int64
	expiresAt time.Time
}

type loginChallenge struct {
	userID    uint
	pwdTS     int64
	expiresAt time.Time
}

// Service is the process-wide (or test-local) holder of the three stores
// above. now is overridable so tests can exercise TTL expiry deterministically
// without sleeping.
type Service struct {
	now func() time.Time

	mu         sync.Mutex
	setups     map[uint]pendingSetup
	principals map[string]principalEntry
	challenges map[string]loginChallenge
	replaySeen map[string]time.Time
}

// NewService builds an empty service with its own stores.
func NewService() *Service {
	return &Service{
		now:        time.Now,
		setups:     map[uint]pendingSetup{},
		principals: map[string]principalEntry{},
		challenges: map[string]loginChallenge{},
		replaySeen: map[string]time.Time{},
	}
}

// Default is the process-wide TOTP service. Both the built-in AuthNMethod
// adapter (login step-up) and the self-service HTTP handlers (begin/confirm)
// reference it, so a setup begun through one path can be confirmed through
// the same store.
var Default = NewService()

// issuerName is the otpauth:// issuer label shown in the user's authenticator
// app, drawn from the configured site title (falling back to "AList" before
// any admin has set one).
func issuerName() string {
	return setting.GetStr(conf.SiteTitle, "AList")
}

func randomToken() string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// sweepLocked evicts every expired entry across all four stores. Caller must
// hold s.mu.
func (s *Service) sweepLocked() {
	now := s.now()
	for k, v := range s.setups {
		if now.After(v.expiresAt) {
			delete(s.setups, k)
		}
	}
	for k, v := range s.principals {
		if now.After(v.expiresAt) {
			delete(s.principals, k)
		}
	}
	for k, v := range s.challenges {
		if now.After(v.expiresAt) {
			delete(s.challenges, k)
		}
	}
	for k, exp := range s.replaySeen {
		if now.After(exp) {
			delete(s.replaySeen, k)
		}
	}
}

// validateCode checks code against secret using the standard ±1-period skew
// (otptotp.Validate — the same primitive the legacy inline login check used),
// then applies an in-memory anti-replay guard scoped to replayKey: the SAME
// code string is accepted at most once per replayKey within replayTTL, so a
// captured code cannot be resubmitted even though the standard skew would
// otherwise still accept it a second time.
func (s *Service) validateCode(secret, code, replayKey string) bool {
	if code == "" || secret == "" || !otptotp.Validate(code, secret) {
		return false
	}
	fullKey := replayKey + ":" + code
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if exp, seen := s.replaySeen[fullKey]; seen && now.Before(exp) {
		return false
	}
	s.replaySeen[fullKey] = now.Add(replayTTL)
	return true
}

// BeginSetup starts a new TOTP enrollment for user: mints a fresh secret,
// parks it pending confirmation, and returns the secret (base32) plus the
// otpauth:// URI a QR-code renderer consumes. Refuses if the user already has
// a confirmed TOTP secret (unbind first) — this is advisory (the pending
// store has no DB read lock), the authoritative guard is ConfirmSetup's same
// check right before it would return a secret for the handler to persist.
func (s *Service) BeginSetup(user *model.User) (secret, otpauthURI string, err error) {
	if user.OtpSecret != "" {
		return "", "", ErrAlreadyEnabled
	}
	key, err := otptotp.Generate(otptotp.GenerateOpts{
		Issuer:      issuerName(),
		AccountName: user.Username,
	})
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.sweepLocked()
	s.setups[user.ID] = pendingSetup{secret: key.Secret(), expiresAt: s.now().Add(setupTTL)}
	s.mu.Unlock()
	return key.Secret(), key.URL(), nil
}

// ConfirmSetup verifies code against the pending secret begun for user. On
// success it returns the secret for the caller to persist (write
// user.OtpSecret + mint recovery codes) and clears the pending entry so a
// second confirm attempt (with the same or a different code) always misses.
func (s *Service) ConfirmSetup(user *model.User, code string) (secret string, err error) {
	if user.OtpSecret != "" {
		return "", ErrAlreadyEnabled
	}
	s.mu.Lock()
	s.sweepLocked()
	pending, ok := s.setups[user.ID]
	s.mu.Unlock()
	if !ok {
		return "", ErrSetupNotFound
	}
	if !s.validateCode(pending.secret, code, fmt.Sprintf("setup:%d", user.ID)) {
		return "", ErrInvalidCode
	}
	s.mu.Lock()
	// Re-check identity before deleting: a concurrent second confirm for the
	// same user racing this one must not delete a DIFFERENT (re-begun)
	// pending setup out from under it. Cheap because setups is keyed by user.
	if cur, ok := s.setups[user.ID]; ok && cur.secret == pending.secret {
		delete(s.setups, user.ID)
	}
	s.mu.Unlock()
	return pending.secret, nil
}

// VerifyBoundCode checks code against user's CONFIRMED secret (never a
// pending, unconfirmed setup) — either the user's current TOTP code (replay-
// guarded) or one of their unused recovery codes (consumed on success via
// op.ConsumeRecoveryCode). Used by the self-service unbind flow to require
// proof of possession before removing the second factor.
func (s *Service) VerifyBoundCode(user *model.User, code string) bool {
	if user.OtpSecret == "" {
		return false
	}
	if s.validateCode(user.OtpSecret, code, fmt.Sprintf("manage:%d", user.ID)) {
		return true
	}
	return op.ConsumeRecoveryCode(user.ID, code) == nil
}

// IssuePrincipal mints a short-lived, opaque handle for user's already
// password-proven identity — the ext.Principal the AuthNMethod.Begin contract
// requires for a step-up factor. Called by the login orchestrator (auth.go's
// loginHash) right after a password check succeeds; consumed synchronously by
// the very next call into BeginLogin (via the AuthNMethod adapter's Begin), so
// its TTL only needs to survive that one hop.
//
// It captures user.PwdTS at mint time (red-team M8): FinishLogin later
// refuses a challenge whose captured PwdTS no longer matches the user's
// CURRENT PwdTS, so a challenge minted against an old password becomes
// unusable the instant the password changes — mirroring how the refresh-token
// and protocol-Auth paths already treat a PwdTS mismatch as "this credential
// predates a password change, refuse it".
func (s *Service) IssuePrincipal(user *model.User) string {
	token := randomToken()
	s.mu.Lock()
	s.sweepLocked()
	s.principals[token] = principalEntry{userID: user.ID, pwdTS: user.PwdTS, expiresAt: s.now().Add(principalTTL)}
	s.mu.Unlock()
	return token
}

// BeginLogin resolves and single-use-consumes a principal minted by
// IssuePrincipal, then mints a login-challenge id bound to that user (and its
// captured PwdTS — see IssuePrincipal). Called from the ext.AuthNMethod
// adapter's Begin.
func (s *Service) BeginLogin(principal string) (challengeID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.principals[principal]
	delete(s.principals, principal) // single-use regardless of outcome
	if !ok || s.now().After(entry.expiresAt) {
		return "", ErrPrincipalNotFound
	}
	s.sweepLocked()
	id := randomToken()
	s.challenges[id] = loginChallenge{userID: entry.userID, pwdTS: entry.pwdTS, expiresAt: s.now().Add(loginChallengeTTL)}
	return id, nil
}

// FinishLogin verifies a login challenge's response: code must be either the
// user's current TOTP code (±1 period, anti-replay) or one of their unused
// recovery codes (consumed on success — see op.ConsumeRecoveryCode). It
// returns the resolved username on success; every rejection shape (unknown/
// expired/already-used challenge, a PwdTS that moved since Begin (M8), TOTP
// disabled since Begin, wrong code) is a clean verified=false so the host
// counts it as one ordinary failed attempt without leaking which case it was.
//
// Unlike its first (pre-red-team) version, username is populated on EVERY
// outcome where the challenge resolved to a real user row — including a
// failure — so the caller (server/handles/login_stepup.go) can audit the
// attempt against the actual account instead of a blank/anonymous actor
// (red-team I3). Only a challenge that never resolves to a user at all
// (unknown/expired challenge id) reports an empty username: there is no
// account to attribute the attempt to.
func (s *Service) FinishLogin(challengeID, code string) (username string, verified bool) {
	s.mu.Lock()
	entry, ok := s.challenges[challengeID]
	delete(s.challenges, challengeID) // single-use regardless of outcome
	s.mu.Unlock()
	if !ok || s.now().After(entry.expiresAt) {
		return "", false
	}
	user, err := op.GetUserById(entry.userID)
	if err != nil || user == nil {
		return "", false
	}
	// M8: the password was changed after this challenge was minted — refuse
	// it regardless of code correctness. Still report the username: this IS
	// an attempt against a real, identifiable account.
	if entry.pwdTS != user.PwdTS {
		return user.Username, false
	}
	if user.Disabled || user.OtpSecret == "" {
		return user.Username, false
	}
	if s.validateCode(user.OtpSecret, code, fmt.Sprintf("login:%d", user.ID)) {
		return user.Username, true
	}
	if err := op.ConsumeRecoveryCode(user.ID, code); err == nil {
		return user.Username, true
	}
	return user.Username, false
}
