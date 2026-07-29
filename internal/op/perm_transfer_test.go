package op

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestDetachDeletesFollowedManualRows is the detach acceptance case: after a
// move with perm_policy=detach, the manual rows that would have followed the
// object (including a deny row) are gone, and the moved path falls back to the
// destination's effective policy. Capture happens before the move (read-only),
// deletion after it succeeds.
func TestDetachDeletesFollowedManualRows(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src/child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dest"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/det", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	allow := &model.Permission{Name: "det-allow", Path: "/det/src", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	deny := &model.Permission{Name: "det-deny", Path: "/det/src/child", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	u := makeRelocPermUser(t, "detU", []*model.Permission{allow, deny})

	storage, err := GetStorageByMountPath("/det")
	if err != nil {
		t.Fatal(err)
	}

	// Precondition: readable at src before the move (rules out a false green).
	if d, err := ResolveEffectivePolicy(u, "/det/src"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("precondition: /det/src must be readable before move: %v %v", d, err)
	}

	// Capture BEFORE the move (rows still anchored at the source).
	ids, err := CaptureManualPermissionIDs(context.Background(), storage, "/src")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 manual rows captured (allow+deny), got %d", len(ids))
	}

	if err := Move(context.Background(), storage, "/src", "/dest"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if err := DeleteDetachedPermissions(ids); err != nil {
		t.Fatalf("detach delete: %v", err)
	}
	InvalidateRolePolicyForTest(uint(u.Role[0]))

	// Both rows are gone.
	if _, err := db.GetPermissionById(allow.ID); err == nil {
		t.Fatal("allow row must be deleted by detach")
	}
	if _, err := db.GetPermissionById(deny.ID); err == nil {
		t.Fatal("deny row must be deleted by detach")
	}
	// role bindings dropped too.
	rids, err := db.GetRoleIDsByPermission(allow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rids) != 0 {
		t.Fatalf("allow row bindings must be deleted, got %v", rids)
	}
	// The moved subtree no longer carries the detached policy: neither the old
	// source path nor the new destination path is covered by these rows.
	if d, err := ResolveEffectivePolicy(u, "/det/dest/src"); err != nil || d.Can(model.ActRead) || d.Denied(model.ActRead) {
		t.Fatalf("moved path must fall back (no manual allow/deny) after detach: %+v %v", d, err)
	}
}

// TestDetachFailedMoveKeepsPermissions guards the "failed move must delete
// nothing" invariant: capture is read-only and deletion only runs on success,
// so a move that errors leaves every manual row intact.
func TestDetachFailedMoveKeepsPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/detf", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	p := &model.Permission{Name: "detf-allow", Path: "/detf/src", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	if err := CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })

	storage, err := GetStorageByMountPath("/detf")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := CaptureManualPermissionIDs(context.Background(), storage, "/src")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected the manual row captured before the move, got %d", len(ids))
	}
	// Move into a nonexistent destination directory: op.Move fails.
	if err := Move(context.Background(), storage, "/src", "/nope"); err == nil {
		t.Fatal("expected move into missing dst dir to fail")
	}
	// Deletion is not called on failure; the row survives.
	if _, err := db.GetPermissionById(p.ID); err != nil {
		t.Fatalf("row must survive a failed move (detach not applied): %v", err)
	}
}

// TestClonePermissionsForCopy is the copy_perms acceptance case: cloning copies
// the source subtree's manual rows to the destination, re-anchored, and — the
// load-bearing part — reproduces the role bindings so a member of that role
// gains the same effective access at the destination path.
func TestClonePermissionsForCopy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src/child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dest"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := createTestStorage(t, "/cln", dir)
	t.Cleanup(resetObjRelocateHooks)

	allow := &model.Permission{Name: "cln-allow", Path: "/cln/src", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	deny := &model.Permission{Name: "cln-deny", Path: "/cln/src/child", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	// A member of this role is the "another role member" the spec's clone test
	// requires: their access is entirely role-derived, proving bindings cloned.
	u := makeRelocPermUser(t, "clnU", []*model.Permission{allow, deny})

	storage, err := GetStorageByMountPath("/cln")
	if err != nil {
		t.Fatal(err)
	}
	// Perform a same-storage copy of /src into /dest, then clone perms.
	if err := Copy(context.Background(), storage, "/src", "/dest"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := ClonePermissionsForCopy(context.Background(), storage, "/src", storage, "/dest/src"); err != nil {
		t.Fatalf("clone: %v", err)
	}
	InvalidateRolePolicyForTest(uint(u.Role[0]))

	// Source rows untouched.
	if d, err := ResolveEffectivePolicy(u, "/cln/src"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("source access must remain after copy: %+v %v", d, err)
	}
	// Destination gained an equivalent allow at the top and deny in the child
	// subtree — the role member sees the same shape at the new location.
	if d, err := ResolveEffectivePolicy(u, "/cln/dest/src"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("cloned allow must grant read at destination: %+v %v", d, err)
	}
	if d, err := ResolveEffectivePolicy(u, "/cln/dest/src/child"); err != nil || !d.Denied(model.ActRead) {
		t.Fatalf("cloned deny must apply at destination child: %+v %v", d, err)
	}

	// Exactly two clone rows exist at the destination anchors, each bound.
	assertClonedRowBound(t, sid, "/dest/src", uint(u.Role[0]))
	assertClonedRowBound(t, sid, "/dest/src/child", uint(u.Role[0]))
}

func assertClonedRowBound(t *testing.T, storageID uint, anchor string, roleID uint) {
	t.Helper()
	nodeID, found, err := db.GetNodeByChain(storageID, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("no node at cloned anchor %s", anchor)
	}
	rows, err := db.GetManualPermissionsByNodeIDs([]uint{nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one cloned row at %s, got %d", anchor, len(rows))
	}
	if rows[0].Origin != model.OriginManual {
		t.Fatalf("cloned row at %s must be Origin=manual, got %q", anchor, rows[0].Origin)
	}
	rids, err := db.GetRoleIDsByPermission(rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	bound := false
	for _, r := range rids {
		if r == roleID {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("cloned row at %s must be bound to role %d, got bindings %v", anchor, roleID, rids)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(rows[0].ID) })
}
