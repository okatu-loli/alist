package op_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// appPasswordTestUserSeq keeps every test's fixture username unique across
// the package's shared in-memory DB (see storage_test.go's init()).
var appPasswordTestUserSeq int64

func newAppPasswordTestUser(t *testing.T, disabled bool) *model.User {
	t.Helper()
	n := atomic.AddInt64(&appPasswordTestUserSeq, 1)
	u := &model.User{
		Username: fmt.Sprintf("apwuser%d", n),
		Disabled: disabled,
	}
	u.SetPassword("irrelevant-primary-password")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID) })
	return u
}

// TestCreateAppPasswordFormatAndOneTime pins the plaintext shape (alp_ prefix,
// crypto/rand entropy, base32 lowercased/grouped) and that it is never
// reconstructible from the stored row (Hash/Salt only, never the plaintext).
func TestCreateAppPasswordFormatAndOneTime(t *testing.T) {
	u := newAppPasswordTestUser(t, false)

	plaintext, ap, err := op.CreateAppPassword(u.ID, "phone client")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}
	if !strings.HasPrefix(plaintext, "alp_") {
		t.Fatalf("plaintext must start with alp_, got %q", plaintext)
	}
	if plaintext == "alp_" {
		t.Fatalf("plaintext must carry entropy after the prefix")
	}
	if strings.ToLower(plaintext) != plaintext {
		t.Fatalf("plaintext must be lowercase, got %q", plaintext)
	}
	if ap.ID == 0 {
		t.Fatal("created app password must have a persisted id")
	}
	if ap.Hash == "" || ap.Salt == "" {
		t.Fatal("created app password must have a non-empty hash and salt")
	}
	if ap.Hash == plaintext {
		t.Fatal("stored hash must never equal the plaintext")
	}

	// Two creates never collide on plaintext (crypto/rand entropy).
	plaintext2, _, err := op.CreateAppPassword(u.ID, "second client")
	if err != nil {
		t.Fatalf("create second app password: %v", err)
	}
	if plaintext2 == plaintext {
		t.Fatal("two app passwords must have distinct plaintexts")
	}

	// The plaintext is never retrievable again via List (Hash/Salt excluded
	// from JSON too, but this pins the Go-level row shape).
	list, err := op.ListAppPasswords(u.ID)
	if err != nil {
		t.Fatalf("list app passwords: %v", err)
	}
	for _, row := range list {
		if row.Hash == plaintext || row.Salt == plaintext {
			t.Fatal("plaintext leaked into a persisted field")
		}
	}
}

// TestVerifyAppPasswordUniformErrors pins the anti-enumeration contract: a
// correct plaintext resolves to the owning user, and every miss shape — wrong
// plaintext, disabled user, and a user with zero app passwords at all —
// collapses to the same errs.PermissionDenied.
func TestVerifyAppPasswordUniformErrors(t *testing.T) {
	u := newAppPasswordTestUser(t, false)
	plaintext, _, err := op.CreateAppPassword(u.ID, "verify client")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}

	t.Run("correct", func(t *testing.T) {
		got, err := op.VerifyAppPassword(u.Username, plaintext)
		if err != nil {
			t.Fatalf("verify correct plaintext: %v", err)
		}
		if got.ID != u.ID {
			t.Fatalf("verify returned wrong user: got %d want %d", got.ID, u.ID)
		}
	})

	t.Run("wrong plaintext", func(t *testing.T) {
		_, err := op.VerifyAppPassword(u.Username, "alp_totally-wrong")
		if !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("wrong plaintext must be PermissionDenied, got %v", err)
		}
	})

	t.Run("unknown username", func(t *testing.T) {
		_, err := op.VerifyAppPassword("no-such-user-at-all", plaintext)
		if !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("unknown username must be PermissionDenied, got %v", err)
		}
	})

	t.Run("disabled user", func(t *testing.T) {
		du := newAppPasswordTestUser(t, true)
		dplain, _, err := op.CreateAppPassword(du.ID, "disabled client")
		if err != nil {
			t.Fatalf("create app password for disabled user: %v", err)
		}
		_, err = op.VerifyAppPassword(du.Username, dplain)
		if !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("disabled user must be PermissionDenied even with a correct plaintext, got %v", err)
		}
	})

	t.Run("user with no app passwords", func(t *testing.T) {
		bare := newAppPasswordTestUser(t, false)
		_, err := op.VerifyAppPassword(bare.Username, "alp_anything-goes-here")
		if !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("user with no app passwords must be PermissionDenied (uniform, no early distinction), got %v", err)
		}
	})

	// Finding 2 (W5 security review): a guest must never authenticate to a
	// protocol via app password, defense-in-depth mirroring the raw-password
	// branch's user.IsGuest() check in internal/webdav/auth.go's
	// verifyCredentials. Proven even with a real, otherwise-valid app
	// password row -- not just the "no app passwords" collapse above.
	t.Run("guest with a valid app password", func(t *testing.T) {
		guest := newAppPasswordTestUser(t, false)
		gplain, _, err := op.CreateAppPassword(guest.ID, "guest client")
		if err != nil {
			t.Fatalf("create app password for guest: %v", err)
		}
		guest.Role = model.Roles{model.GUEST}
		if err := db.UpdateUser(guest); err != nil {
			t.Fatalf("mark user guest: %v", err)
		}

		_, err = op.VerifyAppPassword(guest.Username, gplain)
		if !errors.Is(err, errs.PermissionDenied) {
			t.Fatalf("guest with a valid app password must be PermissionDenied, got %v", err)
		}
	})
}

// TestVerifyAppPasswordUpdatesLastUsed proves a successful verify stamps
// LastUsed best-effort, visible on a subsequent list.
func TestVerifyAppPasswordUpdatesLastUsed(t *testing.T) {
	u := newAppPasswordTestUser(t, false)
	plaintext, ap, err := op.CreateAppPassword(u.ID, "last-used client")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}

	list, err := op.ListAppPasswords(u.ID)
	if err != nil {
		t.Fatalf("list before verify: %v", err)
	}
	for _, row := range list {
		if row.ID == ap.ID && row.LastUsed != nil {
			t.Fatal("precondition: LastUsed must be nil before any verify")
		}
	}

	if _, err := op.VerifyAppPassword(u.Username, plaintext); err != nil {
		t.Fatalf("verify: %v", err)
	}

	list, err = op.ListAppPasswords(u.ID)
	if err != nil {
		t.Fatalf("list after verify: %v", err)
	}
	var found bool
	for _, row := range list {
		if row.ID == ap.ID {
			found = true
			if row.LastUsed == nil {
				t.Fatal("LastUsed must be set after a successful verify")
			}
		}
	}
	if !found {
		t.Fatal("app password missing from list after verify")
	}
}

// TestDeleteAppPasswordRevokesImmediately proves there is no cache: deleting
// an app password makes the very next verify with its plaintext fail.
func TestDeleteAppPasswordRevokesImmediately(t *testing.T) {
	u := newAppPasswordTestUser(t, false)
	plaintext, ap, err := op.CreateAppPassword(u.ID, "revoke-me")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}

	if _, err := op.VerifyAppPassword(u.Username, plaintext); err != nil {
		t.Fatalf("verify before delete must succeed: %v", err)
	}

	if err := op.DeleteAppPassword(u.ID, ap.ID); err != nil {
		t.Fatalf("delete app password: %v", err)
	}

	if _, err := op.VerifyAppPassword(u.Username, plaintext); !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("verify after delete must fail immediately, got %v", err)
	}
}

// TestDeleteAppPasswordScopedToOwner proves the op layer itself enforces
// ownership (design W3: self-service only, no admin bypass): deleting another
// user's id is a uniform AppPasswordNotFound and never actually deletes the
// row.
func TestDeleteAppPasswordScopedToOwner(t *testing.T) {
	owner := newAppPasswordTestUser(t, false)
	other := newAppPasswordTestUser(t, false)
	plaintext, ap, err := op.CreateAppPassword(owner.ID, "owned-by-owner")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}

	if err := op.DeleteAppPassword(other.ID, ap.ID); !errors.Is(err, errs.AppPasswordNotFound) {
		t.Fatalf("deleting another user's app password must be AppPasswordNotFound, got %v", err)
	}

	// Never actually deleted: the owner can still verify with it.
	if _, err := op.VerifyAppPassword(owner.Username, plaintext); err != nil {
		t.Fatalf("app password must survive a cross-owner delete attempt: %v", err)
	}

	// A missing id (never existed) is the same uniform error.
	if err := op.DeleteAppPassword(owner.ID, ap.ID+999999); !errors.Is(err, errs.AppPasswordNotFound) {
		t.Fatalf("missing id must be AppPasswordNotFound, got %v", err)
	}
}

// TestAppPasswordPerUserCap pins the abuse guard: the 21st app password for
// one user is refused.
func TestAppPasswordPerUserCap(t *testing.T) {
	u := newAppPasswordTestUser(t, false)
	for i := 0; i < op.AppPasswordMaxPerUser; i++ {
		if _, _, err := op.CreateAppPassword(u.ID, fmt.Sprintf("client-%d", i)); err != nil {
			t.Fatalf("create app password %d: %v", i, err)
		}
	}
	if _, _, err := op.CreateAppPassword(u.ID, "one-too-many"); !errors.Is(err, errs.AppPasswordLimitExceeded) {
		t.Fatalf("21st app password must be AppPasswordLimitExceeded, got %v", err)
	}
}

// TestAppPasswordConcurrentCreateVerify is the -race proof: concurrent
// creates and verifies against the same user hit no data race (op keeps no
// mutable shared state for app passwords — every verify reads the DB).
func TestAppPasswordConcurrentCreateVerify(t *testing.T) {
	u := newAppPasswordTestUser(t, false)
	seed, _, err := op.CreateAppPassword(u.ID, "seed")
	if err != nil {
		t.Fatalf("seed app password: %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, _ = op.CreateAppPassword(u.ID, fmt.Sprintf("concurrent-%d", i))
		}(i)
		go func() {
			defer wg.Done()
			_, _ = op.VerifyAppPassword(u.Username, seed)
		}()
	}
	wg.Wait()
}
