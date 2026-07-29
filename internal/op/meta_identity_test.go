package op

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

// relocateMetaNode moves the node currently at oldRel to newRel within storageID
// and drops the derived-path caches, mirroring what the node relocate hook does
// on a real move/rename — without needing a driver call. It lets these tests
// reproduce "the object moved, its meta followed via the node, but the meta row's
// stored Path stayed behind".
func relocateMetaNode(t *testing.T, storageID uint, oldRel, newRel string) {
	t.Helper()
	if err := relocateNode(storageID, oldRel, newRel); err != nil {
		t.Fatalf("relocate node %s -> %s: %v", oldRel, newRel, err)
	}
	InvalidateAllNodePathCache()
	invalidateMetaIndex()
}

// TestCreateMetaAtReclaimedPathAfterMove is the hole-2 regression: a meta anchored
// at /a follows its node when the object moves to /b, but the meta row's stored
// Path stays "/a". Recreating a directory at /a and setting a meta on it must
// succeed — the stale stored Path must not collide on a unique key. Logical
// uniqueness is on the LIVE derived path, and the two metas now live at different
// live paths (/a and /b).
func TestCreateMetaAtReclaimedPathAfterMove(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := createTestStorage(t, "/mid", dir)

	m1 := &model.Meta{Path: "/mid/a", Password: "p1"}
	if err := CreateMeta(m1); err != nil {
		t.Fatalf("create first meta: %v", err)
	}
	if m1.NodeID == 0 {
		t.Fatalf("meta must be node-anchored, got NodeID 0: %+v", m1)
	}

	// The object at /mid/a moves to /mid/b: the node follows, so the meta's live
	// path is now /mid/b, but its stored Path column is still "/mid/a".
	relocateMetaNode(t, sid, "/a", "/b")

	// Reclaim /mid/a with a brand-new meta. Before the fix this collides on the
	// unique(path) key with m1's stale stored Path.
	m2 := &model.Meta{Path: "/mid/a", Password: "p2"}
	if err := CreateMeta(m2); err != nil {
		t.Fatalf("create meta at reclaimed path: %v", err)
	}
	if m2.NodeID == 0 || m2.NodeID == m1.NodeID {
		t.Fatalf("reclaimed meta must anchor a fresh node: m2.NodeID=%d m1.NodeID=%d", m2.NodeID, m1.NodeID)
	}

	// Both live: /mid/a resolves to the new meta, /mid/b to the moved one.
	if got, err := GetNearestMeta("/mid/a"); err != nil || got.Password != "p2" {
		t.Fatalf("/mid/a must resolve to reclaimed meta (p2), got %+v err %v", got, err)
	}
	if got, err := GetNearestMeta("/mid/b"); err != nil || got.Password != "p1" {
		t.Fatalf("/mid/b must resolve to moved meta (p1), got %+v err %v", got, err)
	}
}

// TestCreateMetaRejectsDuplicateLivePath keeps the meta-per-directory invariant:
// two metas at the same LIVE path is rejected (the check moved from the DB unique
// key to op, keyed on the derived live path).
func TestCreateMetaRejectsDuplicateLivePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/middup", dir)

	if err := CreateMeta(&model.Meta{Path: "/middup/x", Password: "a"}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := CreateMeta(&model.Meta{Path: "/middup/x", Password: "b"}); err == nil {
		t.Fatal("second meta at the same live path must be rejected")
	}
}

// TestUpdateMetaOntoOccupiedLivePathRejected: moving a meta (by editing its Path)
// onto a path already owned by a different meta is rejected.
func TestUpdateMetaOntoOccupiedLivePathRejected(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"p", "q"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	createTestStorage(t, "/midupd", dir)

	if err := CreateMeta(&model.Meta{Path: "/midupd/p", Password: "a"}); err != nil {
		t.Fatalf("create p: %v", err)
	}
	mq := &model.Meta{Path: "/midupd/q", Password: "b"}
	if err := CreateMeta(mq); err != nil {
		t.Fatalf("create q: %v", err)
	}

	mq.Path = "/midupd/p"
	if err := UpdateMeta(mq); err == nil {
		t.Fatal("updating meta onto an occupied live path must be rejected")
	}
}
