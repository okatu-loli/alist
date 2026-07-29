package op_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

func TestPermissionOpCRUDInvalidatesPolicy(t *testing.T) {
	r := &model.Role{Name: "pcrud-role"}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	u := &model.User{ID: 6666, Username: "pcrud", Role: model.Roles{int(r.ID)}}

	p := &model.Permission{Name: "p1", Path: "/pc", Actions: model.Actions{model.ActRead},
		Effect: model.EffectAllow, Recursive: true}
	if err := op.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(p.ID) })
	if err := op.SetRolePermissions(r.ID, []uint{p.ID}); err != nil {
		t.Fatal(err)
	}
	if d, _ := op.ResolveEffectivePolicy(u, "/pc/x"); !d.Can(model.ActRead) {
		t.Fatal("binding must take effect without manual invalidation")
	}
	// update 翻转 effect → 决策跟着翻,不需要手动清缓存
	p.Effect = model.EffectDeny
	if err := op.UpdatePermission(p); err != nil {
		t.Fatal(err)
	}
	if d, _ := op.ResolveEffectivePolicy(u, "/pc/x"); d.Can(model.ActRead) {
		t.Fatal("op.UpdatePermission must invalidate policy cache")
	}
	// 反查
	rids, err := op.GetRoleIDsByPermission(p.ID)
	if err != nil || len(rids) != 1 || rids[0] != r.ID {
		t.Fatalf("reverse query: %v %v", rids, err)
	}
}

// The policy engine (policy.go) treats any Effect != "deny" as an allow, so
// a typo like "Deny"/"DENY" must be rejected at creation/update time instead
// of silently granting access.
func TestCreatePermissionRejectsInvalidEffect(t *testing.T) {
	p := &model.Permission{Name: "bad-effect", Path: "/pe", Actions: model.Actions{model.ActRead},
		Effect: "Deny"}
	if err := op.CreatePermission(p); err == nil {
		t.Fatal("CreatePermission with Effect \"Deny\" must error, got nil")
	}
}

func TestCreatePermissionAcceptsValidEffect(t *testing.T) {
	p := &model.Permission{Name: "good-effect", Path: "/pe2", Actions: model.Actions{model.ActRead},
		Effect: model.EffectDeny}
	if err := op.CreatePermission(p); err != nil {
		t.Fatalf("CreatePermission with Effect %q must succeed: %v", model.EffectDeny, err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(p.ID) })
}

func TestCreatePermissionDefaultsEmptyEffectToAllow(t *testing.T) {
	p := &model.Permission{Name: "default-effect", Path: "/pe3", Actions: model.Actions{model.ActRead}}
	if err := op.CreatePermission(p); err != nil {
		t.Fatalf("CreatePermission with empty Effect must default to allow: %v", err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(p.ID) })
	if p.Effect != model.EffectAllow {
		t.Errorf("Effect = %q, want %q", p.Effect, model.EffectAllow)
	}
}
