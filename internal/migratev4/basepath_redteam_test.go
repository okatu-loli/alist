package migratev4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/pkg/errors"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	_ "github.com/alist-org/alist/v3/drivers"
)

// rtUser is a v3 users row whose `role` column is the 3.46+ JSON []int form, so
// the fixture can bind a user to MORE than one role (the base fixture's fxUser
// only carries a single pre-3.46 integer role).
type rtUser struct {
	ID         uint `gorm:"primaryKey"`
	Username   string
	PwdHash    string
	PwdTS      int64
	Salt       string
	Password   string
	BasePath   string
	Role       string `gorm:"column:role"` // JSON e.g. "[0,10]"
	Disabled   bool
	Permission int32
	Authn      string
}

func (rtUser) TableName() string { return "x_users" }

// rtRole is a v3 roles row (3.46+). RawPermission is the JSON PermissionScopes
// blob op.CreateRole converts on carry.
type rtRole struct {
	ID            uint `gorm:"primaryKey"`
	Name          string
	Description   string
	Default       bool
	RawPermission string `gorm:"column:raw_permission"`
}

func (rtRole) TableName() string { return "x_roles" }

// TestMigrateBasePathFenceSurvivesExtraRole is the W4 red-team headline for
// attack surface #4: a v3 user whose base_path fenced them to a subtree, but who
// ALSO carried a custom role granting broad access, must STILL be fenced after
// migration.
//
// v3 base_path was an ABSOLUTE confinement — no role could let the user escape
// that subtree, because every request path was physically prefixed by base_path.
// In v4 there is no BasePath and every role scope is an independent grant, so the
// migrator must neutralise EVERY out-of-fence grant, not just the built-in
// read-all "general" role. Here "carol" is fenced to /home/carol but also holds
// a custom "poweruser" role scoped to "/" (recursive read-all). If the migrator
// only drops "general" (dropRoleID(general)), the poweruser role survives and
// re-opens read-all — silently downgrading the fence.
//
// Load-bearing: revert writeUsers to `roleIDs = dropRoleID(roleIDs, generalID)`
// and this goes red (carol can read /other).
func TestMigrateBasePathFenceSurvivesExtraRole(t *testing.T) {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}

	root := t.TempDir()
	sourceDir := filepath.Join(root, "v3")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	writeFenceFixture(t, sourceDir)

	target := filepath.Join(root, "v4")
	if _, err := Run(Options{Source: sourceDir, Target: target}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	carol, err := db.GetUserByName("carol")
	if err != nil {
		t.Fatalf("get carol: %v", err)
	}

	scopeRole, err := op.GetRoleByName("migrated-scope:carol")
	if err != nil {
		t.Fatalf("carol base_path scope role missing: %v", err)
	}
	powerRole, err := op.GetRoleByName("poweruser")
	if err != nil {
		t.Fatalf("poweruser role should still exist as a role row: %v", err)
	}
	general, err := op.GetRoleByName("general")
	if err != nil {
		t.Fatalf("general role missing: %v", err)
	}

	// carol must be bound ONLY to the private scope role: the broad poweruser
	// role and the read-all general role must both be stripped from a fenced user.
	roleIDs, err := db.GetUserRoleIDs(carol.ID)
	if err != nil {
		t.Fatalf("carol role ids: %v", err)
	}
	if hasUint(roleIDs, powerRole.ID) {
		t.Errorf("fenced carol must NOT keep the broad 'poweruser' role %d: %v", powerRole.ID, roleIDs)
	}
	if hasUint(roleIDs, general.ID) {
		t.Errorf("fenced carol must NOT keep the read-all 'general' role %d: %v", general.ID, roleIDs)
	}
	if !hasUint(roleIDs, scopeRole.ID) {
		t.Errorf("fenced carol must be bound to her private scope role %d: %v", scopeRole.ID, roleIDs)
	}

	ctx := context.Background()
	// Inside the fence: allowed.
	if err := op.Authorize(ctx, carol, model.ActRead, op.AuthzRequest{Path: "/home/carol"}); err != nil {
		t.Errorf("carol should read within her fence: %v", err)
	}
	if err := op.Authorize(ctx, carol, model.ActRead, op.AuthzRequest{Path: "/home/carol/sub/f"}); err != nil {
		t.Errorf("carol should read below her fence: %v", err)
	}
	// THE HEADLINE: a SIBLING outside the fence must be denied even though the
	// surviving broad role would have granted read-all. (Ancestors like "/" and
	// "/home" stay readable via Traverse — that is how the user navigates DOWN to
	// the fence — but the listing filter below proves no sibling content leaks.)
	if err := op.Authorize(ctx, carol, model.ActRead, op.AuthzRequest{Path: "/other"}); !errors.Is(err, errs.PermissionDenied) {
		t.Errorf("fenced carol must be DENIED outside her fence (/other), got %v", err)
	}
	if err := op.Authorize(ctx, carol, model.ActWrite, op.AuthzRequest{Path: "/other"}); !errors.Is(err, errs.PermissionDenied) {
		t.Errorf("fenced carol must be DENIED write outside her fence (/other), got %v", err)
	}

	// Listing does not leak siblings: at "/" only the "home" waypoint (leading to
	// the fence) survives; at "/home" only "carol" survives. If the broad
	// poweruser role had survived, "other"/"bob" would leak here.
	rootObjs := []model.Obj{
		&model.Object{Name: "home", IsFolder: true},
		&model.Object{Name: "other", IsFolder: true},
	}
	if got := op.FilterListing(ctx, carol, "/", rootObjs); len(got) != 1 || got[0].GetName() != "home" {
		t.Errorf("listing / must show only [home] waypoint, got %v", objNames(got))
	}
	homeObjs := []model.Obj{
		&model.Object{Name: "carol", IsFolder: true},
		&model.Object{Name: "bob", IsFolder: true},
	}
	if got := op.FilterListing(ctx, carol, "/home", homeObjs); len(got) != 1 || got[0].GetName() != "carol" {
		t.Errorf("listing /home must show only [carol], got %v", objNames(got))
	}
}

func objNames(objs []model.Obj) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetName())
	}
	return out
}

// writeFenceFixture builds a minimal v3 sqlite database + config.json: a roles
// table with a broad "poweruser" role (scope "/" => read-all) and a user "carol"
// fenced by base_path /home/carol but bound to [general, poweruser].
func writeFenceFixture(t *testing.T, dir string) {
	t.Helper()
	dbPath := filepath.Join(dir, "data.db")
	src, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	if err := src.AutoMigrate(&rtUser{}, &rtRole{}); err != nil {
		t.Fatalf("migrate fixture: %v", err)
	}

	// Custom v3 role id=10: a single PermissionScope on "/" (recursive read-all
	// once op.CreateRole's scope conversion appends ActRead).
	if err := src.Create(&rtRole{
		ID: 10, Name: "poweruser", Description: "broad access",
		RawPermission: `[{"path":"/","permission":0}]`,
	}).Error; err != nil {
		t.Fatalf("seed role: %v", err)
	}

	carolSalt := "carolsalt00000000"
	if err := src.Create(&rtUser{
		ID: 5, Username: "carol", Salt: carolSalt,
		PwdHash: model.TwoHashPwd("carolpass", carolSalt),
		PwdTS:   333, BasePath: "/home/carol",
		Role: "[0,10]", // GENERAL + poweruser
	}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	cfg := `{"database":{"type":"sqlite3","db_file":"data.db","table_prefix":"x_"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	if sqlDB, err := src.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

func hasUint(ss []uint, want uint) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
