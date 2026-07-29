package op

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

func TestCreateLabelValidation(t *testing.T) {
	// Bad color is rejected before any DB write.
	if err := CreateLabel(&model.Label{Name: "bad", Color: "red"}); err == nil {
		t.Fatal("non-hex color must be rejected")
	}
	if err := CreateLabel(&model.Label{Name: "bad2", Color: "#12345"}); err == nil {
		t.Fatal("five-digit hex must be rejected")
	}
	// Empty name (after trim) is rejected.
	if err := CreateLabel(&model.Label{Name: "   "}); err == nil {
		t.Fatal("blank name must be rejected")
	}

	// A valid label with an empty color and a trimmed name succeeds.
	l := &model.Label{Name: "  todo  ", Color: ""}
	if err := CreateLabel(l); err != nil {
		t.Fatalf("valid label rejected: %v", err)
	}
	if l.Name != "todo" {
		t.Fatalf("name not trimmed: %q", l.Name)
	}
	t.Cleanup(func() { _ = DeleteLabel(l.ID) })

	// A #RRGGBB color is accepted.
	l2 := &model.Label{Name: "review", Color: "#A1B2C3"}
	if err := CreateLabel(l2); err != nil {
		t.Fatalf("hex color rejected: %v", err)
	}
	t.Cleanup(func() { _ = DeleteLabel(l2.ID) })

	// Duplicate name is rejected.
	if err := CreateLabel(&model.Label{Name: "todo"}); err == nil {
		t.Fatal("duplicate name must be rejected")
	}
}

func TestUpdateLabelNameUniqueness(t *testing.T) {
	a := &model.Label{Name: "alpha"}
	b := &model.Label{Name: "beta"}
	if err := CreateLabel(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteLabel(a.ID) })
	if err := CreateLabel(b); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteLabel(b.ID) })

	// Renaming b onto a's name collides.
	b.Name = "alpha"
	if err := UpdateLabel(b); err == nil {
		t.Fatal("renaming onto an existing name must be rejected")
	}

	// A color-only edit (same name) is allowed — uniqueness excludes self.
	a.Color = "#000000"
	if err := UpdateLabel(a); err != nil {
		t.Fatalf("color-only edit rejected: %v", err)
	}
}

// TestApplyAndListLabels drives the full op path: mount a storage, create a
// label, apply it to a path (anchoring a node on demand), and read it back.
func TestApplyAndListLabels(t *testing.T) {
	ctx := context.Background()
	createTestStorage(t, "/lbl", t.TempDir())

	l := &model.Label{Name: "apply-me", Color: "#ff8800"}
	if err := CreateLabel(l); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteLabel(l.ID) })

	// Before applying, the path has no labels (no node) — empty, not an error.
	if labels, err := LabelsOf("/lbl/docs"); err != nil || len(labels) != 0 {
		t.Fatalf("unanchored path must have no labels: %v %+v", err, labels)
	}

	if err := ApplyLabel(ctx, "/lbl/docs", l.ID); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Idempotent re-apply.
	if err := ApplyLabel(ctx, "/lbl/docs", l.ID); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	labels, err := LabelsOf("/lbl/docs")
	if err != nil || len(labels) != 1 || labels[0].ID != l.ID {
		t.Fatalf("labels of path: %v %+v", err, labels)
	}

	if err := RemoveLabel(ctx, "/lbl/docs", l.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if labels, _ := LabelsOf("/lbl/docs"); len(labels) != 0 {
		t.Fatalf("label not removed: %+v", labels)
	}
	// Removing again (or from a never-anchored path) is a no-op.
	if err := RemoveLabel(ctx, "/lbl/never", l.ID); err != nil {
		t.Fatalf("remove from unanchored path must be a no-op: %v", err)
	}
}

// TestLabelFollowsMoveByVirtualPath is the op-level "labels follow moves" proof:
// apply a label to /lblmv/x/dir, then relocate the node (what the move/rename
// relocate handler does), and query by the NEW virtual path — the label is still
// there, and the old path no longer carries it. The binding is never rewritten;
// it follows because it anchors the node that moved.
func TestLabelFollowsMoveByVirtualPath(t *testing.T) {
	ctx := context.Background()
	sid := createTestStorage(t, "/lblmv", t.TempDir())

	l := &model.Label{Name: "follows"}
	if err := CreateLabel(l); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteLabel(l.ID) })

	if err := ApplyLabel(ctx, "/lblmv/x/dir", l.ID); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Relocate the node /x/dir -> /y/dir, mirroring the relocate handler, then
	// drop the derived-path caches like a real move does.
	if err := relocateNode(sid, "/x/dir", "/y/dir"); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	InvalidateAllNodePathCache()

	// The label is reachable at the new virtual path.
	labels, err := LabelsOf("/lblmv/y/dir")
	if err != nil || len(labels) != 1 || labels[0].ID != l.ID {
		t.Fatalf("label did not follow move: %v %+v", err, labels)
	}
	// And it is no longer at the old virtual path.
	if labels, _ := LabelsOf("/lblmv/x/dir"); len(labels) != 0 {
		t.Fatalf("label still at old path after move: %+v", labels)
	}
}
