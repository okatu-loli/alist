package bootstrap

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestRunTokenGCRemovesExpiredRows is a wiring smoke test: it doesn't
// re-prove GC's own deletion logic (covered by
// tokenstore.TestStoreGCExpired) — it proves runTokenGC, the function
// StartTokenGC's ticker actually calls, reaches a real store and has an
// effect, and that a nil store (tokenstore.Init not yet called) doesn't
// panic.
func TestRunTokenGCRemovesExpiredRows(t *testing.T) {
	dB, err := gorm.Open(sqlite.Open("file:bootstrap_token_gc_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Token{}); err != nil {
		t.Fatal(err)
	}
	oldDB := db.GetDb()
	db.Init(dB)
	oldStore := tokenstore.Get()
	tokenstore.Init(tokenstore.NewSQLiteStore())
	t.Cleanup(func() {
		db.Init(oldDB)
		tokenstore.Init(oldStore)
	})

	if err := tokenstore.Get().Save(tokenstore.Record{
		JTI: "expired", UserID: 1, SessionID: "S", Kind: model.TokenKindAccess, ExpiresAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	runTokenGC()

	if _, err := tokenstore.Get().Get("expired"); err == nil {
		t.Fatal("runTokenGC must have purged the expired row")
	}
}

func TestRunTokenGCWithNilStoreDoesNotPanic(t *testing.T) {
	oldStore := tokenstore.Get()
	tokenstore.Init(nil)
	t.Cleanup(func() { tokenstore.Init(oldStore) })
	runTokenGC() // must not panic
}
