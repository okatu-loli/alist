package op_test

import (
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// seedSentinelRoles seeds the guest/admin roles at their fixed legacy
// sentinel IDs (mirrors internal/bootstrap/data/role.go), same as a real
// deployment. This package's shared in-memory sqlite DB otherwise never runs
// that seed step, so a freshly created test role can land on ID 2 by plain
// autoincrement and get misidentified as "admin" by User.IsAdmin() (which
// matches by legacy ID, not name) — a pre-existing quirk unrelated to this
// file's dual-write tests. Called lazily (not from init()) because
// storage_test.go's init() — which calls db.Init() — is not guaranteed to
// run before this file's init() in file-order.
var seedSentinelRolesOnce sync.Once

func seedSentinelRoles() {
	seedSentinelRolesOnce.Do(func() {
		if _, err := op.GetRoleByName("guest"); err != nil {
			_ = op.CreateRole(&model.Role{ID: uint(model.GUEST), Name: "guest"})
		}
		if _, err := op.GetRoleByName("admin"); err != nil {
			_ = op.CreateRole(&model.Role{ID: uint(model.ADMIN), Name: "admin"})
		}
	})
}

func TestRoleCRUDSyncsScopePermissions(t *testing.T) {
	seedSentinelRoles()
	r := &model.Role{Name: "sync-test", PermissionScopes: []model.PermissionEntry{
		{Path: "/data", Permission: 1 << 3},
	}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	ps, err := db.GetPermissionsByRoleIDs([]uint{r.ID})
	if err != nil || len(ps) != 1 {
		t.Fatalf("create did not sync: %v %v", ps, err)
	}
	if !ps[0].Actions.Contains(model.ActWrite) || !ps[0].Actions.Contains(model.ActRead) {
		t.Errorf("actions: %v", ps[0].Actions)
	}
	// update 换 scope → 重生成
	r.PermissionScopes = []model.PermissionEntry{{Path: "/other", Permission: 1 << 7}}
	if err := op.UpdateRole(r); err != nil {
		t.Fatal(err)
	}
	ps, _ = db.GetPermissionsByRoleIDs([]uint{r.ID})
	if len(ps) != 1 || ps[0].Path != "/other" || !ps[0].Actions.Contains(model.ActRemove) {
		t.Fatalf("update did not resync: %+v", ps)
	}
	// delete → 清空
	rid := r.ID
	if err := op.DeleteRole(rid); err != nil {
		t.Fatal(err)
	}
	if ps, _ := db.GetPermissionsByRoleIDs([]uint{rid}); len(ps) != 0 {
		t.Fatalf("delete left rows: %v", ps)
	}
}

// TestRoleCreateAtomicWithScopeSync guards the role-row / scope-permission
// atomicity invariant across a failed create. Note on how this was derived:
// with op.CreateRole's write order (role row via
// db.CreateRoleWithScopePermissions, which runs the role insert and the
// scope-permission rows in one transaction), a duplicate name fails at the
// role insert itself — before any permission row is written — so this
// scenario was never actually reachable as a leak, pre- or post-refactor
// (verified empirically against the pre-refactor code, which called
// db.CreateRole then syncScopePermissions as two separate steps: the first
// step's unique-constraint failure already short-circuited before the
// second ran). No sequence was found that forces syncScopePermissions itself
// to fail after the role row commits without invasive fault injection
// (CreatePermission/AddRolePermission have no realistic failure mode from
// caller-supplied data alone). This test therefore serves as a regression
// anchor for the invariant rather than a reproduction of a prior bug; the
// refactor's actual value is the structural guarantee (single transaction)
// that makes the invariant hold by construction instead of by accident of
// call order.
func TestRoleCreateAtomicWithScopeSync(t *testing.T) {
	seedSentinelRoles()
	r1 := &model.Role{Name: "atomic-role", PermissionScopes: []model.PermissionEntry{{Path: "/a1", Permission: 1 << 3}}}
	if err := op.CreateRole(r1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r1.ID) })
	before, _, err := db.GetPermissions(1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	dup := &model.Role{Name: "atomic-role", PermissionScopes: []model.PermissionEntry{{Path: "/a2", Permission: 1 << 3}}}
	if err := op.CreateRole(dup); err == nil {
		t.Fatal("duplicate role name must fail")
	}
	after, _, err := db.GetPermissions(1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed create leaked permission rows: %d -> %d", len(before), len(after))
	}
}

func TestUserCRUDSyncsUserRoles(t *testing.T) {
	seedSentinelRoles()
	r := &model.Role{Name: "ur-role", PermissionScopes: []model.PermissionEntry{{Path: "/", Permission: 0}}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	u := &model.User{Username: "ur-user", Role: model.Roles{int(r.ID)}}
	u.SetPassword("x")
	if err := op.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(u.ID) })
	ids, err := db.GetUserRoleIDs(u.ID)
	if err != nil || len(ids) != 1 || ids[0] != r.ID {
		t.Fatalf("user_roles after create: %v %v", ids, err)
	}
	uid := u.ID
	if err := op.DeleteUserById(uid); err != nil {
		t.Fatal(err)
	}
	if ids, _ := db.GetUserRoleIDs(uid); len(ids) != 0 {
		t.Fatalf("user_roles after delete: %v", ids)
	}
}
