package op

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestStorageDeleteSkipsAnchoredRowInDecision is the spec §7 acceptance case:
// after its storage is deleted a node-anchored permission derives no path, so
// the policy engine SKIPS it (it stops covering anything) — but the admin list
// endpoint still returns the row (with a blanked Path) so it can be cleaned up.
func TestStorageDeleteSkipsAnchoredRowInDecision(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := createTestStorage(t, "/del7", dir)

	p := &model.Permission{Name: "del7-read", Path: "/del7/docs",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	u := makeRelocPermUser(t, "del7U", []*model.Permission{p})
	if p.NodeID == 0 {
		t.Fatalf("permission must be node-anchored: %+v", p)
	}

	// Live: covered.
	if d, err := ResolveEffectivePolicy(u, "/del7/docs"); err != nil || !d.Can(model.ActRead) {
		t.Fatalf("precondition: must be readable while storage live: %+v %v", d, err)
	}

	if err := DeleteStorageById(context.Background(), sid); err != nil {
		t.Fatalf("delete storage: %v", err)
	}
	InvalidateRolePolicyForTest(uint(u.Role[0]))

	// Decision: the row derives no path and is skipped entirely.
	if d, err := ResolveEffectivePolicy(u, "/del7/docs"); err != nil || d.Can(model.ActRead) {
		t.Fatalf("row on a deleted storage must be skipped in decisions: %+v %v", d, err)
	}

	// Admin list: the row is still present (not filtered), Path blanked to flag
	// it unresolved.
	ps, _, err := GetPermissions(1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range ps {
		if ps[i].ID == p.ID {
			found = true
			if ps[i].Path != "" {
				t.Fatalf("deleted-storage row Path must be blanked for display, got %q", ps[i].Path)
			}
		}
	}
	if !found {
		t.Fatal("deleted-storage row must remain listed for manual cleanup (spec §7)")
	}
}

// TestFirstLoadAdoptionIsIdempotent is the spec §6 acceptance case: a legacy /
// migration row that carries a Path under a loaded storage but no node is
// adopted (node_id backfilled) on load; running adoption again changes nothing.
func TestFirstLoadAdoptionIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/adopt", dir)

	// Simulate migration/legacy rows: written straight to the DB, so they carry
	// a virtual Path but no node yet (as migratev4.writeMetas / a hand-edit do).
	perm := &model.Permission{Name: "adopt-p", Path: "/adopt/a/b", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	if err := db.CreatePermission(perm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(perm.ID) })
	meta := &model.Meta{Path: "/adopt/a/b", Password: "x"}
	if err := db.CreateMeta(meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteMetaById(meta.ID); invalidateMetaIndex() })

	// First adoption backfills node_id on both rows.
	adoptNodelessAnchors()
	gotP, err := db.GetPermissionById(perm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotP.NodeID == 0 {
		t.Fatalf("permission not adopted: %+v", gotP)
	}
	gotM, err := db.GetMetaById(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotM.NodeID == 0 {
		t.Fatalf("meta not adopted: %+v", gotM)
	}
	if gotP.NodeID != gotM.NodeID {
		t.Fatalf("permission and meta at the same path must share one node: %d vs %d", gotP.NodeID, gotM.NodeID)
	}
	nid := gotP.NodeID

	// Second adoption is a no-op: node_id unchanged.
	adoptNodelessAnchors()
	gotP2, err := db.GetPermissionById(perm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotP2.NodeID != nid {
		t.Fatalf("adoption not idempotent: node_id changed %d -> %d", nid, gotP2.NodeID)
	}
	if vp, err := NodeVirtualPath(nid); err != nil || vp != "/adopt/a/b" {
		t.Fatalf("adopted node must resolve at the original path: vp=%q err=%v", vp, err)
	}
}
