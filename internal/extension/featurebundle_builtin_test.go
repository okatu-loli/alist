package extension

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// decoratingBundle is the labels-shaped built-in: a FeatureBundle value that
// ALSO implements FileDecorator (group "_labels"). It contributes a FileColumn
// backed by that decorator.
type decoratingBundle struct {
	id string
}

func (b *decoratingBundle) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    b.id,
		Name:  map[string]string{"en": "Labels"},
		Kinds: []ext.Kind{ext.KindFeatureBundle, ext.KindFileDecorator},
		Contributions: ext.Contributions{
			FileColumns: []ext.FileColumn{{ID: "labels", FieldGroup: "_labels"}},
			MenuActions: []ext.MenuAction{{ID: "labels", Target: "file"}},
		},
	}
}
func (b *decoratingBundle) Init(context.Context, ext.Host) error { return nil }
func (b *decoratingBundle) Shutdown(context.Context) error       { return nil }
func (b *decoratingBundle) Bundle() ext.BundleSpec               { return ext.BundleSpec{} }

func (b *decoratingBundle) Groups() []string { return []string{"_labels"} }
func (b *decoratingBundle) Decorate(_ context.Context, group string, refs []ext.ObjRef) (map[string]any, error) {
	if group != "_labels" {
		return nil, nil
	}
	out := map[string]any{}
	for _, rf := range refs {
		out[ext.RefKey(rf)] = []string{"tag"}
	}
	return out, nil
}

// RegisterBuiltinFeatureBundle must make the labels bundle reachable through
// every leg it contributes: the FileDecorator (ByKind + DecorateFields), and its
// contributions (FileColumn/MenuAction) through AggregateContributions.
func TestRegisterBuiltinFeatureBundleWiresDecoratorAndContributions(t *testing.T) {
	r := NewRegistry()
	b := &decoratingBundle{id: "builtin.labels"}
	if err := RegisterBuiltinFeatureBundle(r, b); err != nil {
		t.Fatalf("RegisterBuiltinFeatureBundle: %v", err)
	}
	t.Cleanup(func() { CollapseFeatureBundle(b.id) })

	// Reachable as a FileDecorator.
	if got := r.ByKind(ext.KindFileDecorator); len(got) != 1 || got[0].Manifest.ID != b.id {
		t.Fatalf("bundle not indexed under FileDecorator: %+v", got)
	}
	// The entry answers HasKind for both its primary and contributed kinds.
	e, _ := r.Get(b.id)
	if !e.HasKind(ext.KindFeatureBundle) || !e.HasKind(ext.KindFileDecorator) {
		t.Fatalf("HasKind wrong: %+v", e.extraKinds)
	}

	// DecorateFields routes the group to it.
	fr := DecorateFields(context.Background(), r, []string{"_labels"},
		[]ext.ObjRef{{Anchor: "/x/a"}}, 0)
	if v, ok := fr.Fields["_labels"]["/x/a"]; !ok {
		t.Fatalf("decorator not invoked: %+v", fr)
	} else if _, ok := v.([]string); !ok {
		t.Fatalf("unexpected value %T", v)
	}

	// Contributions surface the FileColumn + MenuAction.
	doc := AggregateContributions(r)
	if len(doc.Extensions) != 1 {
		t.Fatalf("want 1 extension, got %d", len(doc.Extensions))
	}
	c := doc.Extensions[0].Contributions
	if len(c.FileColumns) != 1 || c.FileColumns[0].FieldGroup != "_labels" {
		t.Fatalf("FileColumn missing: %+v", c.FileColumns)
	}
	if len(c.MenuActions) != 1 || c.MenuActions[0].ID != "labels" {
		t.Fatalf("MenuAction missing: %+v", c.MenuActions)
	}
}

// Collapse must remove the FileDecorator leg so a disabled bundle stops
// decorating.
func TestCollapseFeatureBundleDropsDecorator(t *testing.T) {
	r := NewRegistry()
	b := &decoratingBundle{id: "builtin.labels2"}
	if err := RegisterBuiltinFeatureBundle(r, b); err != nil {
		t.Fatal(err)
	}
	CollapseFeatureBundle(b.id)
	fr := DecorateFields(context.Background(), r, []string{"_labels"},
		[]ext.ObjRef{{Anchor: "/x/a"}}, 0)
	if len(fr.Fields) != 0 {
		t.Fatalf("decorator still active after collapse: %+v", fr)
	}
}

func TestRegisterBuiltinFeatureBundleEmptyID(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltinFeatureBundle(r, &decoratingBundle{id: ""}); err == nil {
		t.Fatal("empty manifest id must be rejected")
	}
}
