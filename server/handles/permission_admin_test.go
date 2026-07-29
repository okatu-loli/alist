package handles

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/gin-gonic/gin"
)

// makeHandlerOperator builds a NON-admin storage-manager operator. The
// handler-layer control-plane gates are defense-in-depth: even if a route were
// mis-mapped, a non-admin must not write the authorization graph (spec §5).
// createNonReservedRole creates a role whose auto-incremented ID is guaranteed
// to be greater than model.ADMIN. Role IDs double as the identifiers
// IsAdmin/IsGuest match on (GUEST=1, ADMIN=2), and the in-memory test DB never
// seeds those built-in roles, so an early role could otherwise land on ID 2 and
// make a non-admin operator read as admin — masking the very denials these
// tests assert. Auto-increment never reuses IDs, so this settles in <=2 tries.
func createNonReservedRole(t *testing.T, name string) *model.Role {
	t.Helper()
	for i := 0; ; i++ {
		r := &model.Role{Name: fmt.Sprintf("%s-%d", name, i)}
		if err := op.CreateRole(r); err != nil {
			t.Fatal(err)
		}
		if r.ID > model.ADMIN {
			t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
			return r
		}
		_ = op.DeleteRole(r.ID)
	}
}

func makeHandlerOperator(t *testing.T) *model.User {
	t.Helper()
	r := createNonReservedRole(t, "h-op-role")
	p := &model.Permission{
		Name:         "h-op-storage",
		ResourceType: model.ResourceSystem,
		Path:         "storage",
		Effect:       model.EffectAllow,
		Actions: model.Actions{
			model.SystemAction("storage", "view"),
			model.SystemAction("storage", "manage"),
		},
	}
	if err := db.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })
	if err := db.AddRolePermission(r.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	op.InvalidateRolePolicyForTest(r.ID)
	return &model.User{ID: 6666, Username: "h-op", Role: model.Roles{int(r.ID)}}
}

func handlerAdmin() *model.User {
	return &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}
}

func postJSON(u *model.User, h func(*gin.Context), rawURL, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", rawURL, bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

// TestOperatorCannotCreateSystemPermission: a non-admin creating a system-type
// permission row must be rejected by the handler itself (spec §5.1).
func TestOperatorCannotCreateSystemPermission(t *testing.T) {
	u := makeHandlerOperator(t)
	w := postJSON(u, CreatePermission, "/api/admin/permission/create",
		`{"name":"escalate","resource_type":"system","path":"user","effect":"allow","actions":["system.user.manage"]}`)
	if code := respCode(t, w); code != 403 {
		t.Fatalf("non-admin creating a system permission must be 403, got %d: %s", code, w.Body.String())
	}
}

// TestOperatorCannotUpdateSystemPermission: a non-admin mutating an existing
// system-type row must be rejected, and the row must be left untouched.
func TestOperatorCannotUpdateSystemPermission(t *testing.T) {
	u := makeHandlerOperator(t)
	p := &model.Permission{
		Name: "sys-target", ResourceType: model.ResourceSystem, Path: "user",
		Effect: model.EffectAllow, Actions: model.Actions{model.SystemAction("user", "view")},
	}
	if err := db.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })

	w := postJSON(u, UpdatePermission, "/api/admin/permission/update",
		fmt.Sprintf(`{"id":%d,"effect":"deny"}`, p.ID))
	if code := respCode(t, w); code != 403 {
		t.Fatalf("non-admin updating a system permission must be 403, got %d: %s", code, w.Body.String())
	}
	got, err := op.GetPermission(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effect != model.EffectAllow {
		t.Fatalf("denied update must leave the row untouched, effect = %q", got.Effect)
	}
}

// TestOperatorCannotDeleteSystemPermission: a non-admin deleting a system-type
// row must be rejected, and the row must survive.
func TestOperatorCannotDeleteSystemPermission(t *testing.T) {
	u := makeHandlerOperator(t)
	p := &model.Permission{
		Name: "sys-del-target", ResourceType: model.ResourceSystem, Path: "role",
		Effect: model.EffectAllow, Actions: model.Actions{model.SystemAction("role", "view")},
	}
	if err := db.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })

	w := postJSON(u, DeletePermission, fmt.Sprintf("/api/admin/permission/delete?id=%d", p.ID), "")
	if code := respCode(t, w); code != 403 {
		t.Fatalf("non-admin deleting a system permission must be 403, got %d: %s", code, w.Body.String())
	}
	if _, err := op.GetPermission(p.ID); err != nil {
		t.Fatalf("denied delete must leave the row intact: %v", err)
	}
}

// TestAdminCanCreateSystemPermission: the control-plane path is unchanged for a
// real admin — the handler gate never fires for IsAdmin (regression).
func TestAdminCanCreateSystemPermission(t *testing.T) {
	w := postJSON(handlerAdmin(), CreatePermission, "/api/admin/permission/create",
		`{"name":"admin-sys","resource_type":"system","path":"storage","effect":"allow","actions":["system.storage.view"]}`)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("admin creating a system permission must be 200, got %d: %s", code, w.Body.String())
	}
	// clean up whatever row the admin path created
	ps, _, err := op.GetPermissions(1, 1000)
	if err == nil {
		for _, p := range ps {
			if p.Name == "admin-sys" {
				_ = db.DeletePermissionById(p.ID)
			}
		}
	}
}

// TestOperatorCannotChangeUserRole: a non-admin changing a user's role binding
// via UpdateUser must be rejected (spec §5.3, decision A: user-manager does
// user lifecycle but never the role binding), and the binding must be intact.
func TestOperatorCannotChangeUserRole(t *testing.T) {
	u := makeHandlerOperator(t)
	role := createNonReservedRole(t, "victim-role")
	target := &model.User{Username: "victim-user", Role: model.Roles{int(role.ID)}}
	target.SetPassword("x")
	if err := op.CreateUser(target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(target.ID) })

	w := postJSON(u, UpdateUser, "/api/admin/user/update",
		fmt.Sprintf(`{"id":%d,"username":"victim-user","role":[]}`, target.ID))
	if code := respCode(t, w); code != 403 {
		t.Fatalf("non-admin changing a user's role binding must be 403, got %d: %s", code, w.Body.String())
	}
	got, err := op.GetUserById(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !utils.SliceEqual(got.Role, model.Roles{int(role.ID)}) {
		t.Fatalf("denied role change must leave the binding intact, got %v", got.Role)
	}
}
