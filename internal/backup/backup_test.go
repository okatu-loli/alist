package backup_test

import (
	"bytes"
	"testing"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/backup"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/bootstrap/data"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// boot spins a throwaway sqlite database in a temp dir and seeds the preset
// data (admin/guest roles+users, settings), then a small custom dataset that
// exercises every domain and both binding kinds. op.ResetCachesForTest runs
// right after InitDB and before any seeding, because another test in this
// process may have booted a different database whose ids are cached.
func boot(t *testing.T) {
	t.Helper()
	oldDataDir, oldMode := flags.DataDir, flags.MigrateMode
	t.Cleanup(func() { flags.DataDir, flags.MigrateMode = oldDataDir, oldMode })
	flags.DataDir = t.TempDir()
	flags.MigrateMode = "auto"
	bootstrap.InitConfig()
	bootstrap.InitDB()
	op.ResetCachesForTest()
	data.InitData()
	seedCustom(t)
}

// seedCustom adds a storage, a meta anchored in it, a custom role, a manual
// permission bound to that role, and a user in that role — the cross-references
// (meta.storage_id, user.Role, user_roles, role_permissions) a restore must
// keep consistent.
func seedCustom(t *testing.T) {
	t.Helper()
	g := db.GetDb()
	st := model.Storage{MountPath: "/bk", Driver: "local", Status: "work",
		Addition: `{"root_folder_path":"/tmp/bk","secret":"top"}`}
	if err := g.Create(&st).Error; err != nil {
		t.Fatalf("seed storage: %v", err)
	}
	m := model.Meta{Path: "/bk/secret", Password: "pw", PSub: true}
	if err := g.Create(&m).Error; err != nil {
		t.Fatalf("seed meta: %v", err)
	}
	role := model.Role{Name: "bk_tester", Description: "test editor"}
	if err := g.Create(&role).Error; err != nil {
		t.Fatalf("seed role: %v", err)
	}
	perm := model.Permission{Name: "perm-editor", ResourceType: model.ResourceFS,
		Path: "/bk", Actions: model.Actions{model.ActRead, model.ActWrite}, Effect: model.EffectAllow,
		Recursive: true, Origin: model.OriginManual}
	if err := g.Create(&perm).Error; err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	if err := g.Create(&model.RolePermission{RoleID: role.ID, PermissionID: perm.ID}).Error; err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	u := (&model.User{Username: "bk_user1", Role: model.Roles{int(role.ID)}}).SetPassword("secret-pass")
	if err := g.Create(u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.SetUserRoles(u.ID, []uint{role.ID}); err != nil {
		t.Fatalf("seed user_roles: %v", err)
	}
}

// roundTrip writes the current db to an in-memory archive and decodes it back.
func roundTrip(t *testing.T, opts backup.Options) *backup.Archive {
	t.Helper()
	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var buf bytes.Buffer
	if _, err := backup.Write(&buf, arc, opts); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := backup.Read(&buf, opts.Password)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

func metaByPath(t *testing.T, path string) *model.Meta {
	t.Helper()
	var m model.Meta
	if err := db.GetDb().Where("path = ?", path).First(&m).Error; err != nil {
		t.Fatalf("meta %s: %v", path, err)
	}
	return &m
}

func userByName(t *testing.T, name string) *model.User {
	t.Helper()
	var u model.User
	if err := db.GetDb().Where("username = ?", name).First(&u).Error; err != nil {
		t.Fatalf("user %s: %v", name, err)
	}
	return &u
}

func countRows(t *testing.T, model any) int64 {
	t.Helper()
	var n int64
	if err := db.GetDb().Model(model).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestOverwriteRoundTrip(t *testing.T) {
	boot(t)

	// Archive the pristine state.
	var buf bytes.Buffer
	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := backup.Write(&buf, arc, backup.Options{}); err != nil {
		t.Fatalf("write: %v", err)
	}

	origPwdHash := userByName(t, "bk_user1").PwdHash

	// Mutate: corrupt a meta password, drop the custom user + its binding.
	if err := db.GetDb().Model(&model.Meta{}).Where("path = ?", "/bk/secret").
		Update("password", "CHANGED").Error; err != nil {
		t.Fatalf("mutate meta: %v", err)
	}
	uid := userByName(t, "bk_user1").ID
	db.GetDb().Where("user_id = ?", uid).Delete(&model.UserRole{})
	db.GetDb().Where("username = ?", "bk_user1").Delete(&model.User{})

	// Restore (overwrite) from the pristine archive.
	restored, err := backup.Read(&buf, "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	diff, err := backup.Apply(restored, backup.StrategyOverwrite, false)
	if err != nil {
		t.Fatalf("apply overwrite: %v", err)
	}
	if diff.DryRun {
		t.Fatal("real restore reported dry_run")
	}

	// Equivalence: natural keys back, the mutated field reset, and the user's
	// role membership rebuilt from the name-resolved binding.
	if got := metaByPath(t, "/bk/secret").Password; got != "pw" {
		t.Errorf("meta password not restored: got %q", got)
	}
	u := userByName(t, "bk_user1")
	if u.PwdHash != origPwdHash {
		t.Errorf("user pwd hash not restored: got %q want %q", u.PwdHash, origPwdHash)
	}
	editorRole := roleByName(t, "bk_tester")
	if len(u.Role) != 1 || u.Role[0] != int(editorRole.ID) {
		t.Errorf("user role membership not rebuilt: got %v want [%d]", u.Role, editorRole.ID)
	}
}

func TestMergeUpsert(t *testing.T) {
	boot(t)
	var buf bytes.Buffer
	arc, _ := backup.Collect(db.GetDb())
	if _, err := backup.Write(&buf, arc, backup.Options{}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A setting present in the archive is mutated (should be updated back); a
	// brand-new setting absent from the archive is added (merge must keep it).
	if err := db.GetDb().Model(&model.SettingItem{}).Where("`key` = ?", "version").
		Update("value", "MUTATED").Error; err != nil {
		// key column name differs per dialect quoting; fall back to a portable update
		db.GetDb().Model(&model.SettingItem{}).Where("key = ?", "version").Update("value", "MUTATED")
	}
	db.GetDb().Create(&model.SettingItem{Key: "custom_extra", Value: "keep-me"})
	// Drop the custom user so merge re-creates it.
	uid := userByName(t, "bk_user1").ID
	db.GetDb().Where("user_id = ?", uid).Delete(&model.UserRole{})
	db.GetDb().Where("username = ?", "bk_user1").Delete(&model.User{})

	restored, _ := backup.Read(&buf, "")
	if _, err := backup.Apply(restored, backup.StrategyMerge, false); err != nil {
		t.Fatalf("apply merge: %v", err)
	}

	// archive setting restored...
	var vs model.SettingItem
	db.GetDb().Where("key = ?", "version").First(&vs)
	if vs.Value == "MUTATED" {
		t.Error("merge did not update the mutated archive setting")
	}
	// ...merge-only extra survived (merge never deletes)...
	if countRows(t, &model.SettingItem{}) == 0 {
		t.Fatal("no settings")
	}
	var extra int64
	db.GetDb().Model(&model.SettingItem{}).Where("key = ?", "custom_extra").Count(&extra)
	if extra != 1 {
		t.Error("merge deleted a db-only setting")
	}
	// ...and the user came back with its role.
	u := userByName(t, "bk_user1")
	if len(u.Role) != 1 {
		t.Errorf("merge did not rebuild recreated user's roles: %v", u.Role)
	}
}

func TestDryRunMatchesRealAndWritesNothing(t *testing.T) {
	boot(t)
	var buf bytes.Buffer
	arc, _ := backup.Collect(db.GetDb())
	backup.Write(&buf, arc, backup.Options{})

	// Drop the custom user: a restore will "create" it.
	uid := userByName(t, "bk_user1").ID
	db.GetDb().Where("user_id = ?", uid).Delete(&model.UserRole{})
	db.GetDb().Where("username = ?", "bk_user1").Delete(&model.User{})
	usersBefore := countRows(t, &model.User{})

	restored, _ := backup.Read(&buf, "")
	dry, err := backup.Apply(restored, backup.StrategyMerge, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !dry.DryRun {
		t.Error("dry-run diff missing dry_run flag")
	}
	if got := countRows(t, &model.User{}); got != usersBefore {
		t.Errorf("dry-run wrote to the db: users %d -> %d", usersBefore, got)
	}
	if !contains(dry.Domains["users"].Create, "bk_user1") {
		t.Errorf("dry-run diff should create editor1, got %+v", dry.Domains["users"])
	}

	// A real run must apply exactly the same diff.
	real, err := backup.Apply(restored, backup.StrategyMerge, false)
	if err != nil {
		t.Fatalf("real: %v", err)
	}
	if !sameDomainDiff(dry.Domains["users"], real.Domains["users"]) {
		t.Errorf("dry vs real users diff differ:\n dry=%+v\nreal=%+v",
			dry.Domains["users"], real.Domains["users"])
	}
	if got := countRows(t, &model.User{}); got != usersBefore+1 {
		t.Errorf("real run did not create the user: users=%d", got)
	}
}

func TestSchemaVersionMismatchRejected(t *testing.T) {
	boot(t)
	// A well-formed archive but with a bogus schema fingerprint.
	blob := craftArchive(t, "9999_not_a_real_migration")
	if _, err := backup.Read(bytes.NewReader(blob), ""); err == nil {
		t.Fatal("expected schema_version mismatch to be rejected")
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	boot(t)
	arc, _ := backup.Collect(db.GetDb())
	var buf bytes.Buffer
	if _, err := backup.Write(&buf, arc, backup.Options{Password: "correct-horse"}); err != nil {
		t.Fatalf("write encrypted: %v", err)
	}
	blob := buf.Bytes()

	if _, err := backup.Read(bytes.NewReader(blob), "wrong"); err == nil {
		t.Error("wrong password accepted")
	}
	if _, err := backup.Read(bytes.NewReader(blob), ""); err == nil {
		t.Error("empty password accepted for an encrypted archive")
	}
	if _, err := backup.Read(bytes.NewReader(blob), "correct-horse"); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
}

func TestSanitizeStripsSecrets(t *testing.T) {
	boot(t)
	// sanitize + password are mutually exclusive.
	if _, err := backup.Write(&bytes.Buffer{}, &backup.Archive{},
		backup.Options{Sanitize: true, Password: "x"}); err == nil {
		t.Error("sanitize+password should be rejected")
	}

	out := roundTrip(t, backup.Options{Sanitize: true})
	for _, s := range out.Storages {
		if s.MountPath == "/bk" && s.Addition != "{}" {
			t.Errorf("sanitize left storage addition: %q", s.Addition)
		}
	}
	found := false
	for _, u := range out.Users {
		if u.Username == "bk_user1" {
			found = true
			if u.PwdHash != "" || u.Password != "" || u.Salt != "" {
				t.Errorf("sanitize left user credentials: hash=%q pwd=%q salt=%q",
					u.PwdHash, u.Password, u.Salt)
			}
		}
	}
	if !found {
		t.Fatal("editor1 missing from sanitized export")
	}
	if !out.Manifest.Sanitized {
		t.Error("manifest.sanitized not set")
	}
}
