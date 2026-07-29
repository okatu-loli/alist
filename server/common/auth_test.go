package common

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestStore(t *testing.T) {
	t.Helper()
	dB, err := gorm.Open(sqlite.Open("file:auth_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Token{}); err != nil {
		t.Fatal(err)
	}
	old := db.GetDb()
	db.Init(dB)
	oldStore := tokenstore.Get()
	tokenstore.Init(tokenstore.NewSQLiteStore())
	SecretKey = []byte("test-secret")
	t.Cleanup(func() {
		db.Init(old)
		tokenstore.Init(oldStore)
	})
}

func TestTokenSurvivesProcessRestart(t *testing.T) {
	setupTestStore(t)
	// simulate restart: nothing clears the persistent store; parse must
	// still pass even though the old in-memory validTokenCache would have
	// lost the entry.
	u := &model.User{Username: "alice", PwdTS: 1}
	tok, err := GenerateToken(u, "sess-x")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ParseToken(tok)
	if err != nil || claims.JTI == "" || claims.SessionID != "sess-x" {
		t.Fatalf("token must survive restart via store: %v %+v", err, claims)
	}
}

func TestRevokedTokenRejected(t *testing.T) {
	setupTestStore(t)
	u := &model.User{Username: "bob", PwdTS: 1}
	tok, _ := GenerateToken(u, "sess-y")
	claims, _ := ParseToken(tok)
	if err := tokenstore.Get().Revoke(claims.JTI); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(tok); err == nil {
		t.Fatal("revoked token must be rejected")
	}
}

func TestParseFailsClosedWithoutStore(t *testing.T) {
	setupTestStore(t)
	u := &model.User{Username: "carol", PwdTS: 1}
	tok, err := GenerateToken(u, "sess-z")
	if err != nil {
		t.Fatal(err)
	}
	oldStore := tokenstore.Get()
	tokenstore.Init(nil)
	defer tokenstore.Init(oldStore)
	if _, err := ParseToken(tok); err == nil {
		t.Fatal("ParseToken must reject when tokenstore is not initialized (fail closed)")
	}
}

func TestInvalidateTokenRevokesInStore(t *testing.T) {
	setupTestStore(t)
	u := &model.User{Username: "dave", PwdTS: 1}
	tok, err := GenerateToken(u, "sess-w")
	if err != nil {
		t.Fatal(err)
	}
	if err := InvalidateToken(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(tok); err == nil {
		t.Fatal("invalidated token must be rejected")
	}
}

func TestInvalidateEmptyTokenIsNoop(t *testing.T) {
	setupTestStore(t)
	if err := InvalidateToken(""); err != nil {
		t.Fatalf("invalidating empty guest token must be a no-op: %v", err)
	}
}

// TestParseRejectsRefreshTokenAsAccess pins that a refresh token cannot be
// used as a bearer token: it is structurally identical to an access token
// and its jti is equally present in the store, so ParseToken must consult
// rec.Kind rather than accepting anything the store recognizes.
func TestParseRejectsRefreshTokenAsAccess(t *testing.T) {
	setupTestStore(t)
	u := &model.User{Username: "erin", PwdTS: 1}
	refreshTok, err := GenerateRefreshToken(u, "sess-refresh", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(refreshTok); err == nil {
		t.Fatal("ParseToken must reject a refresh token presented as a bearer token")
	}

	// Access tokens must still be accepted, confirming the check is
	// Kind-specific rather than breaking ParseToken generally.
	accessTok, err := GenerateToken(u, "sess-access")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(accessTok); err != nil {
		t.Fatalf("access token must still be accepted: %v", err)
	}
}
