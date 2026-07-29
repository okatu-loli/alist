package migratev4

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/bootstrap/data"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// migrateAndBoot runs the exact sequence docs/UPGRADE.md prescribes after
// `alist migrate v4` completes: start the v4 server against the freshly
// created data dir, i.e. run the normal boot sequence
// (internal/bootstrap/data.InitData). It returns the role rows that exist in
// the just-migrated-and-booted target database, read directly off the DB
// (bypassing every op-layer cache) so staleness in those caches cannot mask a
// missing/garbled row.
func migrateAndBoot(t *testing.T, root, name, knownDriver string) []model.Role {
	t.Helper()
	sourceDir := filepath.Join(root, name, "v3")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("%s: mkdir source: %v", name, err)
	}
	writeV3Fixture(t, sourceDir, knownDriver)

	target := filepath.Join(root, name, "v4")
	if _, err := Run(Options{Source: sourceDir, Target: target}); err != nil {
		t.Fatalf("%s: migrate: %v", name, err)
	}
	// initTarget (called from Run) already rewired the process's global db
	// handle at this to the target; InitData runs the same boot sequence a
	// real `alist server` invocation would run right after the migration.
	data.InitData()

	var roles []model.Role
	if err := db.GetDb().Find(&roles).Error; err != nil {
		t.Fatalf("%s: list roles directly off db: %v", name, err)
	}
	return roles
}

// TestChainedMigrateThenBootAcrossDatabases is the repro for the roleCache
// staleness hazard flagged in
// internal/migratev4/preview_port_w4_test.go's TestMigrateV4PreviewArchivesDefaultNotWrittenBySourcelessMigration
// comment: op's process-wide caches (internal/op/role.go's roleCache chief
// among them) are populated from whichever database the process's global db
// handle pointed at when the entry was cached, and are never invalidated when
// that handle is later repointed at a different database.
//
// This is not merely a test artifact: `alist migrate v4`'s initTarget()
// re-points the process's global db handle in-process
// (bootstrap.InitDB -> db.Init) exactly the way a second migration run in this
// test does, and docs/UPGRADE.md's prescribed next step — start the v4 server
// against the target, i.e. run data.InitData() — reads through those same
// caches. Any process that migrates (or reopens the db handle) more than
// once and boots after each open would hit the same corruption; this test
// merely forces that within a single test binary run.
//
// Before the fix: migrating+booting database B while database A's roles are
// still warm in roleCache makes ensureRole()/initRoles() believe guest/admin/
// general already exist (a cache hit returns A's rows without ever touching
// B), so migratev4 never creates any role row in B at all — B silently ends
// up with zero role rows even though Run() and InitData() both report
// success.
func TestChainedMigrateThenBootAcrossDatabases(t *testing.T) {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	knownDriver := firstDriverName(t)
	root := t.TempDir()

	rolesA := migrateAndBoot(t, root, "a", knownDriver)
	if len(rolesA) < 3 {
		t.Fatalf("setup: database a got %d role rows after migrate+boot, want >= 3 (guest/admin/general)", len(rolesA))
	}

	rolesB := migrateAndBoot(t, root, "b", knownDriver)
	if len(rolesB) < 3 {
		t.Fatalf("database b got %d role rows after migrate+boot, want >= 3 (guest/admin/general); "+
			"got %+v -- this is the roleCache cross-database staleness bug: ensureRole()/initRoles() "+
			"believed the roles already existed because database a's entries were still cached",
			len(rolesB), rolesB)
	}

	guest, err := op.GetRoleByName("guest")
	if err != nil {
		t.Fatalf("database b: GetRoleByName(guest) after boot: %v", err)
	}
	var dbGuest model.Role
	if err := db.GetDb().Where("id = ?", guest.ID).First(&dbGuest).Error; err != nil {
		t.Fatalf("database b: cached guest role id=%d does not exist in the database: %v", guest.ID, err)
	}
	if dbGuest.Name != "guest" {
		t.Fatalf("database b: cached guest role id=%d resolves to a different row in the database: %+v", guest.ID, dbGuest)
	}
}

// TestMigrateThenBootRolesAndSettingsCorrect is the integration scenario the
// previous wave (W4, docs/UPGRADE.md 迁移体验收尾) had to route around: a real
// v3 database migrated with migratev4.Run(), immediately followed by the
// normal v4 boot sequence (data.InitData()) in the same process — the exact
// step docs/UPGRADE.md prescribes right after `alist migrate v4` — asserting
// that both the carried settings and the role set (carried + built-in +
// preset) come out correct. It only needed the roleCache fix above to be
// possible at all: before it, initRoles()'s self-heal path and
// initPresetRoles() would corrupt/skip rows depending on what a prior test in
// this binary had already warmed the cache with.
func TestMigrateThenBootRolesAndSettingsCorrect(t *testing.T) {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	knownDriver := firstDriverName(t)
	root := t.TempDir()

	sourceDir := filepath.Join(root, "v3")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	validKey := writeV3Fixture(t, sourceDir, knownDriver)

	target := filepath.Join(root, "v4")
	if _, err := Run(Options{Source: sourceDir, Target: target}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The step right after `alist migrate v4`, per docs/UPGRADE.md: start the
	// v4 server against the freshly created data dir.
	data.InitData()

	// --- settings: the value carried from the v3 source survives boot's
	// initSettings backfill untouched, and a key v4 dropped never reappears. ---
	setting, err := db.GetSettingItemByKey(validKey)
	if err != nil {
		t.Fatalf("get carried setting %q after boot: %v", validKey, err)
	}
	if setting.Value != "migrated-value" {
		t.Errorf("carried setting %q clobbered by boot: got %q, want %q", validKey, setting.Value, "migrated-value")
	}
	if _, err := db.GetSettingItemByKey("obsolete_v3_only_key"); err == nil {
		t.Error("dropped v3-only setting key reappeared after boot")
	}

	// --- roles: the built-in guest/admin roles migratev4 created (Protected
	// unset) must be self-healed to Protected=true by initRoles(), and the
	// preset operator roles (viewer/editor/uploader/storage-manager/
	// user-manager) must exist, seeded by initPresetRoles(). ---
	for _, name := range []string{"guest", "admin"} {
		role, err := op.GetRoleByName(name)
		if err != nil {
			t.Fatalf("built-in role %q missing after boot: %v", name, err)
		}
		if !role.Protected {
			t.Errorf("built-in role %q not self-healed to Protected=true after boot", name)
		}
	}
	for _, name := range []string{"viewer", "editor", "uploader", "storage-manager", "user-manager"} {
		if _, err := op.GetRoleByName(name); err != nil {
			t.Errorf("preset role %q missing after boot: %v", name, err)
		}
	}

	// admin user (carried from the v3 source) must still resolve to the
	// admin role by ID, reading straight off the database.
	admin, err := db.GetUserByName("admin")
	if err != nil {
		t.Fatalf("get admin user after boot: %v", err)
	}
	adminRole, err := op.GetRoleByName("admin")
	if err != nil {
		t.Fatalf("get admin role after boot: %v", err)
	}
	adminRoleIDs, err := db.GetUserRoleIDs(admin.ID)
	if err != nil || !containsUint(adminRoleIDs, adminRole.ID) {
		t.Errorf("admin user not bound to admin role %d after boot: %v (err=%v)", adminRole.ID, adminRoleIDs, err)
	}
}
