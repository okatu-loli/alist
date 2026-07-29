package op

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

func TestPermissionFollowsRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "team"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := createTestStorage(t, "/pr", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	p := &model.Permission{Name: "team-read", Path: "/pr/team",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	if err := CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })
	if p.NodeID == 0 {
		t.Fatalf("permission must be node-anchored on create: %+v", p)
	}
	nid := p.NodeID

	storage, err := GetStorageByMountPath("/pr")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storage, "/team", "crew"); err != nil {
		t.Fatal(err)
	}
	// The row's node is repointed (one edit); its derived path is now /pr/crew.
	// The stale Path column is intentionally left untouched (dropped in W3).
	if node, found, err := db.GetNodeByChain(sid, "/crew"); err != nil || !found || node != nid {
		t.Fatalf("node not at new relpath after rename: node=%d found=%v err=%v", node, found, err)
	}
	if _, found, err := db.GetNodeByChain(sid, "/team"); err != nil || found {
		t.Fatalf("node still resolvable at old relpath after rename: found=%v err=%v", found, err)
	}
	if vp, err := NodeVirtualPath(nid); err != nil || vp != "/pr/crew" {
		t.Fatalf("permission did not follow rename: vp=%q err=%v", vp, err)
	}
}

// makeRelocPermUser mirrors policy_test.go's makePolicyUser but lives in
// package op (not op_test) so relocation tests here — which need the
// package-private test helpers createTestStorage/resetObjRelocateHooks — can
// also drive a decision through Authorize/ResolveEffectivePolicy.
func makeRelocPermUser(t *testing.T, name string, perms []*model.Permission) *model.User {
	t.Helper()
	r := &model.Role{Name: name + "-role"}
	if err := CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteRole(r.ID) })
	for _, p := range perms {
		// op.CreatePermission anchors the row to a node (when its path resolves
		// to a loaded storage), the state real admin creates produce and the
		// prerequisite for node-based relocation to carry the row.
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
	return &model.User{ID: 99990 + uint(len(name)), Username: name, Role: model.Roles{int(r.ID)}}
}

// TestPermissionRelocationDecisionFollowsRename is the decision-level S1
// counterpart to TestPermissionFollowsRename: it is not enough for the DB row
// to carry a new Path/Anchor, the policy engine must actually flip which
// virtual path it covers. Reproduces the task's acceptance criteria:
// old path denies post-move, new path allows post-move, and the effect was
// really live (read at the old path before the rename to rule out a false
// green from a permission that never worked).
func TestPermissionRelocationDecisionFollowsRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs/inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/prd", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	p := &model.Permission{Name: "docs-read", Path: "/prd/docs",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	u := makeRelocPermUser(t, "prdU", []*model.Permission{p})

	// Precondition: readable at the old path (and its subtree) before the move.
	if d, err := ResolveEffectivePolicy(u, "/prd/docs"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("precondition: /prd/docs must be readable before rename: %v %v", d, err)
	}
	if d, err := ResolveEffectivePolicy(u, "/prd/docs/inner"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("precondition: /prd/docs/inner must be readable before rename: %v %v", d, err)
	}

	storage, err := GetStorageByMountPath("/prd")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storage, "/docs", "manuals"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// invalidateAllPolicy() runs inside the hook, but exercise the same
	// invalidation path a real caller would rely on, matching policy_test.go's
	// pattern for the cache round-trip.
	InvalidateRolePolicyForTest(uint(u.Role[0]))

	if d, err := ResolveEffectivePolicy(u, "/prd/manuals"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("new path must be readable after rename: %v %v", d, err)
	}
	if d, err := ResolveEffectivePolicy(u, "/prd/manuals/inner"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("new path subtree must be readable after rename: %v %v", d, err)
	}
	if d, err := ResolveEffectivePolicy(u, "/prd/docs"); err != nil || d.Can(model.ActRead) {
		t.Fatalf("old path must no longer be covered after rename: %v %v", d, err)
	}
	if err := Authorize(context.Background(), u, model.ActRead, AuthzRequest{Path: "/prd/docs"}); err == nil {
		t.Fatal("Authorize must deny the stale old path after rename")
	}
	if err := Authorize(context.Background(), u, model.ActRead, AuthzRequest{Path: "/prd/manuals"}); err != nil {
		t.Fatalf("Authorize must allow the new path after rename: %v", err)
	}
}

// TestPermissionFollowsMove is TestPermissionFollowsRename's Move-kind
// sibling: Move and Rename fire distinct RelocateKind values (hook.go), and
// both must drive relocatePermissions — the hook only special-cases Remove.
func TestPermissionFollowsMove(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src/child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dest"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/prm", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	p := &model.Permission{Name: "src-read", Path: "/prm/src",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	child := &model.Permission{Name: "src-child-deny", Path: "/prm/src/child",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	if err := CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })
	if err := CreatePermission(child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(child.ID) })

	storage, err := GetStorageByMountPath("/prm")
	if err != nil {
		t.Fatal(err)
	}
	if err := Move(context.Background(), storage, "/src", "/dest"); err != nil {
		t.Fatalf("move: %v", err)
	}

	// Moving /src repoints one node; the child node follows via parent_id, so
	// both derived paths land under /dest without per-row rewrites.
	if vp, err := NodeVirtualPath(p.NodeID); err != nil || vp != "/prm/dest/src" {
		t.Fatalf("top permission did not follow move: vp=%q err=%v", vp, err)
	}
	if vp, err := NodeVirtualPath(child.NodeID); err != nil || vp != "/prm/dest/src/child" {
		t.Fatalf("recursive subtree permission did not follow move: vp=%q err=%v", vp, err)
	}
}

// TestPermissionRelocationCrossStorageIsolated guards storage isolation in node
// anchoring: node identity includes storage_id, so a permission anchored at the
// SAME relative path but on a DIFFERENT storage is a distinct node and must be
// untouched by a relocation on the first storage, even though the relpaths
// collide.
func TestPermissionRelocationCrossStorageIsolated(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirA, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dirB, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/prx-a", dirA)
	createTestStorage(t, "/prx-b", dirB)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	pA := &model.Permission{Name: "a-shared", Path: "/prx-a/shared",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	pB := &model.Permission{Name: "b-shared", Path: "/prx-b/shared",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	for _, p := range []*model.Permission{pA, pB} {
		if err := CreatePermission(p); err != nil {
			t.Fatal(err)
		}
		pid := p.ID
		t.Cleanup(func() { _ = db.DeletePermissionById(pid) })
	}

	storageA, err := GetStorageByMountPath("/prx-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storageA, "/shared", "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Node identity includes storage_id, so A's rename touches only A's node.
	if vp, err := NodeVirtualPath(pA.NodeID); err != nil || vp != "/prx-a/renamed" {
		t.Fatalf("storage A permission did not follow rename: vp=%q err=%v", vp, err)
	}
	if vp, err := NodeVirtualPath(pB.NodeID); err != nil || vp != "/prx-b/shared" {
		t.Fatalf("storage B permission must be untouched by storage A's rename: vp=%q err=%v", vp, err)
	}
}

// TestPermissionRelocationFileExactOnly: renaming a FILE moves only its own
// node. A permission on a prefix-sharing sibling ("/file.txt-extra") is a
// distinct node and must be untouched — structurally guaranteed under node
// anchoring, replacing the old isDir-threading / LIKE-prefix guard.
func TestPermissionRelocationFileExactOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/prf", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	// Anchored exactly at the file: must follow.
	exact := &model.Permission{Name: "file-read", Path: "/prf/file.txt",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: false}
	// A sibling sharing "/file.txt" as a string prefix (not a path boundary):
	// a distinct node that must never be treated as "under" the file.
	stray := &model.Permission{Name: "stray", Path: "/prf/file.txt-extra",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: false}
	for _, p := range []*model.Permission{exact, stray} {
		if err := CreatePermission(p); err != nil {
			t.Fatal(err)
		}
		pid := p.ID
		t.Cleanup(func() { _ = db.DeletePermissionById(pid) })
	}

	storage, err := GetStorageByMountPath("/prf")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storage, "/file.txt", "renamed.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if vp, err := NodeVirtualPath(exact.NodeID); err != nil || vp != "/prf/renamed.txt" {
		t.Fatalf("exact-anchored file permission did not follow rename: vp=%q err=%v", vp, err)
	}
	if vp, err := NodeVirtualPath(stray.NodeID); err != nil || vp != "/prf/file.txt-extra" {
		t.Fatalf("prefix-sharing sibling permission must be untouched by the file rename: vp=%q err=%v", vp, err)
	}
}

// TestPermissionRelocationSkipsScopeRows guards the 0-B fix: a move/rename
// relocates only manual permission rows. Scope-origin rows have the role's
// PermissionScopes declaration as their truth (SyncRoleScopePermissions
// regenerates them wholesale), so moving them here would only produce a
// temporary follow that the next scope resync bounces back to the old path.
func TestPermissionRelocationSkipsScopeRows(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dest"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/rss", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	// Scope rows carry no node (their truth is the role's PermissionScopes), so
	// they are written straight through db.CreatePermission as production does.
	scope := &model.Permission{Name: "role:x:scope:0", Path: "/rss/a", Origin: model.OriginScope,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	manual := &model.Permission{Name: "manual-a", Path: "/rss/a", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	if err := db.CreatePermission(scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(scope.ID) })
	if err := CreatePermission(manual); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(manual.ID) })

	storage, err := GetStorageByMountPath("/rss")
	if err != nil {
		t.Fatal(err)
	}
	if err := Move(context.Background(), storage, "/a", "/dest"); err != nil {
		t.Fatalf("move: %v", err)
	}

	// Scope row is unanchored (node_id 0): the node move does not touch it, and
	// its Path (its truth) stays put.
	freshScope, err := db.GetPermissionById(scope.ID)
	if err != nil {
		t.Fatal(err)
	}
	if freshScope.NodeID != 0 || freshScope.Path != "/rss/a" {
		t.Fatalf("scope row must not follow move (role declaration is its truth): %+v", freshScope)
	}
	// Manual row is node-anchored: its node followed the move, so its derived
	// path is now under /dest.
	if vp, err := NodeVirtualPath(manual.NodeID); err != nil || vp != "/rss/dest/a" {
		t.Fatalf("manual row must follow move: vp=%q err=%v", vp, err)
	}
}

func TestPermissionSurvivesRemove(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/pr2", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	p := &model.Permission{Name: "keep-deny", Path: "/pr2/keep",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	if err := CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })

	storage, err := GetStorageByMountPath("/pr2")
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(context.Background(), storage, "/keep"); err != nil {
		t.Fatal(err)
	}
	// A removal never touches nodes (spec §1): the deny row and its node both
	// survive, so the rule reapplies if /keep is recreated.
	if _, err := db.GetPermissionById(p.ID); err != nil {
		t.Fatalf("deny permission must survive object removal: %v", err)
	}
	if vp, err := NodeVirtualPath(p.NodeID); err != nil || vp != "/pr2/keep" {
		t.Fatalf("deny permission node must survive removal: vp=%q err=%v", vp, err)
	}
}
