package tokenstore

import (
	"errors"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	dB, err := gorm.Open(sqlite.Open("file:tokenstore_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Token{}); err != nil {
		t.Fatal(err)
	}
	old := db.GetDb()
	db.Init(dB)
	t.Cleanup(func() { db.Init(old) })
	return NewSQLiteStore()
}

func TestStoreSaveGetRevoke(t *testing.T) {
	s := newTestStore(t)
	future := int64(1 << 62)
	if err := s.Save(Record{JTI: "j1", UserID: 5, SessionID: "sess1", Kind: model.TokenKindAccess, ExpiresAt: future}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("j1"); err != nil {
		t.Fatalf("get saved: %v", err)
	}
	if _, err := s.Get("nope"); err == nil {
		t.Fatal("missing jti must error (fail closed)")
	}
	if err := s.Revoke("j1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("j1"); err == nil {
		t.Fatal("revoked jti must error")
	}
}

// TestGetDistinguishesNotFoundFromDBError checks that Get tells a genuine
// DB failure apart from "jti never existed". It uses its own uniquely-named
// in-memory db (not the "tokenstore_test" shared-cache db newTestStore
// hands out) so that closing the underlying *sql.DB to force a real error
// cannot destroy state any other test depends on.
func TestGetDistinguishesNotFoundFromDBError(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get("nope"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("missing jti: want ErrTokenNotFound, got %v", err)
	}

	dB, err := gorm.Open(sqlite.Open("file:tokenstore_dberr_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Token{}); err != nil {
		t.Fatal(err)
	}
	old := db.GetDb()
	db.Init(dB)
	t.Cleanup(func() { db.Init(old) })

	sqlDB, err := dB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = s.Get("anything")
	if err == nil {
		t.Fatal("closed-db query must still error (fail closed)")
	}
	if errors.Is(err, ErrTokenNotFound) {
		t.Fatal("a real DB error must NOT be reported as ErrTokenNotFound")
	}
}

func TestStoreRevokeBySessionAndUser(t *testing.T) {
	s := newTestStore(t)
	future := int64(1 << 62)
	_ = s.Save(Record{JTI: "a", UserID: 7, SessionID: "S", Kind: "access", ExpiresAt: future})
	_ = s.Save(Record{JTI: "b", UserID: 7, SessionID: "S", Kind: "refresh", ExpiresAt: future})
	_ = s.Save(Record{JTI: "c", UserID: 7, SessionID: "T", Kind: "access", ExpiresAt: future})
	if err := s.RevokeBySession("S"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("a"); err == nil {
		t.Fatal("a in session S must be revoked")
	}
	if _, err := s.Get("c"); err != nil {
		t.Fatal("c in session T must survive")
	}
	if err := s.RevokeByUser(7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("c"); err == nil {
		t.Fatal("c must be revoked by user sweep")
	}
}

func TestStoreRevokeIfActive(t *testing.T) {
	s := newTestStore(t)
	future := int64(1 << 62)
	if err := s.Save(Record{JTI: "cas", UserID: 9, SessionID: "S", Kind: model.TokenKindRefresh, ExpiresAt: future}); err != nil {
		t.Fatal(err)
	}
	won, err := s.RevokeIfActive("cas")
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("first RevokeIfActive on an active token must win")
	}
	won2, err := s.RevokeIfActive("cas")
	if err != nil {
		t.Fatal(err)
	}
	if won2 {
		t.Fatal("second RevokeIfActive on an already-revoked token must lose")
	}
	if _, err := s.Get("cas"); err == nil {
		t.Fatal("token must be revoked after RevokeIfActive wins")
	}
}

func TestStoreGCExpired(t *testing.T) {
	s := newTestStore(t)
	_ = s.Save(Record{JTI: "old", UserID: 1, SessionID: "S", Kind: "access", ExpiresAt: 1}) // long expired
	_ = s.Save(Record{JTI: "live", UserID: 1, SessionID: "S", Kind: "access", ExpiresAt: 1 << 62})
	removed, err := s.GC()
	if err != nil || removed < 1 {
		t.Fatalf("gc: %d %v", removed, err)
	}
	if _, err := s.Get("live"); err != nil {
		t.Fatal("live token must survive GC")
	}
}
