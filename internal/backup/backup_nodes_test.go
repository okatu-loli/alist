package backup_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers"
	"github.com/alist-org/alist/v3/internal/backup"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestBackupNodeAnchorRoundTrip is the hole-1 regression: permission/meta rows
// anchored on the nodes table must survive export→clear→restore with their
// anchor ALIVE — not merely present as rows. After a restore into a fresh
// database and a storage load, the restored permission/meta must be re-anchored
// (node_id backfilled), enforce decisions at the live path, AND follow the object
// when it is renamed. The nodes table is not exported; the exporter records each
// anchored row's live virtual path, and normal storage-load adoption rebuilds the
// node chain — the same intake path a v3→v4 migration uses.
func TestBackupNodeAnchorRoundTrip(t *testing.T) {
	boot(t)
	ctx := context.Background()
	// bootstrap.LoadStorages installs this in production; boot() doesn't run it,
	// so register the node relocate hook here to exercise move-follow.
	op.RegisterNodeRelocation()

	// A real on-disk storage root with team/sub, so the Local driver loads and
	// adoption can resolve the virtual path back to a relpath.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "team", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := op.CreateStorage(ctx, model.Storage{
		MountPath: "/nb", Driver: "Local",
		Addition: `{"root_folder_path":"` + root + `"}`,
	}); err != nil {
		t.Fatalf("create storage: %v", err)
	}

	// Node-anchored meta + permission, created through op so they anchor onto the
	// nodes table (not raw rows).
	meta := &model.Meta{Path: "/nb/team", Password: "pw", PSub: true}
	if err := op.CreateMeta(meta); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	if meta.NodeID == 0 {
		t.Fatalf("meta must be node-anchored before export, got NodeID 0")
	}

	g := db.GetDb()
	role := model.Role{Name: "nbrole"}
	if err := g.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	perm := &model.Permission{Name: "nb-read", ResourceType: model.ResourceFS,
		Path: "/nb/team", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow,
		Recursive: true, Origin: model.OriginManual}
	if err := op.CreatePermission(perm); err != nil {
		t.Fatalf("create permission: %v", err)
	}
	if perm.NodeID == 0 {
		t.Fatalf("permission must be node-anchored before export, got NodeID 0")
	}
	if err := g.Create(&model.RolePermission{RoleID: role.ID, PermissionID: perm.ID}).Error; err != nil {
		t.Fatalf("bind role_permission: %v", err)
	}
	u := (&model.User{Username: "nbuser", Role: model.Roles{int(role.ID)}}).SetPassword("x")
	if err := g.Create(u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.SetUserRoles(u.ID, []uint{role.ID}); err != nil {
		t.Fatalf("set user roles: %v", err)
	}

	// Sanity: the anchor works in the source database.
	if src, err := op.GetUserByName("nbuser"); err != nil {
		t.Fatalf("get source user: %v", err)
	} else if d, err := op.ResolveEffectivePolicy(src, "/nb/team"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("source: user must read /nb/team: %+v %v", d, err)
	}

	// Export.
	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var buf bytes.Buffer
	if _, err := backup.Write(&buf, arc, backup.Options{}); err != nil {
		t.Fatalf("write: %v", err)
	}
	restored, err := backup.Read(&buf, "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Clear + restore into a brand-new database.
	boot(t)
	if _, err := backup.Apply(restored, backup.StrategyOverwrite, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Load the restored storage: this fires adoption, which must re-anchor the
	// restored permission/meta onto freshly-built nodes.
	st := storageByMount(t, "/nb")
	if err := op.LoadStorage(ctx, *st); err != nil {
		t.Fatalf("load restored storage: %v", err)
	}

	// The restored permission is node-anchored again (anchor alive, not dangling).
	var rp model.Permission
	if err := db.GetDb().Where("name = ?", "nb-read").First(&rp).Error; err != nil {
		t.Fatalf("reload permission: %v", err)
	}
	if rp.NodeID == 0 {
		t.Fatalf("restored permission must be re-anchored (node_id != 0), got 0")
	}

	ru, err := op.GetUserByName("nbuser")
	if err != nil {
		t.Fatalf("get restored user: %v", err)
	}
	if d, err := op.ResolveEffectivePolicy(ru, "/nb/team"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("restored: user must read /nb/team: %+v %v", d, err)
	}
	if em, err := op.ResolveEffectiveMeta("/nb/team"); err != nil || !em.PasswordApplies || em.Password != "pw" {
		t.Fatalf("restored: meta password must apply at /nb/team: %+v %v", em, err)
	}

	// The anchor is LIVE, not just present: rename the object and the decision +
	// meta follow to the new path, and abandon the old one.
	storage, _, err := op.GetStorageAndActualPath("/nb")
	if err != nil {
		t.Fatalf("get storage: %v", err)
	}
	if err := op.Rename(ctx, storage, "/team", "team2"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if d, err := op.ResolveEffectivePolicy(ru, "/nb/team2"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("after rename: user must read /nb/team2 (anchor followed): %+v %v", d, err)
	}
	if d, err := op.ResolveEffectivePolicy(ru, "/nb/team"); err != nil || d.Can(model.ActRead) {
		t.Fatalf("after rename: old path /nb/team must no longer be granted: %+v %v", d, err)
	}
	if em, err := op.ResolveEffectiveMeta("/nb/team2"); err != nil || !em.PasswordApplies || em.Password != "pw" {
		t.Fatalf("after rename: meta must follow to /nb/team2: %+v %v", em, err)
	}
}
