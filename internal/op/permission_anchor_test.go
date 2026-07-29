package op_test

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestSystemRowAnchorSuppressed verifies that a system-type permission row is
// never anchored to a node and never path-cleaned (spec §2.4). A storage is
// mounted at /storage so that, without suppression, the fs anchor resolver
// would anchor this row to a node — precisely the leak this guards against so
// fs matching cannot leak into a system-domain decision.
func TestSystemRowAnchorSuppressed(t *testing.T) {
	sid, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: "/storage", Driver: "Local",
		Addition: `{"root_folder_path":"` + t.TempDir() + `"}`,
	})
	if err != nil {
		t.Fatalf("mount storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	p := &model.Permission{ResourceType: model.ResourceSystem, Path: "storage",
		Actions: model.Actions{model.SystemAction("storage", "view")}, Effect: model.EffectAllow}
	if err := op.CreatePermission(p); err != nil {
		t.Fatalf("create system permission: %v", err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(p.ID) })

	if p.NodeID != 0 {
		t.Fatalf("system row must not be node-anchored, got node_id=%d", p.NodeID)
	}
	if p.Path != "storage" {
		t.Fatalf("system path must not be FixAndCleanPath'd, got %q", p.Path)
	}
}
