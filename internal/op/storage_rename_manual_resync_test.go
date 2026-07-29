package op

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestStorageRenameFollowsManualPermissionsAndMeta: renaming a storage's mount
// point must keep every node-anchored manual permission and meta matching at
// the NEW mount. Under node anchoring this is automatic — a row's virtual path
// is derived by joining the storage's live mount point (NodeVirtualPath looks it
// up fresh), so no per-row Path rewrite (the former ResyncManual/MetaPaths) is
// needed; the rename only drops the path-derived caches. A manual deny row
// silently failing to match at the new path would be a security regression.
func TestStorageRenameFollowsManualPermissionsAndMeta(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data/secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := createTestStorage(t, "/mr-old", dir)

	// manual allow on the whole subtree, manual deny carved out of it
	allow := &model.Permission{Name: "mr-allow", Path: "/mr-old/data", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	deny := &model.Permission{Name: "mr-deny", Path: "/mr-old/data/secret", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	r := &model.Role{Name: "mr-resync-role"}
	if err := CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteRole(r.ID) })
	for _, p := range []*model.Permission{allow, deny} {
		if err := CreatePermission(p); err != nil {
			t.Fatal(err)
		}
		pid := p.ID
		t.Cleanup(func() { _ = db.DeletePermissionById(pid) })
		if err := db.AddRolePermission(r.ID, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	InvalidateRolePolicyForTest(r.ID)
	u := &model.User{ID: 78010, Username: "mr-resync-user", Role: model.Roles{int(r.ID)}}

	// meta with a password guarding the same subtree
	m := &model.Meta{Path: "/mr-old/data", Password: "s3cr3t", PSub: true}
	if err := CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteMetaById(m.ID) })

	// preconditions at the old path — rule out a false green from rows that
	// never worked.
	if d, err := ResolveEffectivePolicy(u, "/mr-old/data"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("precondition: /mr-old/data must be readable: %v %v", d, err)
	}
	if d, err := ResolveEffectivePolicy(u, "/mr-old/data/secret"); err != nil || d.Can(model.ActRead) {
		t.Fatalf("precondition: /mr-old/data/secret must be denied: %v %v", d, err)
	}
	if em, err := ResolveEffectiveMeta("/mr-old/data"); err != nil || em.Password != "s3cr3t" {
		t.Fatalf("precondition: meta password must apply at /mr-old/data: %+v %v", em, err)
	}

	// rename the mount point
	storage, err := db.GetStorageById(sid)
	if err != nil {
		t.Fatal(err)
	}
	storage.MountPath = "/mr-new"
	if err := UpdateStorage(context.Background(), *storage); err != nil {
		t.Fatalf("update storage (rename): %v", err)
	}

	// new path: allow still allows, deny still denies (both must have followed)
	if d, err := ResolveEffectivePolicy(u, "/mr-new/data"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("new path must be readable after rename: %v %v", d, err)
	}
	if d, err := ResolveEffectivePolicy(u, "/mr-new/data/secret"); err != nil || d.Can(model.ActRead) {
		t.Fatalf("new path deny must still deny after rename: %v %v", d, err)
	}
	if err := Authorize(context.Background(), u, model.ActRead, AuthzRequest{Path: "/mr-new/data/secret"}); err == nil {
		t.Fatal("Authorize must deny at the new secret path")
	}
	if em, err := ResolveEffectiveMeta("/mr-new/data"); err != nil || em.Password != "s3cr3t" {
		t.Fatalf("meta password must apply at the new path: %+v %v", em, err)
	}

	// old path must no longer be covered by anything
	if d, err := ResolveEffectivePolicy(u, "/mr-old/data"); err != nil || d.Can(model.ActRead) {
		t.Fatalf("old path must no longer be covered after rename: %v %v", d, err)
	}
	if em, err := ResolveEffectiveMeta("/mr-old/data"); err != nil || em.Password != "" {
		t.Fatalf("meta must not apply at the old path after rename: %+v %v", em, err)
	}

	// The rows themselves were never rewritten by the rename — their nodes'
	// derived paths simply join the new mount. Confirm each still resolves to
	// the new virtual path via its node.
	freshAllow, err := db.GetPermissionById(allow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vp, err := NodeVirtualPath(freshAllow.NodeID); err != nil || vp != "/mr-new/data" {
		t.Fatalf("manual allow node did not follow mount rename: vp=%q err=%v", vp, err)
	}
	freshMeta, err := db.GetMetaById(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vp, err := NodeVirtualPath(freshMeta.NodeID); err != nil || vp != "/mr-new/data" {
		t.Fatalf("meta node did not follow mount rename: vp=%q err=%v", vp, err)
	}
}
