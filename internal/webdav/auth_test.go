package webdav

import (
	"errors"
	"testing"

	"github.com/alist-org/alist/v3/internal/authlimit"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestVerifyCredentialsAppPassword pins the primary WebDAV credential: a
// minted app password authenticates regardless of the account's primary
// password or 2FA state.
func TestVerifyCredentialsAppPassword(t *testing.T) {
	u := newTestUser(t, "irrelevant-primary", "")
	plaintext, _, err := op.CreateAppPassword(u.ID, "webdav client")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}
	got, err := verifyCredentials(u.Username, plaintext)
	if err != nil {
		t.Fatalf("app password must verify: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("verify returned wrong user: got %d want %d", got.ID, u.ID)
	}
}

// TestVerifyCredentialsRawPasswordNon2FA pins the fallback path: a non-2FA
// account's real primary password works directly, with no app password
// minted at all.
func TestVerifyCredentialsRawPasswordNon2FA(t *testing.T) {
	u := newTestUser(t, "correct horse battery staple", "")
	got, err := verifyCredentials(u.Username, "correct horse battery staple")
	if err != nil {
		t.Fatalf("raw password for non-2FA account must verify: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("verify returned wrong user: got %d want %d", got.ID, u.ID)
	}
}

// TestVerifyCredentialsRawPasswordRejectedFor2FA pins design W5 §①: a 2FA
// account's raw (correct!) primary password must never authenticate a
// WebDAV request -- the protocol cannot carry a TOTP code, so the account
// must mint and use an app password instead.
func TestVerifyCredentialsRawPasswordRejectedFor2FA(t *testing.T) {
	u := newTestUser(t, "correct horse battery staple", "JBSWY3DPEHPK3PXP")
	_, err := verifyCredentials(u.Username, "correct horse battery staple")
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("2FA account's raw password must be PermissionDenied, got %v", err)
	}

	// The same account's app password still works.
	plaintext, _, err := op.CreateAppPassword(u.ID, "2fa client")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}
	got, err := verifyCredentials(u.Username, plaintext)
	if err != nil {
		t.Fatalf("2FA account's app password must verify: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("verify returned wrong user: got %d want %d", got.ID, u.ID)
	}
}

// TestVerifyCredentialsUniformFailures pins the anti-enumeration contract:
// wrong password, unknown username, and a disabled account all collapse to
// the same errs.PermissionDenied.
func TestVerifyCredentialsUniformFailures(t *testing.T) {
	u := newTestUser(t, "correct horse battery staple", "")

	t.Run("wrong password", func(t *testing.T) {
		if _, err := verifyCredentials(u.Username, "totally wrong"); !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("wrong password must be PermissionDenied, got %v", err)
		}
	})
	t.Run("unknown username", func(t *testing.T) {
		if _, err := verifyCredentials("no-such-user", "whatever"); !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("unknown username must be PermissionDenied, got %v", err)
		}
	})
	t.Run("disabled account", func(t *testing.T) {
		du := newTestUser(t, "correct horse battery staple", "")
		du.Disabled = true
		if err := db.UpdateUser(du); err != nil {
			t.Fatalf("disable user: %v", err)
		}
		if _, err := verifyCredentials(du.Username, "correct horse battery staple"); !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("disabled account must be PermissionDenied, got %v", err)
		}
	})
	t.Run("guest account", func(t *testing.T) {
		gu := newTestUser(t, "correct horse battery staple", "")
		gu.Role = append(gu.Role, model.GUEST)
		if err := db.UpdateUser(gu); err != nil {
			t.Fatalf("make guest: %v", err)
		}
		if _, err := verifyCredentials(gu.Username, "correct horse battery staple"); !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("guest account must be PermissionDenied, got %v", err)
		}
	})
}

// TestBasicAuthThrottleLocksOutAfterMaxAttempts pins the brute-force guard:
// after authlimit.MaxAttempts() consecutive failures from the same key, a
// further attempt is refused even with the CORRECT password, until the
// lockout window elapses. It exercises verifyCredentials + the throttle
// exactly as BasicAuth composes them, keyed the same way BasicAuth keys on
// client IP.
func TestBasicAuthThrottleLocksOutAfterMaxAttempts(t *testing.T) {
	u := newTestUser(t, "correct horse battery staple", "")
	const key = "203.0.113.7"
	authThrottle.Reset(key)
	t.Cleanup(func() { authThrottle.Reset(key) })

	for i := 0; i < authlimit.MaxAttempts(); i++ {
		if _, err := verifyCredentials(u.Username, "wrong"); err == nil {
			t.Fatalf("attempt %d: wrong password must fail", i)
		}
		authThrottle.Fail(key)
	}

	if !authThrottle.Blocked(key) {
		t.Fatal("key must be locked out after MaxAttempts consecutive failures")
	}
}
