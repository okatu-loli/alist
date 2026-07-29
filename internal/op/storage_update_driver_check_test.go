package op

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestUpdateStorageEnableRenameFailsBeforeRoleJSONRewrite is a regression
// test for a permanent-drift window in UpdateStorage: on a combined
// "re-enable a disabled storage while also renaming its mount path"
// request, the old code checked the GetStorageByMountPath(oldMountPath)
// error (the disabled storage's driver isn't in storagesMap, since
// DisableStorage removes it) only AFTER db.UpdateRolePermissionsPathPrefix
// had already rewritten every affected role's PermissionScopes JSON to the
// new path. The overall call still returned an error, but the JSON rewrite
// stuck. Worse, a caller retrying the identical request would find
// oldStorage.MountPath already equal to the new path (it was persisted by
// the failed attempt), so the rename branch would no longer even trigger —
// the drift became permanent.
func TestUpdateStorageEnableRenameFailsBeforeRoleJSONRewrite(t *testing.T) {
	dir := t.TempDir()
	sid := createTestStorage(t, "/s5r2-old", dir)

	r := &model.Role{Name: "s5r2-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/s5r2-old/data", Permission: 1 << 3}, // write
	}}
	if err := CreateRole(r); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _ = DeleteRole(r.ID) })

	if err := DisableStorage(context.Background(), sid); err != nil {
		t.Fatalf("disable storage: %v", err)
	}

	storage, err := db.GetStorageById(sid)
	if err != nil {
		t.Fatalf("get storage: %v", err)
	}
	// Combined enable + rename: storage.Disabled starts true (from
	// DisableStorage above), so it's absent from storagesMap; requesting
	// Disabled=false at the same time as a MountPath change is exactly the
	// combination that used to slip the JSON rewrite past the driver error.
	storage.Disabled = false
	storage.MountPath = "/s5r2-new"
	if err := UpdateStorage(context.Background(), *storage); err == nil {
		t.Fatalf("expected UpdateStorage to fail (no live driver for the disabled storage's old mount), got nil error")
	}

	role, err := db.GetRole(r.ID)
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if len(role.PermissionScopes) != 1 || role.PermissionScopes[0].Path != "/s5r2-old/data" {
		t.Fatalf("role.PermissionScopes must stay untouched when UpdateStorage fails before completing the rename: %+v", role.PermissionScopes)
	}
}
