package migratev4

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/bootstrap/data"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	// Register all drivers so op.GetDriver recognises the "known" storage's
	// driver and rejects the fabricated unknown one.
	_ "github.com/alist-org/alist/v3/drivers"
)

// --- v3 fixture structs. TableName() pins the fully-prefixed v3 table names so
// the reader (which applies the "x_" prefix via NamingStrategy) finds them.

type fxUser struct {
	ID         uint `gorm:"primaryKey"`
	Username   string
	PwdHash    string
	PwdTS      int64
	Salt       string
	Password   string
	BasePath   string
	Role       int // pre-3.46 integer role column (0=general,1=guest,2=admin)
	Disabled   bool
	Permission int32
	Authn      string
}

func (fxUser) TableName() string { return "x_users" }

type fxStorage struct {
	ID        uint `gorm:"primaryKey"`
	MountPath string
	Driver    string
	Status    string
	Addition  string
	Modified  time.Time
	Disabled  bool
}

func (fxStorage) TableName() string { return "x_storages" }

type fxMeta struct {
	ID       uint `gorm:"primaryKey"`
	Path     string
	Password string
	PSub     bool
}

// gorm's inflection treats "meta" as already plural, so the table is "x_meta"
// (not "x_metas") — matching model.Meta and the legacy reader.
func (fxMeta) TableName() string { return "x_meta" }

type fxSetting struct {
	Key   string `gorm:"column:key;primaryKey"`
	Value string
}

func (fxSetting) TableName() string { return "x_setting_items" }

type fxShare struct {
	ID       uint `gorm:"primaryKey"`
	ShareID  string
	RootPath string
}

func (fxShare) TableName() string { return "x_shares" }

// writeV3Fixture builds a v3-shaped sqlite database + config.json in dir and
// returns the chosen valid v4 setting key it seeded a custom value for.
func writeV3Fixture(t *testing.T, dir, knownDriver string) (validKey string) {
	t.Helper()
	dbPath := filepath.Join(dir, "data.db")
	src, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	if err := src.AutoMigrate(&fxUser{}, &fxStorage{}, &fxMeta{}, &fxSetting{}, &fxShare{}); err != nil {
		t.Fatalf("migrate fixture: %v", err)
	}

	adminSalt := "adminsalt00000000"
	aliceSalt := "alicesalt00000000"
	users := []fxUser{
		{ID: 1, Username: "admin", Salt: adminSalt, PwdHash: model.TwoHashPwd("adminpass", adminSalt),
			PwdTS: 111, BasePath: "/", Role: model.ADMIN, Permission: 0xFFFF},
		{ID: 2, Username: "alice", Salt: aliceSalt, PwdHash: model.TwoHashPwd("alicepass", aliceSalt),
			PwdTS: 222, BasePath: "/home/alice", Role: model.GENERAL},
	}
	if err := src.Create(&users).Error; err != nil {
		t.Fatalf("seed users: %v", err)
	}

	storages := []fxStorage{
		{ID: 1, MountPath: "/local", Driver: knownDriver, Status: "work", Addition: "{}"},
		{ID: 2, MountPath: "/ghost", Driver: "NonexistentDriverXYZ", Status: "work", Addition: "{}"},
	}
	if err := src.Create(&storages).Error; err != nil {
		t.Fatalf("seed storages: %v", err)
	}

	if err := src.Create(&fxMeta{ID: 1, Path: "/secret", Password: "letmein", PSub: true}).Error; err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	// Pick a real v4 setting key to carry, plus a made-up dropped one.
	for _, item := range data.InitialSettings() {
		if item.Key != "version" {
			validKey = item.Key
			break
		}
	}
	settings := []fxSetting{
		{Key: validKey, Value: "migrated-value"},
		{Key: "obsolete_v3_only_key", Value: "gone"},
		// preview_archives_by_default (v3→v4 preview-management port):
		// v3 already ships this key with default "true" — a source dump
		// customizing it to "false" must survive the migration unchanged now
		// that the key is registered in data.InitialSettings(), not fall into
		// settingsDropped the way it did before this key existed on v4.
		{Key: conf.PreviewArchivesByDefault, Value: "false"},
	}
	if err := src.Create(&settings).Error; err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := src.Create(&fxShare{ID: 1, ShareID: "abc123", RootPath: "/pub"}).Error; err != nil {
		t.Fatalf("seed share: %v", err)
	}

	// config.json so the reader can resolve the source as a v3 data dir.
	cfg := `{"database":{"type":"sqlite3","db_file":"data.db","table_prefix":"x_"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	if sqlDB, err := src.DB(); err == nil {
		_ = sqlDB.Close()
	}
	return validKey
}

func TestMigrateV4EndToEnd(t *testing.T) {
	// InitialSettings (used by the fixture helper and by Run) reads defaults off
	// conf.Conf; seed it before anything touches it.
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

	// --- dry-run first: computes the report, creates no target ---
	dryTarget := filepath.Join(root, "v4-dry")
	dryReport, err := Run(Options{Source: sourceDir, Target: dryTarget, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !dryReport.DryRun {
		t.Error("dry-run report not marked DryRun")
	}
	if _, err := os.Stat(filepath.Join(dryTarget, "data.db")); !os.IsNotExist(err) {
		t.Errorf("dry-run created a target database (err=%v)", err)
	}
	if dryReport.UsersCarried != 2 || dryReport.MetasCarried != 1 || dryReport.StoragesCarried != 2 {
		t.Errorf("dry-run counts wrong: %+v", dryReport)
	}
	if len(dryReport.StoragesDisabled) != 1 {
		t.Errorf("dry-run expected 1 disabled storage, got %v", dryReport.StoragesDisabled)
	}
	if dryReport.NotCarried["shares"] != 1 {
		t.Errorf("dry-run expected shares=1 not-carried, got %v", dryReport.NotCarried)
	}
	if !contains(dryReport.SettingsDropped, "obsolete_v3_only_key") {
		t.Errorf("dry-run should report dropped key, got %v", dryReport.SettingsDropped)
	}
	if contains(dryReport.SettingsDropped, conf.PreviewArchivesByDefault) {
		t.Errorf("preview_archives_by_default should be carried, not dropped: %v", dryReport.SettingsDropped)
	}

	// --- real run ---
	target := filepath.Join(root, "v4")
	report, err := Run(Options{Source: sourceDir, Target: target})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "data.db")); err != nil {
		t.Fatalf("target database not created: %v", err)
	}

	assertTargetContents(t, report, validKey)

	// --- idempotence / refusal: a second run onto the populated target fails ---
	if _, err := Run(Options{Source: sourceDir, Target: target}); err == nil {
		t.Error("second run onto populated target should have been refused")
	}
}

// assertTargetContents opens the just-migrated target (global db now points at
// it) and checks the migrated data through the real v4 db/op layer.
func assertTargetContents(t *testing.T, report *Report, validKey string) {
	t.Helper()

	admin, err := db.GetUserByName("admin")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	if !admin.IsAdmin() {
		t.Errorf("admin user not bound to admin role: %v", admin.Role)
	}
	if err := admin.ValidateRawPassword("adminpass"); err != nil {
		t.Errorf("admin password not carried: %v", err)
	}
	adminRoleIDs, err := db.GetUserRoleIDs(admin.ID)
	if err != nil || !containsUint(adminRoleIDs, uint(model.ADMIN)) {
		t.Errorf("admin user_roles binding wrong: %v (err=%v)", adminRoleIDs, err)
	}

	alice, err := db.GetUserByName("alice")
	if err != nil {
		t.Fatalf("get alice: %v", err)
	}
	// W2: the v3 base_path (/home/alice) is no longer carried onto a
	// User.BasePath field (removed in W1); it is converted into a private
	// "migrated-scope:alice" role that fences alice. Because the built-in
	// "general" role grants recursive read on "/", a fenced user must NOT keep
	// it — so alice is bound to the scope role INSTEAD of general. The fence's
	// Authorize behaviour is asserted in TestMigrateBasePathBecomesScope.
	scopeRole, err := op.GetRoleByName("migrated-scope:alice")
	if err != nil {
		t.Fatalf("alice base_path scope role missing: %v", err)
	}
	aliceRoleIDs, err := db.GetUserRoleIDs(alice.ID)
	if err != nil || !containsUint(aliceRoleIDs, scopeRole.ID) {
		t.Errorf("alice not bound to base_path scope role %d: %v (err=%v)", scopeRole.ID, aliceRoleIDs, err)
	}
	generalRole, err := op.GetRoleByName("general")
	if err != nil {
		t.Fatalf("general role missing: %v", err)
	}
	if containsUint(aliceRoleIDs, generalRole.ID) {
		t.Errorf("fenced alice must NOT keep the read-all general role %d: %v", generalRole.ID, aliceRoleIDs)
	}

	// storages: known driver enabled, unknown driver disabled.
	local, err := db.GetStorageByMountPath("/local")
	if err != nil {
		t.Fatalf("get /local storage: %v", err)
	}
	if local.Disabled {
		t.Error("known-driver storage should not be disabled")
	}
	ghost, err := db.GetStorageByMountPath("/ghost")
	if err != nil {
		t.Fatalf("get /ghost storage: %v", err)
	}
	if !ghost.Disabled {
		t.Error("unknown-driver storage should be imported disabled")
	}

	meta, err := db.GetMetaByPath("/secret")
	if err != nil {
		t.Fatalf("get meta: %v", err)
	}
	if meta.Password != "letmein" {
		t.Errorf("meta password not carried: %q", meta.Password)
	}

	setting, err := db.GetSettingItemByKey(validKey)
	if err != nil {
		t.Fatalf("get carried setting %q: %v", validKey, err)
	}
	if setting.Value != "migrated-value" {
		t.Errorf("carried setting value wrong: %q", setting.Value)
	}
	if _, err := db.GetSettingItemByKey("obsolete_v3_only_key"); err == nil {
		t.Error("dropped setting key should not exist on target")
	}

	archivesPrior, err := db.GetSettingItemByKey(conf.PreviewArchivesByDefault)
	if err != nil {
		t.Fatalf("get carried %s: %v", conf.PreviewArchivesByDefault, err)
	}
	if archivesPrior.Value != "false" {
		t.Errorf("%s not carried: got %q, want %q", conf.PreviewArchivesByDefault, archivesPrior.Value, "false")
	}

	// report consistency
	if report.UsersCarried != 2 || report.MetasCarried != 1 || report.StoragesCarried != 2 {
		t.Errorf("report counts wrong: %+v", report)
	}
	if report.NotCarried["shares"] != 1 {
		t.Errorf("report shares not-carried wrong: %v", report.NotCarried)
	}
}

// TestMigrateBasePathBecomesScope is the W2 red-team headline: a v3 user with a
// non-"/" base_path must remain fenced after migration. The v3 base_path becomes
// a private RBAC scope role; reads inside the old base_path are allowed, reads
// outside are denied. A user whose base_path was "/" (or empty) gets no scope
// role.
func TestMigrateBasePathBecomesScope(t *testing.T) {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	knownDriver := firstDriverName(t)

	root := t.TempDir()
	sourceDir := filepath.Join(root, "v3")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	// alice: base_path /home/alice (fenced); admin: base_path "/" (unfenced).
	writeV3Fixture(t, sourceDir, knownDriver)

	target := filepath.Join(root, "v4")
	if _, err := Run(Options{Source: sourceDir, Target: target}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// ① alice's v3 base_path became a permission scope covering /home/alice.
	scopeRole, err := op.GetRoleByName("migrated-scope:alice")
	if err != nil {
		t.Fatalf("alice base_path scope role missing: %v", err)
	}
	foundScope := false
	for _, e := range scopeRole.PermissionScopes {
		if e.Path == "/home/alice" {
			foundScope = true
		}
	}
	if !foundScope {
		t.Errorf("scope role missing /home/alice entry: %+v", scopeRole.PermissionScopes)
	}

	alice, err := db.GetUserByName("alice")
	if err != nil {
		t.Fatalf("get alice: %v", err)
	}
	bound := false
	for _, rid := range alice.Role {
		if uint(rid) == scopeRole.ID {
			bound = true
		}
	}
	if !bound {
		t.Errorf("alice not bound to scope role %d: %v", scopeRole.ID, alice.Role)
	}

	ctx := context.Background()
	// ② read inside the old base_path fence is allowed (scope coverage implies read).
	if err := op.Authorize(ctx, alice, model.ActRead, op.AuthzRequest{Path: "/home/alice"}); err != nil {
		t.Errorf("alice should be allowed to read within her base_path fence: %v", err)
	}
	if err := op.Authorize(ctx, alice, model.ActRead, op.AuthzRequest{Path: "/home/alice/sub/file"}); err != nil {
		t.Errorf("alice should be allowed to read below her base_path fence: %v", err)
	}
	// ③ read OUTSIDE the fence is denied — the red-team headline assertion.
	if err := op.Authorize(ctx, alice, model.ActRead, op.AuthzRequest{Path: "/other"}); err == nil {
		t.Error("alice must be denied outside her v3 base_path fence (/other)")
	}

	// ④ admin's base_path was "/" — no scope role is created for it.
	if _, err := op.GetRoleByName("migrated-scope:admin"); err == nil {
		t.Error("admin base_path '/' must not create a scope role")
	}
}

func firstDriverName(t *testing.T) string {
	t.Helper()
	names := op.GetDriverNames()
	if len(names) == 0 {
		t.Fatal("no drivers registered")
	}
	return names[0]
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func containsUint(ss []uint, want uint) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
