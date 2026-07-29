package migration

import (
	"fmt"
	"testing"

	"gorm.io/gorm"
)

// 0009 builds the nodes table, backfills node_id onto relpath-anchored
// permission (manual only) and meta rows — leaving scope rows and above-mount
// (zero-storage) rows unanchored — then drops the anchor triple. This test
// exercises the backfill half in isolation (build + backfill, no drop) so it
// can seed and read the anchor columns; the drop half is covered by
// TestNodesMigrationDropsAnchorTriple below. Re-running the backfill is a no-op.
func TestNodesMigrationBackfill(t *testing.T) {
	d := openMem(t, "nodes_mig")
	mig := New(d, testPrefix)
	if err := mig.UpTo("0008_permission_display_name"); err != nil {
		t.Fatalf("up to 0008: %v", err)
	}

	permT := testPrefix + "permissions"
	metaT := d.NamingStrategy.TableName("meta")

	seed := []string{
		// manual, relpath-anchored -> adopted
		fmt.Sprintf("INSERT INTO %s (name, path, storage_id, anchor, anchor_kind, effect, recursive, origin) VALUES ('manual', '/mnt/a/b', 2, '/a/b', 0, 'allow', 1, 'manual')", permT),
		// scope-origin -> skipped (stays node_id 0)
		fmt.Sprintf("INSERT INTO %s (name, path, storage_id, anchor, anchor_kind, effect, recursive, origin) VALUES ('role:x:scope:0', '/mnt/a', 2, '/a', 0, 'allow', 1, 'scope')", permT),
		// above every mount (storage_id 0) -> skipped
		fmt.Sprintf("INSERT INTO %s (name, path, storage_id, anchor, anchor_kind, effect, recursive, origin) VALUES ('virtual', '/', 0, '', 0, 'allow', 1, 'manual')", permT),
		// meta, relpath-anchored -> adopted, reuses the manual row's chain
		fmt.Sprintf("INSERT INTO %s (path, storage_id, anchor, anchor_kind) VALUES ('/mnt/a/b', 2, '/a/b', 0)", metaT),
	}
	for _, s := range seed {
		if err := d.Exec(s).Error; err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	// Mirror 0009's Migrate up to (but not including) the drop, so the anchor
	// columns the assertions read are still present.
	if err := d.AutoMigrate(&node{}, &permissionNodeID{}, &metaNodeID{}); err != nil {
		t.Fatalf("build nodes registry: %v", err)
	}
	if err := backfillNodeIDs(d); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	manualNode := nodeIDOf(t, d, permT, "manual")
	if manualNode == 0 {
		t.Fatal("manual permission not adopted (node_id 0)")
	}
	if got := nodeIDOf(t, d, permT, "role:x:scope:0"); got != 0 {
		t.Fatalf("scope row was adopted: node_id=%d", got)
	}
	if got := nodeIDOf(t, d, permT, "virtual"); got != 0 {
		t.Fatalf("above-mount row was adopted: node_id=%d", got)
	}
	metaNode := metaNodeIDOf(t, d, metaT, "/mnt/a/b")
	if metaNode == 0 {
		t.Fatal("meta not adopted (node_id 0)")
	}
	if metaNode != manualNode {
		t.Fatalf("meta and permission on the same anchor got different nodes: %d != %d", metaNode, manualNode)
	}

	// root + a + b, shared by both rows.
	if n := countRows(t, d, testPrefix+"nodes"); n != 3 {
		t.Fatalf("expected 3 nodes, got %d", n)
	}

	// Idempotent: re-running the backfill creates nothing and moves no row.
	if err := backfillNodeIDs(d); err != nil {
		t.Fatalf("re-backfill: %v", err)
	}
	if n := countRows(t, d, testPrefix+"nodes"); n != 3 {
		t.Fatalf("re-backfill created nodes: %d", n)
	}
	if got := nodeIDOf(t, d, permT, "manual"); got != manualNode {
		t.Fatalf("re-backfill moved manual node_id: %d != %d", got, manualNode)
	}
}

// TestNodesMigrationDropsAnchorTriple runs the full migration set and checks
// that 0009's final step removed the storage_id/anchor/anchor_kind columns and
// the composite anchor index from both tables, while keeping node_id and path.
func TestNodesMigrationDropsAnchorTriple(t *testing.T) {
	d := openMem(t, "nodes_drop")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	m := d.Migrator()

	targets := []struct {
		tbl   interface{}
		index string
	}{
		{&permissionNodeID{}, "idx_permission_anchor"},
		{&metaNodeID{}, "idx_meta_anchor"},
	}
	for _, tt := range targets {
		for _, col := range []string{"storage_id", "anchor", "anchor_kind"} {
			if m.HasColumn(tt.tbl, col) {
				t.Errorf("column %q must be dropped by 0009 but is still present", col)
			}
		}
		if m.HasIndex(tt.tbl, tt.index) {
			t.Errorf("index %q must be dropped by 0009 but is still present", tt.index)
		}
		// The node_id column and the path column survive the drop.
		if !m.HasColumn(tt.tbl, "node_id") {
			t.Errorf("node_id column must survive the drop")
		}
		if !m.HasColumn(tt.tbl, "path") {
			t.Errorf("path column must survive the drop")
		}
	}
}

func nodeIDOf(t *testing.T, d *gorm.DB, table, name string) uint {
	t.Helper()
	var nid uint
	if err := d.Table(table).Where("name = ?", name).Select("COALESCE(node_id, 0)").Scan(&nid).Error; err != nil {
		t.Fatalf("read node_id of %q: %v", name, err)
	}
	return nid
}

func metaNodeIDOf(t *testing.T, d *gorm.DB, table, path string) uint {
	t.Helper()
	var nid uint
	if err := d.Table(table).Where("path = ?", path).Select("COALESCE(node_id, 0)").Scan(&nid).Error; err != nil {
		t.Fatalf("read meta node_id of %q: %v", path, err)
	}
	return nid
}

func countRows(t *testing.T, d *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := d.Table(table).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
