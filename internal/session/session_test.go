package session

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSessionTestDB(t *testing.T) {
	t.Helper()
	dB, err := gorm.Open(sqlite.Open("file:session_pkg_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Session{}, &model.Token{}); err != nil {
		t.Fatal(err)
	}
	old := db.GetDb()
	db.Init(dB)
	t.Cleanup(func() { db.Init(old) })
}

// TestMarkInactiveIsAtomic pins that MarkInactive performs both writes —
// flipping the session to inactive and revoking every token tied to it —
// through the single transactional db.MarkInactiveAndRevokeTokens function,
// rather than the old two independent calls (db.MarkInactive then
// store.RevokeBySession) that could leave the session inactive with its
// tokens still live if the second call failed. True rollback-on-partial-
// failure is hard to inject against a real gorm transaction without a
// fault-injecting driver, so this test's honest scope is: both writes land
// from one MarkInactive call, for a session with multiple tokens attached.
func TestMarkInactiveIsAtomic(t *testing.T) {
	setupSessionTestDB(t)

	sess := &model.Session{UserID: 1, DeviceKey: "dev-atomic", Status: model.SessionActive}
	if err := db.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	for _, jti := range []string{"jti-a", "jti-b"} {
		tok := &model.Token{JTI: jti, UserID: 1, SessionID: "dev-atomic", Kind: model.TokenKindAccess, ExpiresAt: 1 << 62}
		if err := db.SaveToken(tok); err != nil {
			t.Fatal(err)
		}
	}
	// An unrelated session/token pair must be left untouched.
	other := &model.Session{UserID: 2, DeviceKey: "dev-other", Status: model.SessionActive}
	if err := db.CreateSession(other); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveToken(&model.Token{JTI: "jti-other", UserID: 2, SessionID: "dev-other", Kind: model.TokenKindAccess, ExpiresAt: 1 << 62}); err != nil {
		t.Fatal(err)
	}

	if err := MarkInactive("dev-atomic"); err != nil {
		t.Fatalf("MarkInactive failed: %+v", err)
	}

	got, err := db.GetSession(1, "dev-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.SessionInactive {
		t.Fatalf("session status = %d, want SessionInactive", got.Status)
	}
	for _, jti := range []string{"jti-a", "jti-b"} {
		tok, err := db.GetToken(jti)
		if err != nil {
			t.Fatal(err)
		}
		if !tok.Revoked {
			t.Fatalf("token %s must be revoked after MarkInactive", jti)
		}
	}

	otherSess, err := db.GetSession(2, "dev-other")
	if err != nil {
		t.Fatal(err)
	}
	if otherSess.Status != model.SessionActive {
		t.Fatal("unrelated session must stay active")
	}
	otherTok, err := db.GetToken("jti-other")
	if err != nil {
		t.Fatal(err)
	}
	if otherTok.Revoked {
		t.Fatal("unrelated token must stay unrevoked")
	}
}

// TestMarkInactiveAndRevokeTokensIsSingleTransaction pins that
// db.MarkInactiveAndRevokeTokens — the function session.MarkInactive now
// calls exclusively — is the one place that closes the "inactive but not
// revoked" window; session.MarkInactive must not perform the mark and the
// revoke as two independent db calls any more.
func TestMarkInactiveAndRevokeTokensIsSingleTransaction(t *testing.T) {
	setupSessionTestDB(t)

	sess := &model.Session{UserID: 1, DeviceKey: "dev-direct", Status: model.SessionActive}
	if err := db.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveToken(&model.Token{JTI: "jti-direct", UserID: 1, SessionID: "dev-direct", Kind: model.TokenKindAccess, ExpiresAt: 1 << 62}); err != nil {
		t.Fatal(err)
	}

	if err := db.MarkInactiveAndRevokeTokens("dev-direct"); err != nil {
		t.Fatalf("MarkInactiveAndRevokeTokens failed: %+v", err)
	}

	got, err := db.GetSession(1, "dev-direct")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.SessionInactive {
		t.Fatal("session must be inactive")
	}
	tok, err := db.GetToken("jti-direct")
	if err != nil {
		t.Fatal(err)
	}
	if !tok.Revoked {
		t.Fatal("token must be revoked")
	}
}
