package op

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// mustCreateMeta creates a meta via op.CreateMeta so it is anchored to a node
// (when its path resolves to a loaded storage) exactly as a real admin create
// would be — node-based relocation only follows rows that carry a NodeID. It
// registers cleanup so the row does not leak into later tests sharing this
// package's process-global sqlite cache=shared connection and meta index.
func mustCreateMeta(t *testing.T, m *model.Meta) {
	t.Helper()
	if err := CreateMeta(m); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	t.Cleanup(func() {
		_ = db.DeleteMetaById(m.ID)
		invalidateMetaIndex()
	})
}

// S1 红线：受保护目录被 rename/move 后，密码/隐藏保护自动跟随。
// 断言刻意先读热缓存再操作——防"DB 改了、缓存还是旧的"假绿。
func TestS1MetaFollowsRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "private/inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/s1", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	// 两层 meta：/s1/private（密码）与 /s1/private/inner（隐藏）
	metaTop := &model.Meta{Path: "/s1/private", Password: "pw", PSub: true}
	metaInner := &model.Meta{Path: "/s1/private/inner", Hide: "secret.*", HSub: true}
	for _, m := range []*model.Meta{metaTop, metaInner} {
		mustCreateMeta(t, m)
	}
	// 预热缓存：先命中一次两个即将随 rename 迁移的路径，好让后续断言真正
	// 检验"DB 改了、缓存也跟着失效"，而不是从未命中过缓存的假绿。
	if _, err := GetNearestMeta("/s1/private/inner/x"); err != nil {
		t.Fatalf("warm cache (inner): %v", err)
	}
	if _, err := GetNearestMeta("/s1/private"); err != nil {
		t.Fatalf("warm cache (top): %v", err)
	}

	storage, _, err := GetStorageAndActualPath("/s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storage, "/private", "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// 新路径立即受保护（穿缓存）
	got, err := GetNearestMeta("/s1/renamed")
	if err != nil {
		t.Fatalf("meta after rename: %v", err)
	}
	if got.Password != "pw" {
		t.Fatalf("S1 violated: password protection lost after rename, meta=%+v", got)
	}
	inner, err := GetNearestMeta("/s1/renamed/inner")
	if err != nil {
		t.Fatalf("inner meta after rename: %v", err)
	}
	if inner.Hide == "" {
		t.Fatalf("S1 violated: subtree meta did not follow, meta=%+v", inner)
	}
	// 旧路径不再命中该 meta
	if m, err := GetNearestMeta("/s1/private"); err == nil && m.Password == "pw" && m.Path == "/s1/private" {
		t.Fatalf("stale meta still on old path: %+v", m)
	}
	// 节点侧:inner meta 的节点已重定位,派生路径跟到新位置(不再依赖 Path 列)
	fresh, err := db.GetMetaById(metaInner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.NodeID == 0 {
		t.Fatalf("inner meta must be node-anchored: %+v", fresh)
	}
	if vp, err := NodeVirtualPath(fresh.NodeID); err != nil || vp != "/s1/renamed/inner" {
		t.Fatalf("inner meta node did not follow rename: vp=%q err=%v", vp, err)
	}
}

// TestS1MetaFollowsRenameBalanceGroup is the balance-group variant of S1.
// The meta's node lives under the group's PRIMARY member ("/bg",
// balanceGroupPrimary in storage.go), but GetBalancedStorage can route an
// actual fs op to ANY member on any given call. fireObjRelocate must report the
// primary's storage_id regardless of which member's driver executed the op —
// otherwise a rename that happens to land on the non-anchor member finds no
// node at (member storage, oldRel) and the meta silently stays behind at the
// old path instead of following the object.
func TestS1MetaFollowsRenameBalanceGroup(t *testing.T) {
	primaryDir := t.TempDir()
	secondaryDir := t.TempDir()
	// The rename below runs against the SECONDARY member's own directory
	// (simulating GetBalancedStorage having routed the op there); the
	// primary's directory does not need to mirror it for this test.
	if err := os.MkdirAll(filepath.Join(secondaryDir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/bg", primaryDir)
	createTestStorage(t, "/bg.balance2", secondaryDir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	meta := &model.Meta{Path: "/bg/target", Password: "pw", PSub: true}
	mustCreateMeta(t, meta)

	secondary, err := GetStorageByMountPath("/bg.balance2")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), secondary, "/target", "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, err := GetNearestMeta("/bg/renamed")
	if err != nil {
		t.Fatalf("meta after balance-group rename: %v", err)
	}
	if got.Password != "pw" {
		t.Fatalf("S1 violated across balance group: password protection lost after a rename routed to the non-anchor member, meta=%+v", got)
	}
	fresh, err := db.GetMetaById(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The anchor storage_id normalized to the primary member, so the node's
	// derived virtual path resolves to the group's mount, not a member path.
	if fresh.NodeID == 0 {
		t.Fatalf("balance-group meta must be node-anchored: %+v", fresh)
	}
	if vp, err := NodeVirtualPath(fresh.NodeID); err != nil || vp != "/bg/renamed" {
		t.Fatalf("meta node did not follow balance-group relocation: vp=%q err=%v", vp, err)
	}
}

// TestMetaSurvivesRemove: under node-based anchoring a removal does not touch
// nodes (spec §1), so a meta's password/hide protection survives deleting its
// object and reapplies if a same-named object is recreated — the meta half of
// the "deny rules must not silently vanish" invariant that also governs
// permissions (TestPermissionSurvivesRemove). This inverts the pre-v4 behavior
// where removeMetas deleted the meta with the object.
func TestMetaSurvivesRemove(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/s1r", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)
	m := &model.Meta{Path: "/s1r/gone", Password: "pw"}
	mustCreateMeta(t, m)
	storage, _, _ := GetStorageAndActualPath("/s1r")
	if err := Remove(context.Background(), storage, "/gone"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Row and its node both survive; protection still derives at the same path.
	if _, err := db.GetMetaById(m.ID); err != nil {
		t.Fatalf("meta must survive object removal: %v", err)
	}
	got, err := GetNearestMeta("/s1r/gone")
	if err != nil || got.Password != "pw" {
		t.Fatalf("meta protection must survive removal (reapplies on recreate): %+v %v", got, err)
	}
}

// TestRelocateHookPanicIsolated: a panicking hook must not corrupt the fs
// operation it observes, nor stop other registered hooks from running.
func TestRelocateHookPanicIsolated(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/panic", dir)
	t.Cleanup(resetObjRelocateHooks)

	RegisterObjRelocateHook(func(storageID uint, kind RelocateKind, oldRel, newRel string, isDir bool) {
		panic("boom")
	})
	var called bool
	RegisterObjRelocateHook(func(storageID uint, kind RelocateKind, oldRel, newRel string, isDir bool) {
		called = true
	})

	storage, _, err := GetStorageAndActualPath("/panic")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storage, "/a", "b"); err != nil {
		t.Fatalf("rename must succeed despite a panicking hook: %v", err)
	}
	if !called {
		t.Fatal("hook registered after the panicking one was never invoked")
	}
}

// TestMetaRelocationSiblingIsolation: renaming "/a_b" (a directory whose meta
// sits on a child) must move only that subtree, never the sibling "/aXb". With
// node-based relocation this is structural — siblings are distinct nodes under
// the root — so the old anchor-prefix LIKE-wildcard hazard (a literal '_' in
// "a_b" matching "aXb") cannot arise at all.
func TestMetaRelocationSiblingIsolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a_b/child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "aXb/child2"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/esc", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	target := &model.Meta{Path: "/esc/a_b/child", Password: "pw"}
	sibling := &model.Meta{Path: "/esc/aXb/child2", Password: "pw2"}
	for _, m := range []*model.Meta{target, sibling} {
		mustCreateMeta(t, m)
	}

	storage, _, err := GetStorageAndActualPath("/esc")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storage, "/a_b", "renamed_ab"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// target subtree followed
	if got, err := GetNearestMeta("/esc/renamed_ab/child"); err != nil || got.Password != "pw" {
		t.Fatalf("target meta did not follow rename: %+v %v", got, err)
	}
	// sibling untouched, still at its original path
	if got, err := GetNearestMeta("/esc/aXb/child2"); err != nil || got.Password != "pw2" {
		t.Fatalf("unrelated sibling meta was affected by the rename: %+v %v", got, err)
	}
	// and the sibling did NOT get pulled under the renamed subtree
	if m, err := GetNearestMeta("/esc/renamed_ab/child2"); err == nil && m.Password == "pw2" {
		t.Fatalf("sibling meta wrongly relocated under renamed subtree: %+v", m)
	}
}

// TestMountRootMetaUnaffectedByInnerRename covers a mount-root meta (anchored on
// the storage-root node). A meta protecting the whole storage must stay put
// (both DB row and derived path) when an unrelated inner directory is renamed:
// the rename repoints only the inner directory's node, a distinct node the root
// meta does not depend on.
func TestMountRootMetaUnaffectedByInnerRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/root1", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)

	rootMeta := &model.Meta{Path: "/root1", Password: "rootpw", PSub: true}
	mustCreateMeta(t, rootMeta)
	// warm the index for the mount root before the unrelated rename
	if _, err := GetNearestMeta("/root1"); err != nil {
		t.Fatalf("warm index: %v", err)
	}

	storage, _, err := GetStorageAndActualPath("/root1")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), storage, "/inner", "inner2"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// The mount-root meta anchors on the storage-root node; renaming an inner
	// directory (a different node, in fact one nothing anchored) must not move
	// it. Its derived path stays the mount root.
	fresh, err := db.GetMetaById(rootMeta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vp, err := NodeVirtualPath(fresh.NodeID); err != nil || vp != "/root1" {
		t.Fatalf("mount-root meta node corrupted by an unrelated inner rename: vp=%q err=%v", vp, err)
	}
	got, err := GetNearestMeta("/root1")
	if err != nil {
		t.Fatalf("meta after unrelated rename: %v", err)
	}
	if got.Password != "rootpw" {
		t.Fatalf("mount-root protection lost after an unrelated inner rename, meta=%+v", got)
	}
}

func TestMetaCreateBackfillsAnchor(t *testing.T) {
	dir := t.TempDir()
	sid := createTestStorage(t, "/dw", dir)
	m := &model.Meta{Path: "/dw/x"}
	if err := CreateMeta(m); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteMetaById(m.ID); invalidateMetaIndex() })
	if m.NodeID == 0 {
		t.Fatalf("node not backfilled on create: %+v", m)
	}
	if node, found, err := db.GetNodeByChain(sid, "/x"); err != nil || !found || node != m.NodeID {
		t.Fatalf("meta node not anchored at /x: node=%d found=%v err=%v", node, found, err)
	}
	// A meta above every mount point ("/" — the true root) stays path-anchored
	// (no node). It MUST be cleaned up, or GetNearestMeta's climb-to-root
	// fallback in every later test in this package (shared process-global
	// sqlite connection) silently picks it up as a false match.
	v := &model.Meta{Path: "/"}
	if err := CreateMeta(v); err != nil {
		t.Fatalf("virtual create: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteMetaById(v.ID); invalidateMetaIndex() })
	if v.NodeID != 0 {
		t.Fatalf("virtual meta should stay path-anchored (no node): %+v", v)
	}

	// Update path to /dw/y and verify the node follows (derived path tracks it).
	m.Path = "/dw/y"
	if err := UpdateMeta(m); err != nil {
		t.Fatalf("update: %v", err)
	}
	fresh, err := db.GetMetaById(m.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if vp, err := NodeVirtualPath(fresh.NodeID); err != nil || vp != "/dw/y" {
		t.Fatalf("meta node did not follow path change: vp=%q err=%v", vp, err)
	}
}

func TestRelocateConcurrentWithReads(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 8; i++ {
		if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("d%d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	createTestStorage(t, "/cc", dir)
	RegisterNodeRelocation()
	t.Cleanup(resetObjRelocateHooks)
	for i := 0; i < 8; i++ {
		mustCreateMeta(t, &model.Meta{Path: fmt.Sprintf("/cc/d%d", i), Password: "p"})
	}
	storage, _, _ := GetStorageAndActualPath("/cc")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = Rename(context.Background(), storage, fmt.Sprintf("/d%d", i), fmt.Sprintf("r%d", i))
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = GetNearestMeta(fmt.Sprintf("/cc/d%d/x", i))
		}(i)
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		wantPath := fmt.Sprintf("/cc/r%d", i)
		m, err := GetNearestMeta(wantPath)
		if err != nil {
			t.Errorf("meta %d lost after concurrent rename: err=%v", i, err)
			continue
		}
		// An exact path+password match rules out GetNearestMeta silently
		// falling back to an unrelated ancestor meta (e.g. a stale root-level
		// entry from cross-test pollution) instead of surfacing the loss.
		if m.Path != wantPath || m.Password != "p" {
			t.Errorf("meta %d lost after concurrent rename: got path=%q password=%q (want path=%q password=\"p\"), full meta=%+v",
				i, m.Path, m.Password, wantPath, m)
		}
	}
}
