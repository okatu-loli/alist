package db

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupRBACTestDB(t *testing.T) {
	t.Helper()
	dB, err := gorm.Open(sqlite.Open("file:rbac_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Permission{}, &model.RolePermission{}, &model.UserRole{}); err != nil {
		t.Fatal(err)
	}
	old := db
	db = dB
	t.Cleanup(func() { db = old })
}

func TestPermissionCRUDAndBindings(t *testing.T) {
	setupRBACTestDB(t)
	p := &model.Permission{Name: "read-data", Path: "/data",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	if err := CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 {
		t.Fatal("no id assigned")
	}
	if err := AddRolePermission(7, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := GetPermissionsByRoleIDs([]uint{7})
	if err != nil || len(got) != 1 || got[0].Path != "/data" || !got[0].Actions.Contains(model.ActRead) {
		t.Fatalf("by role: %v %v", got, err)
	}
	rids, err := GetRoleIDsByPermission(p.ID)
	if err != nil || len(rids) != 1 || rids[0] != 7 {
		t.Fatalf("reverse: %v %v", rids, err)
	}
	// 删 permission 连带删绑定
	if err := DeletePermissionById(p.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ := GetPermissionIDsByRole(7); len(ids) != 0 {
		t.Fatalf("binding not cascaded: %v", ids)
	}
}

func TestScopePermissionSync(t *testing.T) {
	setupRBACTestDB(t)
	scope := &model.Permission{Name: "role:x:scope:0", Path: "/a", Origin: model.OriginScope,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	manual := &model.Permission{Name: "hand", Path: "/b", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	for _, p := range []*model.Permission{scope, manual} {
		if err := CreatePermission(p); err != nil {
			t.Fatal(err)
		}
	}
	_ = AddRolePermission(3, scope.ID)
	_ = AddRolePermission(3, manual.ID)
	if err := DeleteScopePermissionsByRole(3); err != nil {
		t.Fatal(err)
	}
	left, _ := GetPermissionsByRoleIDs([]uint{3})
	if len(left) != 1 || left[0].Origin != model.OriginManual {
		t.Fatalf("manual permission must survive scope sync: %v", left)
	}
}

func TestUserRoles(t *testing.T) {
	setupRBACTestDB(t)
	if err := SetUserRoles(5, []uint{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := SetUserRoles(5, []uint{2, 9}); err != nil { // replace 语义
		t.Fatal(err)
	}
	ids, err := GetUserRoleIDs(5)
	if err != nil || len(ids) != 2 {
		t.Fatalf("user roles: %v %v", ids, err)
	}
	if err := DeleteUserRolesByUser(5); err != nil {
		t.Fatal(err)
	}
	if ids, _ := GetUserRoleIDs(5); len(ids) != 0 {
		t.Fatalf("delete failed: %v", ids)
	}
}
