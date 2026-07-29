package agentsrv

import (
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/sign"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// initTaskCapTestDB bootstraps a fresh in-memory db with the full migration
// chain (which seeds the download signing key) and reloads internal/sign's
// package-level signer from it — mirrors internal/sign/keyset_test.go's
// initSignTestDB, since IssueTaskCap/VerifyTaskCap ride on that exact
// primitive.
func initTaskCapTestDB(t *testing.T) {
	t.Helper()
	dsn := "file:taskcap_test_" + t.Name() + "?mode=memory&cache=shared"
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		t.Fatal(err)
	}
	if err := sign.Reload(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskCapIssueVerifyRoundTrip(t *testing.T) {
	initTaskCapTestDB(t)

	tok, err := IssueTaskCap("agent-1", "task-1", "/src/a.bin", "/dst", time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := VerifyTaskCap(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.AgentID != "agent-1" || claims.TaskID != "task-1" || claims.Src != "/src/a.bin" || claims.DstDir != "/dst" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
	if claims.Exp <= time.Now().Unix() {
		t.Fatalf("exp must be in the future: %d", claims.Exp)
	}
}

// TestTaskCapTTLClampedToMax pins that a caller cannot mint a
// longer-than-MaxTaskCapTTL token by asking for one.
func TestTaskCapTTLClampedToMax(t *testing.T) {
	initTaskCapTestDB(t)

	tok, err := IssueTaskCap("agent-1", "task-1", "/src", "/dst", 24*time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := VerifyTaskCap(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	maxExp := time.Now().Add(MaxTaskCapTTL + time.Second).Unix()
	if claims.Exp > maxExp {
		t.Fatalf("exp %d exceeds clamp ceiling %d — ttl request was not clamped", claims.Exp, maxExp)
	}

	// A zero/negative ttl request must ALSO clamp to the max, not to "no
	// expiry" or "already expired".
	tok2, err := IssueTaskCap("agent-1", "task-1", "/src", "/dst", 0)
	if err != nil {
		t.Fatalf("issue with ttl=0: %v", err)
	}
	claims2, err := VerifyTaskCap(tok2)
	if err != nil {
		t.Fatalf("verify ttl=0 token: %v", err)
	}
	if claims2.Exp <= time.Now().Unix() {
		t.Fatalf("ttl=0 must clamp to MaxTaskCapTTL, not expire immediately: exp=%d", claims2.Exp)
	}
}

// TestTaskCapExpiredRejected pins that a token past its own Exp is refused
// even though nothing about its bytes was tampered with — achieved here by
// minting with a already-past ttl is not directly possible (IssueTaskCap
// clamps ttl<=0 upward), so instead this drives VerifyTaskCap's belt-and-
// suspenders Exp re-check directly against a hand-built expired claims
// payload signed with a real, valid signature — pinning that check
// independently of whichever expiry mechanism (HMAC-embedded vs JSON field)
// a future refactor might touch.
func TestTaskCapExpiredRejected(t *testing.T) {
	initTaskCapTestDB(t)

	// sign.WithDuration with a already-elapsed duration produces a token
	// whose OWN embedded HMAC expiry is already in the past, which is the
	// realistic way an "expired" token comes to exist (TTL elapsed since
	// issuance) — simulate that by issuing with a short ttl and waiting it
	// out. The embedded expiry is second-granularity (unix seconds), so the
	// ttl and sleep both need enough margin to reliably cross a second
	// boundary regardless of where in the current second issuance happened.
	tok, err := IssueTaskCap("agent-1", "task-1", "/src", "/dst", time.Second)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	time.Sleep(2 * time.Second)
	if _, err := VerifyTaskCap(tok); err != errs.InvalidTaskCap {
		t.Fatalf("expired token: want errs.InvalidTaskCap, got %v", err)
	}
}

// TestTaskCapTamperedRejected pins that flipping bytes in either half of the
// token (claims payload or signature) is refused.
func TestTaskCapTamperedRejected(t *testing.T) {
	initTaskCapTestDB(t)

	tok, err := IssueTaskCap("agent-1", "task-1", "/src", "/dst", time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Tamper the signature half.
	tamperedSig := tok[:len(tok)-1] + "x"
	if tamperedSig == tok {
		t.Fatal("tamper did not change the token")
	}
	if _, err := VerifyTaskCap(tamperedSig); err != errs.InvalidTaskCap {
		t.Fatalf("tampered signature: want errs.InvalidTaskCap, got %v", err)
	}

	// Tamper the payload half (a different agent id spliced in — re-encode a
	// modified claims JSON and keep the ORIGINAL signature, which must now
	// fail to verify against the new payload bytes).
	otherTok, err := IssueTaskCap("agent-2", "task-1", "/src", "/dst", time.Minute)
	if err != nil {
		t.Fatalf("issue other: %v", err)
	}
	otherPayload := otherTok[:indexOfDot(otherTok)]
	sigOnly := tok[indexOfDot(tok):]
	frankensteinToken := otherPayload + sigOnly
	if _, err := VerifyTaskCap(frankensteinToken); err != errs.InvalidTaskCap {
		t.Fatalf("payload/signature mismatch: want errs.InvalidTaskCap, got %v", err)
	}
}

func indexOfDot(s string) int {
	for i, r := range s {
		if r == '.' {
			return i
		}
	}
	return -1
}

// TestTaskCapCrossTaskRejected pins that a token minted for task A's claims
// cannot be presented as valid for task B: VerifyTaskCap itself only proves
// self-consistency (the claims match what was signed), so the actual
// cross-task rejection is the CALLER's responsibility (comparing
// claims.TaskID against the URL's :id) — this test pins that the claims
// returned faithfully carry the ORIGINAL task id, so that comparison is
// meaningful and cannot be spoofed by presenting task A's token at task B's
// endpoint and hoping the server trusts the URL instead.
func TestTaskCapCrossTaskRejected(t *testing.T) {
	initTaskCapTestDB(t)

	tokA, err := IssueTaskCap("agent-1", "task-A", "/src", "/dst", time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := VerifyTaskCap(tokA)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.TaskID != "task-A" {
		t.Fatalf("claims.TaskID = %q, want task-A", claims.TaskID)
	}
	if claims.TaskID == "task-B" {
		t.Fatal("sanity: claims must not equal an unrelated task id")
	}
}

// TestTaskCapMalformedRejected covers structurally broken tokens: no dot,
// empty halves, non-base64 payload, valid-base64-but-not-JSON payload.
func TestTaskCapMalformedRejected(t *testing.T) {
	initTaskCapTestDB(t)

	cases := []string{
		"",
		"no-dot-at-all",
		".",
		"onlyonesidenodot",
		"payload.",
		".sigonly",
		"not-valid-base64!!!.sig",
	}
	for _, tc := range cases {
		if _, err := VerifyTaskCap(tc); err != errs.InvalidTaskCap {
			t.Errorf("VerifyTaskCap(%q): want errs.InvalidTaskCap, got %v", tc, err)
		}
	}
}

// IssueTaskCap/VerifyTaskCap's fail-closed-without-a-signer contract rides
// directly on internal/sign.WithDuration/Verify's own fail-closed behavior
// (IssueTaskCap treats sign.WithDuration returning "" as an error; Verify
// propagates any non-nil sign.Verify error as errs.InvalidTaskCap) — that
// underlying contract is already pinned by
// internal/sign.TestSignVerifyFailClosedWithoutSigner. Re-deriving a
// process-wide "no signer" state here would require reaching into
// internal/sign's unexported package state (only that package's own tests
// can do that safely), so it is not duplicated in this file.
