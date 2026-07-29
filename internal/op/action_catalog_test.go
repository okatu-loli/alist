package op_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestCatalogDedupAndSource covers keyed de-duplication (last write wins) and
// namespace ownership: "core" owns bare fs verbs + system.*, a plugin source
// may only register its own plugin:<id>:* prefix (spec §3.1 F2a/F5).
func TestCatalogDedupAndSource(t *testing.T) {
	const src = "plugin:acme"
	t.Cleanup(func() { op.UnregisterBySource(src) })

	// Register once, then again with the same Name but a different payload:
	// the map is keyed by Name so the second registration must overwrite the
	// first, leaving exactly one entry with the last-written value + source.
	if err := op.RegisterActions(src, op.ActionDef{
		Name: "plugin:acme:widget.view", ResourceType: model.ResourceSystem, I18nKey: "old"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := op.RegisterActions(src, op.ActionDef{
		Name: "plugin:acme:widget.view", ResourceType: model.ResourceSystem, I18nKey: "new"}); err != nil {
		t.Fatalf("second register: %v", err)
	}
	snap, err := op.ActionCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	count := 0
	for _, a := range snap.Actions {
		if a.Name == "plugin:acme:widget.view" {
			count++
			if a.I18nKey != "new" {
				t.Fatalf("last-wins dedup: i18n=%q, want new", a.I18nKey)
			}
			if a.Source != src {
				t.Fatalf("source must be stamped: got %q, want %q", a.Source, src)
			}
		}
	}
	if count != 1 {
		t.Fatalf("dedup: want exactly 1 entry, got %d", count)
	}

	// Namespace ownership violations must be rejected, not silently accepted.
	if err := op.RegisterActions(src, op.ActionDef{
		Name: model.SystemAction("storage", "manage"), ResourceType: model.ResourceSystem}); err == nil {
		t.Fatal("plugin registering a core system.* action must error")
	}
	if err := op.RegisterActions(src, op.ActionDef{
		Name: "plugin:other:widget.view", ResourceType: model.ResourceSystem}); err == nil {
		t.Fatal("plugin registering another plugin's prefix must error")
	}
	if err := op.RegisterActions("core", op.ActionDef{
		Name: "plugin:acme:nope", ResourceType: model.ResourceSystem}); err == nil {
		t.Fatal("core registering a plugin:* action must error")
	}
	if err := op.RegisterActions("bogus-source", op.ActionDef{
		Name: model.ActRead, ResourceType: model.ResourceFS}); err == nil {
		t.Fatal("an unrecognized source must error")
	}
}

// TestValidateActionsRejectsUnknown covers create/update-time validation: every
// action must be registered AND belong to the row's resource type; a system row
// must not carry a bare fs verb, and unknown actions/types are rejected (§2.3).
func TestValidateActionsRejectsUnknown(t *testing.T) {
	if err := op.ValidateActions(model.ResourceSystem, model.Actions{model.ActRead}); err == nil {
		t.Fatal("system row carrying a bare fs verb must be rejected")
	}
	if err := op.ValidateActions(model.ResourceFS, model.Actions{"totally.made.up"}); err == nil {
		t.Fatal("unregistered action must be rejected")
	}
	if err := op.ValidateActions(model.ResourceFS, model.Actions{model.SystemAction("storage", "view")}); err == nil {
		t.Fatal("fs row carrying a system action must be rejected")
	}
	if err := op.ValidateActions("bogus", model.Actions{model.ActRead}); err == nil {
		t.Fatal("unknown resource_type must be rejected")
	}
	// Valid combinations must pass. "" defaults to fs.
	if err := op.ValidateActions("", model.Actions{model.ActRead, model.ActWrite}); err != nil {
		t.Fatalf("valid fs actions must pass: %v", err)
	}
	if err := op.ValidateActions(model.ResourceSystem, model.Actions{
		model.SystemAction("storage", "view"), model.SystemAction("storage", "manage")}); err != nil {
		t.Fatalf("valid system actions must pass: %v", err)
	}
}

// TestOrphanActionsSurface: an action that appears in a stored permission row
// but is registered by nobody (e.g. an uninstalled plugin) must be surfaced as
// orphaned by the catalog — auditable, never silently dropped (§3.1 F2d/F6a).
func TestOrphanActionsSurface(t *testing.T) {
	orphan := model.Action("plugin:ghost:widget.manage")
	// db.CreatePermission bypasses op-level validation, simulating a row left
	// behind by an uninstalled plugin.
	p := &model.Permission{Name: "orphan-row", Path: "/orphan-test",
		Actions: model.Actions{orphan}, Effect: model.EffectAllow, Recursive: true}
	if err := db.CreatePermission(p); err != nil {
		t.Fatalf("seed orphan row: %v", err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })

	snap, err := op.ActionCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	found := false
	for _, a := range snap.Actions {
		if a.Name == orphan {
			found = true
			if !a.Orphaned {
				t.Fatal("stored-but-unregistered action must be flagged orphaned")
			}
		}
	}
	if !found {
		t.Fatal("orphaned action must appear in the catalog snapshot")
	}
}
