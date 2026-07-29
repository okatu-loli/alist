package op

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestStorageRenameResyncsScopePermissions is a regression test for the
// drift gap in the storage mount-path rename path: db.UpdateRolePermissionsPathPrefix
// rewrites the role row's PermissionScopes JSON but, before this fix, never
// touched the normalized scope-permission rows the policy engine actually
// reads (same drift class as the enforceAdminRoleDefaults bug, see
// policy_sync_admin_test.go). Without the fix the role.PermissionScopes path
// is correct but db.GetPermissionsByRoleIDs keeps serving the stale path
// forever, so op.ResolveEffectivePolicy silently authorizes against a path
// that no longer exists.
func TestStorageRenameResyncsScopePermissions(t *testing.T) {
	dir := t.TempDir()
	sid := createTestStorage(t, "/s5t8-old", dir)

	r := &model.Role{Name: "s5t8-rename-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/s5t8-old/data", Permission: 1 << 3}, // write
	}}
	if err := CreateRole(r); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _ = DeleteRole(r.ID) })

	storage, err := db.GetStorageById(sid)
	if err != nil {
		t.Fatalf("get storage: %v", err)
	}
	storage.MountPath = "/s5t8-new"
	if err := UpdateStorage(context.Background(), *storage); err != nil {
		t.Fatalf("update storage (rename): %v", err)
	}

	role, err := db.GetRole(r.ID)
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if len(role.PermissionScopes) != 1 || role.PermissionScopes[0].Path != "/s5t8-new/data" {
		t.Fatalf("role.PermissionScopes not rewritten: %+v", role.PermissionScopes)
	}

	perms, err := db.GetPermissionsByRoleIDs([]uint{r.ID})
	if err != nil {
		t.Fatalf("GetPermissionsByRoleIDs: %v", err)
	}
	if len(perms) != 1 || perms[0].Path != "/s5t8-new/data" {
		t.Fatalf("stale scope permission row after mount rename: %+v", perms)
	}
	// Scope rows are never node-anchored: their Path (rebuilt from the role's
	// declaration by the resync) is their sole truth, so the assertion above is
	// the whole contract — there is no anchor to re-point.

	// The policy engine must see the new path immediately (cache invalidated).
	u := &model.User{ID: 79001, Username: "s5t8-rename-user", Role: model.Roles{int(r.ID)}}
	d, err := ResolveEffectivePolicy(u, "/s5t8-new/data")
	if err != nil {
		t.Fatalf("ResolveEffectivePolicy: %v", err)
	}
	if !d.Can(model.ActWrite) {
		t.Errorf("expected write on renamed path, policy=%+v", d)
	}
}
