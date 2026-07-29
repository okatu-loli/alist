package totp

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/pquerna/otp"
	otptotp "github.com/pquerna/otp/totp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared&_totp_pkg=1"), &gorm.Config{})
	if err != nil {
		panic("failed to connect test database: " + err.Error())
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic("failed to migrate test database: " + err.Error())
	}
}

var testUserSeq int64

func newTestUser(t *testing.T) *model.User {
	t.Helper()
	n := atomic.AddInt64(&testUserSeq, 1)
	u := &model.User{Username: fmt.Sprintf("totpuser%d", n)}
	u.SetPassword("irrelevant-primary-password")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID) })
	return u
}

// TestKnownVectorValidates pins the underlying dependency's contract against
// the RFC 6238 Appendix B test vectors (SHA1, 8 digits, 30s period): a known
// secret at a known Unix time must validate as the documented code. This is
// the fixed secret+time vector check — independent of Service's own TTL
// bookkeeping, which uses wall-clock-independent injected time instead (see
// TestConfirmSetupExpires below).
func TestKnownVectorValidates(t *testing.T) {
	// RFC 6238 seed20 (SHA1): ASCII "12345678901234567890".
	secret := base32Encode([]byte("12345678901234567890"))
	cases := []struct {
		unixTime int64
		code     string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
	}
	opts := otptotp.ValidateOpts{Period: 30, Skew: 0, Digits: otp.DigitsEight, Algorithm: otp.AlgorithmSHA1}
	for _, tc := range cases {
		ok, err := otptotp.ValidateCustom(tc.code, secret, time.Unix(tc.unixTime, 0).UTC(), opts)
		if err != nil {
			t.Fatalf("validate at t=%d: %v", tc.unixTime, err)
		}
		if !ok {
			t.Fatalf("known vector at t=%d must validate code %q against secret", tc.unixTime, tc.code)
		}
	}
}

// TestBeginConfirmSetupRoundTrip proves the happy path: begin mints a secret,
// the CURRENT code for that secret confirms it, and the pending entry is then
// gone (a second confirm with the same code misses).
func TestBeginConfirmSetupRoundTrip(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)

	secret, uri, err := svc.BeginSetup(user)
	if err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	if secret == "" || !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("begin setup must return a secret and an otpauth:// URI, got secret=%q uri=%q", secret, uri)
	}

	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate current code: %v", err)
	}

	got, err := svc.ConfirmSetup(user, code)
	if err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	if got != secret {
		t.Fatalf("confirm setup returned secret %q, want %q", got, secret)
	}

	// Single-use: the pending entry is gone, so even the SAME (still
	// time-window-valid) code no longer confirms anything.
	if _, err := svc.ConfirmSetup(user, code); err != ErrSetupNotFound {
		t.Fatalf("second confirm must be ErrSetupNotFound, got %v", err)
	}
}

// TestConfirmSetupWrongCode proves a wrong code is rejected without
// consuming the pending setup (the user gets to retry).
func TestConfirmSetupWrongCode(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)

	secret, _, err := svc.BeginSetup(user)
	if err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	if _, err := svc.ConfirmSetup(user, "000000"); err != ErrInvalidCode {
		t.Fatalf("wrong code must be ErrInvalidCode, got %v", err)
	}

	// The pending setup survives a wrong attempt: the correct code still
	// confirms it afterward.
	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate current code: %v", err)
	}
	if _, err := svc.ConfirmSetup(user, code); err != nil {
		t.Fatalf("correct code after a wrong attempt must still confirm: %v", err)
	}
}

// TestConfirmSetupExpires proves the pending-setup TTL: once the injected
// clock has moved past setupTTL, even the correct code misses.
func TestConfirmSetupExpires(t *testing.T) {
	svc := NewService()
	now := time.Now()
	svc.now = func() time.Time { return now }
	user := newTestUser(t)

	secret, _, err := svc.BeginSetup(user)
	if err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	code, err := otptotp.GenerateCode(secret, now)
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	svc.now = func() time.Time { return now.Add(setupTTL + time.Second) }
	if _, err := svc.ConfirmSetup(user, code); err != ErrSetupNotFound {
		t.Fatalf("expired setup must be ErrSetupNotFound, got %v", err)
	}
}

// TestBeginSetupRefusesAlreadyEnabled proves a user with a confirmed secret
// cannot begin (or confirm) a second enrollment without unbinding first.
func TestBeginSetupRefusesAlreadyEnabled(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)
	user.OtpSecret = "already-set"

	if _, _, err := svc.BeginSetup(user); err != ErrAlreadyEnabled {
		t.Fatalf("begin setup for an already-enabled user must be ErrAlreadyEnabled, got %v", err)
	}
	if _, err := svc.ConfirmSetup(user, "000000"); err != ErrAlreadyEnabled {
		t.Fatalf("confirm setup for an already-enabled user must be ErrAlreadyEnabled, got %v", err)
	}
}

// TestLoginStepUpRoundTrip proves the full step-up ceremony: IssuePrincipal →
// BeginLogin → FinishLogin with the user's confirmed secret's current code.
func TestLoginStepUpRoundTrip(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)
	secret, _, err := svc.BeginSetup(user)
	if err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if _, err := svc.ConfirmSetup(user, code); err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		t.Fatalf("persist confirmed secret: %v", err)
	}

	principal := svc.IssuePrincipal(user)
	challengeID, err := svc.BeginLogin(principal)
	if err != nil {
		t.Fatalf("begin login: %v", err)
	}

	loginCode, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate login code: %v", err)
	}
	username, verified := svc.FinishLogin(challengeID, loginCode)
	if !verified || username != user.Username {
		t.Fatalf("finish login = (%q, %v), want (%q, true)", username, verified, user.Username)
	}

	// Single-use: the same challenge cannot be finished twice.
	if _, verified := svc.FinishLogin(challengeID, loginCode); verified {
		t.Fatal("a login challenge must not be finishable twice")
	}
}

// TestFinishLoginReplayRejected proves anti-replay: submitting the SAME code
// twice against two DIFFERENT challenges (for the same user) succeeds once
// and is rejected the second time, even though the standard ±1-period skew
// would otherwise still accept it.
func TestFinishLoginReplayRejected(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)
	secret, _, _ := svc.BeginSetup(user)
	setupCode, _ := otptotp.GenerateCode(secret, time.Now())
	if _, err := svc.ConfirmSetup(user, setupCode); err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		t.Fatalf("persist secret: %v", err)
	}

	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	p1 := svc.IssuePrincipal(user)
	c1, _ := svc.BeginLogin(p1)
	if _, verified := svc.FinishLogin(c1, code); !verified {
		t.Fatal("first use of a fresh code must verify")
	}

	p2 := svc.IssuePrincipal(user)
	c2, _ := svc.BeginLogin(p2)
	if _, verified := svc.FinishLogin(c2, code); verified {
		t.Fatal("replaying the same code against a second challenge must be rejected")
	}
}

// TestFinishLoginRecoveryCodeFallback proves a valid, unused recovery code
// completes the step-up even when it is not a valid TOTP code, and is
// consumed (single-use) by that success.
func TestFinishLoginRecoveryCodeFallback(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)
	secret, _, _ := svc.BeginSetup(user)
	setupCode, _ := otptotp.GenerateCode(secret, time.Now())
	if _, err := svc.ConfirmSetup(user, setupCode); err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		t.Fatalf("persist secret: %v", err)
	}
	codes, err := op.GenerateRecoveryCodes(user.ID)
	if err != nil {
		t.Fatalf("generate recovery codes: %v", err)
	}

	principal := svc.IssuePrincipal(user)
	challengeID, err := svc.BeginLogin(principal)
	if err != nil {
		t.Fatalf("begin login: %v", err)
	}
	username, verified := svc.FinishLogin(challengeID, codes[0])
	if !verified || username != user.Username {
		t.Fatalf("finish login with recovery code = (%q, %v), want (%q, true)", username, verified, user.Username)
	}

	// Single-use: a second step-up with the SAME recovery code fails.
	principal2 := svc.IssuePrincipal(user)
	challengeID2, err := svc.BeginLogin(principal2)
	if err != nil {
		t.Fatalf("begin login 2: %v", err)
	}
	if _, verified := svc.FinishLogin(challengeID2, codes[0]); verified {
		t.Fatal("a recovery code must not be usable twice")
	}
}

// TestBeginLoginRefusesUnknownPrincipal proves a principal that was never
// issued (or was already consumed) is refused.
func TestBeginLoginRefusesUnknownPrincipal(t *testing.T) {
	svc := NewService()
	if _, err := svc.BeginLogin("not-a-real-principal"); err != ErrPrincipalNotFound {
		t.Fatalf("unknown principal must be ErrPrincipalNotFound, got %v", err)
	}
}

// TestFinishLoginUnknownChallenge proves an unknown/expired challenge id is a
// clean, non-panicking rejection with no username to attribute it to.
func TestFinishLoginUnknownChallenge(t *testing.T) {
	svc := NewService()
	username, verified := svc.FinishLogin("not-a-real-challenge", "123456")
	if verified {
		t.Fatal("unknown challenge must not verify")
	}
	if username != "" {
		t.Fatalf("an unknown challenge resolves to no user, username must be empty, got %q", username)
	}
}

// TestFinishLoginReportsUsernameOnFailure is the red-team I3 regression: a
// WRONG code against a valid, resolved challenge must still report the
// bound user's username (verified=false) so the caller's audit trail can
// attribute the failed attempt to the real account instead of an anonymous
// actor.
func TestFinishLoginReportsUsernameOnFailure(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)
	secret, _, _ := svc.BeginSetup(user)
	setupCode, _ := otptotp.GenerateCode(secret, time.Now())
	if _, err := svc.ConfirmSetup(user, setupCode); err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		t.Fatalf("persist secret: %v", err)
	}

	principal := svc.IssuePrincipal(user)
	challengeID, err := svc.BeginLogin(principal)
	if err != nil {
		t.Fatalf("begin login: %v", err)
	}
	username, verified := svc.FinishLogin(challengeID, "000000")
	if verified {
		t.Fatal("wrong code must not verify")
	}
	if username != user.Username {
		t.Fatalf("a failed attempt against a resolved challenge must still report the username: got %q, want %q", username, user.Username)
	}
}

// TestFinishLoginStaleAfterPasswordChange is the red-team M8 regression: a
// challenge minted (via IssuePrincipal/BeginLogin) BEFORE a password change
// must be refused after the password changes, even with the correct current
// TOTP code — the captured PwdTS no longer matches the user's current one.
func TestFinishLoginStaleAfterPasswordChange(t *testing.T) {
	svc := NewService()
	user := newTestUser(t)
	secret, _, _ := svc.BeginSetup(user)
	setupCode, _ := otptotp.GenerateCode(secret, time.Now())
	if _, err := svc.ConfirmSetup(user, setupCode); err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		t.Fatalf("persist secret: %v", err)
	}

	principal := svc.IssuePrincipal(user)
	challengeID, err := svc.BeginLogin(principal)
	if err != nil {
		t.Fatalf("begin login: %v", err)
	}

	// Password changes AFTER the challenge was minted. Bump PwdTS directly
	// (rather than via SetPassword, whose time.Now().Unix() could collide
	// with the value already captured at IssuePrincipal time within the same
	// wall-clock second) so the test deterministically exercises "PwdTS moved"
	// regardless of execution speed.
	reloaded, err := op.GetUserById(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloaded.PwdTS = reloaded.PwdTS + 1000
	if err := op.UpdateUser(reloaded); err != nil {
		t.Fatalf("change password: %v", err)
	}

	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	username, verified := svc.FinishLogin(challengeID, code)
	if verified {
		t.Fatal("a challenge minted before a password change must be refused, even with the correct current code")
	}
	if username != user.Username {
		t.Fatalf("a stale-PwdTS rejection must still report the username, got %q want %q", username, user.Username)
	}
}

// base32Encode mirrors the RFC 6238 test-vector convention: the seed is an
// ASCII string, base32-encoded (unpadded) to form the secret parameter.
func base32Encode(raw []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	var sb strings.Builder
	var buf uint32
	var bits uint
	for _, b := range raw {
		buf = buf<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(alphabet[(buf>>bits)&31])
		}
	}
	if bits > 0 {
		sb.WriteByte(alphabet[(buf<<(5-bits))&31])
	}
	return sb.String()
}
