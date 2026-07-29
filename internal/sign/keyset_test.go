package sign

import (
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	pkgsign "github.com/alist-org/alist/v3/pkg/sign"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func initSignTestDB(t *testing.T) {
	t.Helper()
	dB, err := gorm.Open(sqlite.Open("file:signkeyset_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		t.Fatal(err)
	}
	if err := Reload(); err != nil {
		t.Fatal(err)
	}
}

func TestKeysetSignVerifyAndRotationGrace(t *testing.T) {
	initSignTestDB(t)
	tok := Sign("/1/a")
	if err := Verify("/1/a", tok); err != nil {
		t.Fatalf("verify own sign: %v", err)
	}
	// simulate rotation: add a new active key, keep old as grace, Reload
	old, _ := db.GetActiveSigningKey(model.SignPurposeDownload)
	_ = db.DeactivateSigningKeys(model.SignPurposeDownload)
	_ = db.CreateSigningKey(&model.SigningKey{Purpose: model.SignPurposeDownload,
		KID: "rotated", Secret: "bmV3LXNlY3JldA==", Active: true, CreatedAt: 2})
	if err := Reload(); err != nil {
		t.Fatal(err)
	}
	// old token still verifies (grace), new tokens use the new key
	if err := Verify("/1/a", tok); err != nil {
		t.Fatalf("grace verify after rotation: %v", err)
	}
	newTok := Sign("/1/a")
	if newTok == tok {
		t.Fatal("post-rotation sign must use new active key")
	}
	// removing grace key invalidates old token
	_ = db.DeleteSigningKey(old.KID)
	if err := Reload(); err != nil {
		t.Fatal(err)
	}
	if err := Verify("/1/a", tok); err == nil {
		t.Fatal("after grace key removal old token must fail")
	}
}

// TestAdminTokenChangeDoesNotBreakSignatures pins the Task 3 decoupling: the
// download signer reads signing_keys, never conf.Token. Rotating the admin
// bearer token setting must not affect outstanding signatures, and no
// sign.Reload() is needed for that to hold since signing never consulted it.
func TestAdminTokenChangeDoesNotBreakSignatures(t *testing.T) {
	initSignTestDB(t)
	tok := Sign("/1/a")
	// simulate admin rotating the bearer token setting
	if err := db.SaveSettingItem(&model.SettingItem{Key: conf.Token, Value: "brand-new-admin-token"}); err != nil {
		t.Fatal(err)
	}
	// no sign.Reload() — the token is unrelated to signing now
	if err := Verify("/1/a", tok); err != nil {
		t.Fatalf("changing admin token must not invalidate download signatures: %v", err)
	}
}

// TestReloadReturnsErrorOnBuildFailure pins Change 1: Reload must surface a
// build failure to the caller instead of only logging it, so Rotate/GC can
// detect and report a signer that failed to come up after a db mutation.
func TestReloadReturnsErrorOnBuildFailure(t *testing.T) {
	dB, err := gorm.Open(sqlite.Open("file:signkeyset_reloadfail_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	// AutoMigrate only, no seed: signing_keys table exists but has no
	// active "download" key, so buildDownloadSigner's GetActiveSigningKey
	// call fails.
	if err := dB.AutoMigrate(&model.SigningKey{}); err != nil {
		t.Fatal(err)
	}
	if err := Reload(); err == nil {
		t.Fatal("Reload with no active key: expected non-nil error")
	}
}

// TestVerifyReloadsOnUnknownKid pins Change 2: a live process must pick up
// a key rotated in by another process/CLI without a restart. It mints a
// token under a kid that only exists in the db (never reloaded into this
// process's resolver) and checks that Verify's unknown-kid retry reloads
// and then succeeds.
func TestVerifyReloadsOnUnknownKid(t *testing.T) {
	initSignTestDB(t)
	// simulate an out-of-band rotation: another process/CLI adds a new key
	// "kid-b" to the db directly, never calling this process's Reload.
	secret := "b3V0LW9mLWJhbmQtc2VjcmV0"
	if err := db.CreateSigningKey(&model.SigningKey{
		Purpose: model.SignPurposeDownload, KID: "kid-b", Secret: secret,
		Active: false, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	// Mint a token under kid-b directly via pkg/sign, exactly as this
	// process's own signer would (same secret-encoding convention as
	// buildDownloadSigner: raw bytes of the base64 Secret string) but
	// without going through this process's resolver at all.
	bSigner := pkgsign.NewHMACSign([]byte(secret), "kid-b", func(string) ([]byte, bool) { return nil, false })
	tok := bSigner.Sign("/1/b", 0)

	// Bypass the reload throttle so the retry actually fires in this test.
	reloadMu.Lock()
	lastReload = time.Time{}
	reloadMu.Unlock()

	if err := Verify("/1/b", tok); err != nil {
		t.Fatalf("verify of out-of-band rotated kid must succeed after live reload: %v", err)
	}
}
