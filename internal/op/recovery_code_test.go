package op_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

var recoveryCodeTestUserSeq int64

func newRecoveryCodeTestUser(t *testing.T) *model.User {
	t.Helper()
	n := atomic.AddInt64(&recoveryCodeTestUserSeq, 1)
	u := &model.User{Username: fmt.Sprintf("rcuser%d", n)}
	u.SetPassword("irrelevant-primary-password")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID) })
	return u
}

// TestGenerateRecoveryCodesShapeAndOneTime pins the count, format, and
// no-plaintext-persisted contract.
func TestGenerateRecoveryCodesShapeAndOneTime(t *testing.T) {
	u := newRecoveryCodeTestUser(t)

	codes, err := op.GenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("generate recovery codes: %v", err)
	}
	if len(codes) != op.RecoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), op.RecoveryCodeCount)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if strings.ToLower(c) != c {
			t.Fatalf("code must be lowercase, got %q", c)
		}
		if seen[c] {
			t.Fatalf("duplicate code generated: %q", c)
		}
		seen[c] = true
	}

	rows, err := db.ListRecoveryCodesByUser(u.ID)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	if len(rows) != op.RecoveryCodeCount {
		t.Fatalf("got %d persisted rows, want %d", len(rows), op.RecoveryCodeCount)
	}
	for _, row := range rows {
		for _, c := range codes {
			if row.Hash == c || row.Salt == c {
				t.Fatal("plaintext leaked into a persisted field")
			}
		}
	}
}

// TestConsumeRecoveryCodeSingleUse proves a code verifies exactly once: the
// second attempt with the same plaintext is rejected even though the code was
// never deleted.
func TestConsumeRecoveryCodeSingleUse(t *testing.T) {
	u := newRecoveryCodeTestUser(t)
	codes, err := op.GenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	code := codes[0]

	if err := op.ConsumeRecoveryCode(u.ID, code); err != nil {
		t.Fatalf("first consume must succeed: %v", err)
	}
	if err := op.ConsumeRecoveryCode(u.ID, code); !errors.Is(err, errs.TOTPInvalidCode) {
		t.Fatalf("second consume of the same code must be TOTPInvalidCode, got %v", err)
	}

	// Case-insensitive / whitespace-tolerant: a still-unused code typed with a
	// different case and stray spaces still matches.
	other := strings.ToUpper(codes[1])
	if err := op.ConsumeRecoveryCode(u.ID, "  "+other+"  "); err != nil {
		t.Fatalf("consume must normalize case/whitespace: %v", err)
	}
}

// TestConsumeRecoveryCodeWrongCode proves a plaintext that never matches any
// row is rejected uniformly.
func TestConsumeRecoveryCodeWrongCode(t *testing.T) {
	u := newRecoveryCodeTestUser(t)
	if _, err := op.GenerateRecoveryCodes(u.ID); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := op.ConsumeRecoveryCode(u.ID, "totally-wrong-code"); !errors.Is(err, errs.TOTPInvalidCode) {
		t.Fatalf("wrong code must be TOTPInvalidCode, got %v", err)
	}
}

// TestGenerateRecoveryCodesRegenerateInvalidatesOld proves a regeneration
// starts from a clean slate: a code from the OLD batch no longer verifies.
func TestGenerateRecoveryCodesRegenerateInvalidatesOld(t *testing.T) {
	u := newRecoveryCodeTestUser(t)
	oldCodes, err := op.GenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("generate first batch: %v", err)
	}
	newCodes, err := op.GenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("generate second batch: %v", err)
	}

	if err := op.ConsumeRecoveryCode(u.ID, oldCodes[0]); !errors.Is(err, errs.TOTPInvalidCode) {
		t.Fatalf("old-batch code must no longer verify after regeneration, got %v", err)
	}
	if err := op.ConsumeRecoveryCode(u.ID, newCodes[0]); err != nil {
		t.Fatalf("new-batch code must verify: %v", err)
	}
}

// TestRecoveryCodeStatusCountsRemaining pins the total/remaining accounting
// the account page renders ("3 of 10 remaining").
func TestRecoveryCodeStatusCountsRemaining(t *testing.T) {
	u := newRecoveryCodeTestUser(t)
	codes, err := op.GenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	total, remaining, err := op.RecoveryCodeStatus(u.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if total != op.RecoveryCodeCount || remaining != op.RecoveryCodeCount {
		t.Fatalf("fresh batch: got total=%d remaining=%d, want both %d", total, remaining, op.RecoveryCodeCount)
	}

	if err := op.ConsumeRecoveryCode(u.ID, codes[0]); err != nil {
		t.Fatalf("consume: %v", err)
	}
	total, remaining, err = op.RecoveryCodeStatus(u.ID)
	if err != nil {
		t.Fatalf("status after consume: %v", err)
	}
	if total != op.RecoveryCodeCount || remaining != op.RecoveryCodeCount-1 {
		t.Fatalf("after one consume: got total=%d remaining=%d, want total=%d remaining=%d",
			total, remaining, op.RecoveryCodeCount, op.RecoveryCodeCount-1)
	}
}

// TestDeleteRecoveryCodesRemovesAll proves unbind leaves no dangling backup
// credentials: after delete, every previously-valid code is rejected and the
// status reports zero.
func TestDeleteRecoveryCodesRemovesAll(t *testing.T) {
	u := newRecoveryCodeTestUser(t)
	codes, err := op.GenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := op.DeleteRecoveryCodes(u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	total, remaining, err := op.RecoveryCodeStatus(u.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if total != 0 || remaining != 0 {
		t.Fatalf("after delete: got total=%d remaining=%d, want 0/0", total, remaining)
	}
	if err := op.ConsumeRecoveryCode(u.ID, codes[0]); !errors.Is(err, errs.TOTPInvalidCode) {
		t.Fatalf("code must not verify after delete, got %v", err)
	}
}

// consumeRecoveryCodeRetryBusy retries op.ConsumeRecoveryCode past transient
// "database is locked" errors — an artifact of the test suite's shared,
// single-file in-memory SQLite under real concurrent writers (no WAL mode),
// orthogonal to the atomicity property this test actually pins. A genuine
// outcome (nil success, or errs.TOTPInvalidCode because this call lost the
// race or the code was already used) is returned immediately; only the
// SQLite busy error is retried.
func consumeRecoveryCodeRetryBusy(userID uint, code string) error {
	var err error
	for i := 0; i < 50; i++ {
		err = op.ConsumeRecoveryCode(userID, code)
		if err == nil || errors.Is(err, errs.TOTPInvalidCode) {
			return err
		}
		if !strings.Contains(err.Error(), "locked") {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return err
}

// TestConsumeRecoveryCodeConcurrentSameCodeOnlyOneWins is the red-team I7
// regression: two goroutines racing to consume the exact same still-unused
// code must not both succeed — db.MarkRecoveryCodeUsedIfUnused's conditional
// UPDATE means only the first writer wins, the second gets the same
// errs.TOTPInvalidCode as an already-used code.
func TestConsumeRecoveryCodeConcurrentSameCodeOnlyOneWins(t *testing.T) {
	u := newRecoveryCodeTestUser(t)
	codes, err := op.GenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	code := codes[0]

	const attempts = 2
	var wg sync.WaitGroup
	results := make([]error, attempts)
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = consumeRecoveryCodeRetryBusy(u.ID, code)
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, errs.TOTPInvalidCode) {
			t.Fatalf("unexpected error from a losing consumer: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("exactly one concurrent consumer of the same code must succeed, got %d successes out of %d", successes, attempts)
	}
}
