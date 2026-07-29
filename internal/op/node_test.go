package op_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// makeStorageRow inserts a bare storage row (no live driver) so NodeVirtualPath
// can resolve a mount point; the node path derivation never touches the driver.
func makeStorageRow(t *testing.T, mountPath string) uint {
	t.Helper()
	s := model.Storage{MountPath: mountPath, Driver: "Local", Addition: "{}"}
	if err := db.GetDb().Create(&s).Error; err != nil {
		t.Fatalf("create storage row: %v", err)
	}
	t.Cleanup(func() { db.GetDb().Delete(&model.Storage{}, s.ID) })
	return s.ID
}

func TestNodeRelPathMultiLevel(t *testing.T) {
	op.InvalidateAllNodePathCache()
	sid := makeStorageRow(t, "/nrp")
	leaf, err := db.EnsureNodeChain(sid, "/a/b/c")
	if err != nil {
		t.Fatal(err)
	}
	gotSID, rel, err := op.NodeRelPath(leaf)
	if err != nil {
		t.Fatalf("relpath: %v", err)
	}
	if gotSID != sid || rel != "/a/b/c" {
		t.Fatalf("bad relpath: sid=%d rel=%q", gotSID, rel)
	}

	// Root node derives to "/".
	root, err := db.EnsureNodeChain(sid, "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, rel, err := op.NodeRelPath(root); err != nil || rel != "/" {
		t.Fatalf("root relpath: rel=%q err=%v", rel, err)
	}
}

func TestNodeRelPathCacheAndInvalidation(t *testing.T) {
	op.InvalidateAllNodePathCache()
	sid := makeStorageRow(t, "/nci")
	leaf, err := db.EnsureNodeChain(sid, "/dir/leaf")
	if err != nil {
		t.Fatal(err)
	}
	if _, rel, _ := op.NodeRelPath(leaf); rel != "/dir/leaf" {
		t.Fatalf("initial rel %q", rel)
	}

	// Rename the leaf directly in the DB WITHOUT invalidating: the cache must
	// still serve the old path, proving the value is memoized.
	if err := db.UpdateNodeParentName(leaf, parentOf(t, sid, "/dir"), "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, rel, _ := op.NodeRelPath(leaf); rel != "/dir/leaf" {
		t.Fatalf("expected stale cached path, got %q", rel)
	}

	// After invalidation the path is recomputed from the DB.
	op.InvalidateNodePathCache(leaf)
	if _, rel, _ := op.NodeRelPath(leaf); rel != "/dir/renamed" {
		t.Fatalf("expected recomputed path, got %q", rel)
	}
}

func TestNodeRelPathCycleErrors(t *testing.T) {
	op.InvalidateAllNodePathCache()
	sid := makeStorageRow(t, "/cyc")
	// Build A -> B (B child of A), then corrupt A to point at B: A<->B loop.
	a, err := db.EnsureNodeChain(sid, "/A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.EnsureNodeChain(sid, "/A/B")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeParentName(a, b, "A"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := op.NodeRelPath(b); err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

func TestNodeVirtualPathStripsBalanceSuffix(t *testing.T) {
	op.InvalidateAllNodePathCache()
	// A balance-group member mount ("/grp.balance2") must derive to the group's
	// virtual mount ("/grp"), never the raw member path.
	sid := makeStorageRow(t, "/grp.balance2")
	leaf, err := db.EnsureNodeChain(sid, "/photos/2026")
	if err != nil {
		t.Fatal(err)
	}
	vp, err := op.NodeVirtualPath(leaf)
	if err != nil {
		t.Fatalf("virtual path: %v", err)
	}
	if vp != "/grp/photos/2026" {
		t.Fatalf("balance suffix not stripped: %q", vp)
	}
}

func TestNodeVirtualPathJoinsMount(t *testing.T) {
	op.InvalidateAllNodePathCache()
	sid := makeStorageRow(t, "/mnt")
	leaf, err := db.EnsureNodeChain(sid, "/a/b")
	if err != nil {
		t.Fatal(err)
	}
	vp, err := op.NodeVirtualPath(leaf)
	if err != nil {
		t.Fatalf("virtual path: %v", err)
	}
	if vp != "/mnt/a/b" {
		t.Fatalf("bad virtual path: %q", vp)
	}
	// Root node maps to the bare mount point.
	root, _, err := db.GetNodeByChain(sid, "/")
	if err != nil {
		t.Fatal(err)
	}
	if vp, err := op.NodeVirtualPath(root); err != nil || vp != "/mnt" {
		t.Fatalf("root virtual path: %q err=%v", vp, err)
	}
}

func parentOf(t *testing.T, sid uint, rel string) uint {
	t.Helper()
	id, found, err := db.GetNodeByChain(sid, rel)
	if err != nil || !found {
		t.Fatalf("parent %q not found: found=%v err=%v", rel, found, err)
	}
	return id
}
