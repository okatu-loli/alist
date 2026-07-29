package sign

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestSignVerifyFailClosedWithoutSigner covers the legacy-mode gap: the
// signing_keys table exists (AutoMigrate ran) but is empty (the seed never
// ran), so get() has nothing to build a signer from and returns nil. Sign
// must degrade to "" instead of panicking, and Verify must fail closed with
// a non-nil error instead of panicking or (worse) reporting success.
func TestSignVerifyFailClosedWithoutSigner(t *testing.T) {
	dB, err := gorm.Open(sqlite.Open("file:signkeyset_nokey_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	// AutoMigrate only: mirrors LegacyAutoMigrate before the seed fix, or any
	// state where the signing_keys table exists but holds no active key.
	if err := dB.AutoMigrate(&model.SigningKey{}); err != nil {
		t.Fatal(err)
	}
	// Force the "process never built a signer" state explicitly rather than
	// relying on test run order: Reload() intentionally keeps the previous
	// signer when a rebuild fails (so a transient DB hiccup doesn't blow away
	// a working signer), which means a signer left behind by an earlier test
	// in this process would otherwise mask the nil-signer path this test
	// exists to cover.
	mu.Lock()
	downloadSigner = nil
	mu.Unlock()
	if err := Reload(); err == nil {
		t.Fatal("Reload with no active key: expected non-nil error")
	}

	tok := Sign("/1/a")
	if tok != "" {
		t.Fatalf("Sign with no signer available: expected empty string, got %q", tok)
	}

	if err := Verify("/1/a", "anything"); err == nil {
		t.Fatal("Verify with no signer available: expected non-nil error (fail closed), got nil")
	}
}
