package op_test

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestSystemAuthzTypeIsolation is the security core of the typed-dispatch
// engine: a permission row is matched only within its own resource type. An
// fs row that happens to carry a system action must NEVER satisfy a system
// decision (that would be a global privilege escalation), and a system row is
// matched by exact domain, not by fs path containment.
func TestSystemAuthzTypeIsolation(t *testing.T) {
	ctx := context.Background()
	u := makeAuthzUser(t, "sysiso", []*model.Permission{
		// P1: an fs row (recursive allow on "/") that also carries a system
		// action. Under fs matching it covers every path; it must not leak
		// into system decisions.
		{Name: "fs-carry", ResourceType: model.ResourceFS, Path: "/", Recursive: true,
			Effect: model.EffectAllow, Actions: model.Actions{model.ActRead, "system.storage.manage"}},
		// P2: a legitimate system row for the "storage" domain.
		{Name: "sys-storage-view", ResourceType: model.ResourceSystem, Path: "storage",
			Effect: model.EffectAllow, Actions: model.Actions{"system.storage.view"}},
	})

	// (1) fs 行携带 system 动作，绝不能泄漏进 system 判定：
	if err := op.Authorize(ctx, u, "system.storage.manage",
		op.AuthzRequest{ResourceType: model.ResourceSystem, Path: "storage"}); err == nil {
		t.Fatal("fs-typed row leaked into system decision — privilege escalation")
	}
	// (2) 正规 system 行按精确域匹配放行：
	if err := op.Authorize(ctx, u, "system.storage.view",
		op.AuthzRequest{ResourceType: model.ResourceSystem, Path: "storage"}); err != nil {
		t.Fatalf("system view should be allowed: %v", err)
	}
	// (3) 未在白名单的域 fast-fail：
	if err := op.Authorize(ctx, u, "system.bogus.view",
		op.AuthzRequest{ResourceType: model.ResourceSystem, Path: "bogus"}); err == nil {
		t.Fatal("unknown domain must be denied")
	}
	// (4) fs 判定完全不受影响（回归）：
	if err := op.Authorize(ctx, u, model.ActRead,
		op.AuthzRequest{ResourceType: model.ResourceFS, Path: "/media"}); err != nil {
		t.Fatalf("fs read regression: %v", err)
	}
}
