package db

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTokenTestDB(t *testing.T) {
	t.Helper()
	dB, err := gorm.Open(sqlite.Open("file:db_token_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Token{}); err != nil {
		t.Fatal(err)
	}
	old := GetDb()
	Init(dB)
	t.Cleanup(func() { Init(old) })
}

// TestRevokeTokenIfActiveIsCompareAndSwap pins the atomicity property the
// refresh-rotation race fix depends on: only the first caller to revoke a
// given jti gets true back, so two concurrent callers racing the same token
// can tell who "won" and treat the loser as a replay.
func TestRevokeTokenIfActiveIsCompareAndSwap(t *testing.T) {
	setupTokenTestDB(t)
	tok := &model.Token{JTI: "cas-1", UserID: 1, SessionID: "S", Kind: model.TokenKindRefresh, ExpiresAt: 1 << 62}
	if err := SaveToken(tok); err != nil {
		t.Fatal(err)
	}

	won, err := RevokeTokenIfActive("cas-1")
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("first RevokeTokenIfActive call on an active token must win")
	}

	won2, err := RevokeTokenIfActive("cas-1")
	if err != nil {
		t.Fatal(err)
	}
	if won2 {
		t.Fatal("second RevokeTokenIfActive call on an already-revoked token must lose")
	}
}

func TestRevokeTokenIfActiveMissingTokenLoses(t *testing.T) {
	setupTokenTestDB(t)
	won, err := RevokeTokenIfActive("does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("RevokeTokenIfActive on a nonexistent jti must not report a win")
	}
}
