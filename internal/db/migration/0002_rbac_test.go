package migration

import (
	"encoding/json"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

// 复用 drift_guard_test.go 的 openMem/testPrefix。
func TestRBACMigrationConvertsScopesAndUserRoles(t *testing.T) {
	d := openMem(t, "rbac_mig")
	// Stop at 0008: rbacBackfill writes the anchor triple (storage_id/anchor),
	// which 0009 drops once identity moves to the nodes registry. 0008 is the
	// last stage where the triple still exists, so the replay below can run and
	// be read back through this migration's frozen `permission` shape.
	if err := New(d, testPrefix).UpTo("0008_permission_display_name"); err != nil {
		t.Fatalf("up: %v", err)
	}
	// 用运行时模型种数据(Up 已建全 schema),再手动重放 0002 的转换逻辑:
	// 先清掉自动迁移可能产生的行,构造"老库"状态 —— role 带 RawPermission、无绑定
	role := &model.Role{Name: "staff", PermissionScopes: []model.PermissionEntry{
		{Path: "/data", Permission: 1<<3 | 1<<5}, // write+move
		{Path: "/pub", Permission: 0},
	}}
	if err := d.Create(role).Error; err != nil {
		t.Fatal(err)
	}
	user := &model.User{Username: "u1", Role: model.Roles{int(role.ID)}}
	if err := d.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	if err := rbacBackfill(d); err != nil { // 迁移的核心函数,导出为包内函数以便测试
		t.Fatal(err)
	}
	// Read through this migration's frozen `permission` shape (has the anchor
	// triple, no node_id): the runtime model.Permission is the post-0009 shape
	// and would not match the 0008 schema this test stops at.
	var perms []permission
	if err := d.Table(testPrefix + "permissions").Find(&perms).Error; err != nil {
		t.Fatal(err)
	}
	if len(perms) != 2 {
		t.Fatalf("want 2 converted permissions, got %d", len(perms))
	}
	for _, p := range perms {
		if p.Effect != model.EffectAllow || !p.Recursive || p.Origin != model.OriginScope {
			t.Errorf("conversion defaults wrong: %+v", p)
		}
		var acts model.Actions
		if err := json.Unmarshal([]byte(p.Actions), &acts); err != nil {
			t.Fatalf("actions json %q: %v", p.Actions, err)
		}
		if !acts.Contains(model.ActRead) {
			t.Errorf("implicit read missing: %+v", p)
		}
	}
	var bindCount int64
	d.Table(testPrefix+"role_permissions").Where("role_id = ?", role.ID).Count(&bindCount)
	if bindCount != 2 {
		t.Errorf("bindings: %d", bindCount)
	}
	var urCount int64
	d.Table(testPrefix+"user_roles").Where("user_id = ? AND role_id = ?", user.ID, role.ID).Count(&urCount)
	if urCount != 1 {
		t.Errorf("user_roles: %d", urCount)
	}
	// 幂等:重跑不翻倍
	if err := rbacBackfill(d); err != nil {
		t.Fatal(err)
	}
	d.Table(testPrefix+"role_permissions").Where("role_id = ?", role.ID).Count(&bindCount)
	if bindCount != 2 {
		t.Errorf("backfill not idempotent: %d", bindCount)
	}
}
