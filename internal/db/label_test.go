package db

import (
	"fmt"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupLabelTestDB swaps the package db handle for a fresh in-memory database
// carrying the three tables the label layer touches (nodes for the follow-move
// test, plus labels and label_bindings). Mirrors setupNodeTestDB.
func setupLabelTestDB(t *testing.T) {
	t.Helper()
	dsn := fmt.Sprintf("file:label_test_%s?mode=memory&cache=shared", t.Name())
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Node{}, &model.Label{}, &model.LabelBinding{}); err != nil {
		t.Fatal(err)
	}
	old := db
	db = dB
	t.Cleanup(func() { db = old })
}

func TestLabelCRUD(t *testing.T) {
	setupLabelTestDB(t)

	l := &model.Label{Name: "urgent", Color: "#ff0000"}
	if err := CreateLabel(l); err != nil {
		t.Fatalf("create: %v", err)
	}
	if l.ID == 0 {
		t.Fatal("id not assigned")
	}

	got, err := GetLabelByID(l.ID)
	if err != nil || got.Name != "urgent" || got.Color != "#ff0000" {
		t.Fatalf("get by id: %+v err=%v", got, err)
	}

	byName, found, err := GetLabelByName("urgent")
	if err != nil || !found || byName.ID != l.ID {
		t.Fatalf("get by name: found=%v id=%v err=%v", found, byName, err)
	}
	if _, found, err := GetLabelByName("nope"); err != nil || found {
		t.Fatalf("missing name must be not-found: found=%v err=%v", found, err)
	}

	l.Color = "#00ff00"
	if err := UpdateLabel(l); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ := GetLabelByID(l.ID); got.Color != "#00ff00" {
		t.Fatalf("update did not persist color: %+v", got)
	}

	ls, err := ListLabels()
	if err != nil || len(ls) != 1 {
		t.Fatalf("list: %v len=%d", err, len(ls))
	}
}

func TestBindLabelIdempotentAndUnbind(t *testing.T) {
	setupLabelTestDB(t)

	l := &model.Label{Name: "a"}
	if err := CreateLabel(l); err != nil {
		t.Fatal(err)
	}
	const nodeID = uint(42)

	if err := BindLabel(l.ID, nodeID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Re-binding the same (label,node) is a no-op, not an error or a duplicate.
	if err := BindLabel(l.ID, nodeID); err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	if n := countBindings(t); n != 1 {
		t.Fatalf("expected 1 binding after idempotent re-bind, got %d", n)
	}

	labels, err := LabelsByNode(nodeID)
	if err != nil || len(labels) != 1 || labels[0].ID != l.ID {
		t.Fatalf("labels by node: %v %+v", err, labels)
	}

	if err := UnbindLabel(l.ID, nodeID); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if n := countBindings(t); n != 0 {
		t.Fatalf("expected 0 bindings after unbind, got %d", n)
	}
	// Unbinding an absent binding is a no-op.
	if err := UnbindLabel(l.ID, nodeID); err != nil {
		t.Fatalf("unbind absent: %v", err)
	}
	if labels, _ := LabelsByNode(nodeID); len(labels) != 0 {
		t.Fatalf("expected no labels after unbind, got %+v", labels)
	}
}

func TestLabelsByNodeMultiple(t *testing.T) {
	setupLabelTestDB(t)

	l1 := &model.Label{Name: "one"}
	l2 := &model.Label{Name: "two"}
	if err := CreateLabel(l1); err != nil {
		t.Fatal(err)
	}
	if err := CreateLabel(l2); err != nil {
		t.Fatal(err)
	}
	const nodeID = uint(7)
	if err := BindLabel(l1.ID, nodeID); err != nil {
		t.Fatal(err)
	}
	if err := BindLabel(l2.ID, nodeID); err != nil {
		t.Fatal(err)
	}
	// A different node shares one label; it must not bleed into node 7's set.
	if err := BindLabel(l1.ID, 99); err != nil {
		t.Fatal(err)
	}

	labels, err := LabelsByNode(nodeID)
	if err != nil || len(labels) != 2 {
		t.Fatalf("expected 2 labels on node, got %d (err %v)", len(labels), err)
	}
}

func TestDeleteLabelCascadesBindings(t *testing.T) {
	setupLabelTestDB(t)

	l := &model.Label{Name: "doomed"}
	if err := CreateLabel(l); err != nil {
		t.Fatal(err)
	}
	for _, nid := range []uint{1, 2, 3} {
		if err := BindLabel(l.ID, nid); err != nil {
			t.Fatal(err)
		}
	}
	if n := countBindings(t); n != 3 {
		t.Fatalf("expected 3 bindings, got %d", n)
	}

	if err := DeleteLabel(l.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := GetLabelByID(l.ID); err == nil {
		t.Fatal("label should be gone")
	}
	if n := countBindings(t); n != 0 {
		t.Fatalf("delete label must cascade its bindings, got %d left", n)
	}
}

func TestBindingsByNodeIDsAndByLabel(t *testing.T) {
	setupLabelTestDB(t)

	l1 := &model.Label{Name: "x"}
	l2 := &model.Label{Name: "y"}
	if err := CreateLabel(l1); err != nil {
		t.Fatal(err)
	}
	if err := CreateLabel(l2); err != nil {
		t.Fatal(err)
	}
	// node 10 -> l1,l2 ; node 20 -> l1 ; node 30 -> none
	for _, b := range []struct{ lid, nid uint }{
		{l1.ID, 10}, {l2.ID, 10}, {l1.ID, 20},
	} {
		if err := BindLabel(b.lid, b.nid); err != nil {
			t.Fatal(err)
		}
	}

	// Batch decorator lookup: bindings for the visible node set.
	bs, err := BindingsByNodeIDs([]uint{10, 20, 30})
	if err != nil {
		t.Fatalf("bindings by node ids: %v", err)
	}
	if len(bs) != 3 {
		t.Fatalf("expected 3 bindings across nodes, got %d", len(bs))
	}
	if empty, err := BindingsByNodeIDs(nil); err != nil || empty != nil {
		t.Fatalf("empty input must be empty result: %v %v", empty, err)
	}

	// By-label view: node ids carrying l1.
	nodes, err := BindingsByLabel(l1.ID)
	if err != nil {
		t.Fatalf("bindings by label: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected l1 on 2 nodes, got %d (%v)", len(nodes), nodes)
	}
}

// TestLabelFollowsNodeRelocation is the "labels follow moves" proof at the data
// layer: bind a label onto the node at /x/dir, relocate that node to /y/dir
// (the single edit the move/rename relocate handler performs), then look the new
// path up — it resolves to the SAME node, and that node still carries the label.
// The binding is never touched; it follows because it anchors node_id.
func TestLabelFollowsNodeRelocation(t *testing.T) {
	setupLabelTestDB(t)
	const sid = uint(3)

	dirNode, err := EnsureNodeChain(sid, "/x/dir")
	if err != nil {
		t.Fatalf("ensure /x/dir: %v", err)
	}
	l := &model.Label{Name: "keep-me"}
	if err := CreateLabel(l); err != nil {
		t.Fatal(err)
	}
	if err := BindLabel(l.ID, dirNode); err != nil {
		t.Fatal(err)
	}

	// Relocate /x/dir -> /y/dir: create the new parent chain, repoint the node.
	newParent, err := EnsureNodeChain(sid, "/y")
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeParentName(dirNode, newParent, "dir"); err != nil {
		t.Fatalf("relocate node: %v", err)
	}

	// New path resolves to the same node identity.
	got, found, err := GetNodeByChain(sid, "/y/dir")
	if err != nil || !found {
		t.Fatalf("resolve /y/dir: found=%v err=%v", found, err)
	}
	if got != dirNode {
		t.Fatalf("relocated node identity changed: %d != %d", got, dirNode)
	}
	// The label is still attached, addressed by the new path's node.
	labels, err := LabelsByNode(got)
	if err != nil || len(labels) != 1 || labels[0].ID != l.ID {
		t.Fatalf("label did not follow move: %v %+v", err, labels)
	}
	// And the old path no longer resolves to that node.
	if _, found, _ := GetNodeByChain(sid, "/x/dir"); found {
		t.Fatal("old path /x/dir should no longer resolve after move")
	}
}

func countBindings(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.LabelBinding{}).Count(&n).Error; err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	return n
}
