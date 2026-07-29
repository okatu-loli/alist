package handles

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// getJSON is the GET counterpart of postJSON (authz_matrix_test.go): builds a
// test gin.Context for a query-string request with the given user attached.
func getJSON(u *model.User, h func(*gin.Context), rawURL string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", rawURL, nil)
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

// TestRolePermissionsReturnsBoundPermission: a role with a manually bound
// permission must have that permission (with its origin) show up in the
// forward "role -> permissions" endpoint, distinct from the reverse
// PermissionRoles lookup that already existed.
func TestRolePermissionsReturnsBoundPermission(t *testing.T) {
	r := createNonReservedRole(t, "rp-role")
	p := &model.Permission{
		Name: "rp-manual", ResourceType: model.ResourceSystem, Path: "storage",
		Effect: model.EffectAllow, Actions: model.Actions{model.SystemAction("storage", "view")},
	}
	if err := db.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })
	if err := db.AddRolePermission(r.ID, p.ID); err != nil {
		t.Fatal(err)
	}

	w := getJSON(handlerAdmin(), RolePermissions, fmt.Sprintf("/api/admin/role/permissions?id=%d", r.ID))
	if code := respCode(t, w); code != 200 {
		t.Fatalf("want 200 got %d body=%s", code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"name":"rp-manual"`) {
		t.Fatalf("response must contain the bound permission: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"origin":"manual"`) {
		t.Fatalf("response must carry origin so the frontend can distinguish scope vs manual: %s", w.Body.String())
	}
}

// TestRolePermissionsBadID: a missing/invalid id must 400 before ever
// touching the DB.
func TestRolePermissionsBadID(t *testing.T) {
	w := getJSON(handlerAdmin(), RolePermissions, "/api/admin/role/permissions")
	if code := respCode(t, w); code != 400 {
		t.Fatalf("missing id must be 400, got %d: %s", code, w.Body.String())
	}
	w = getJSON(handlerAdmin(), RolePermissions, "/api/admin/role/permissions?id=nope")
	if code := respCode(t, w); code != 400 {
		t.Fatalf("non-numeric id must be 400, got %d: %s", code, w.Body.String())
	}
}

// TestRolePermissionsGated: the route is RequireSystem("role","view"), so a
// non-admin without that permission must be denied. Driven through the real
// middleware (not just the handler) since that's what actually enforces the
// gate in the router (spec-style regression, mirrors authz_system_test.go).
func TestRolePermissionsGated(t *testing.T) {
	u := makeHandlerOperator(t) // storage-manager only, no role.view
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/admin/role/permissions?id=1", nil)
	c.Set(ctxkey.User, u)
	middlewares.RequireSystem("role", "view")(c)
	if !c.IsAborted() {
		t.Fatal("operator without role.view must be denied by RequireSystem before reaching the handler")
	}

	// Control: an operator granted role.view passes the gate.
	r := createNonReservedRole(t, "rp-viewer-role")
	viewPerm := &model.Permission{
		Name: "rp-role-view", ResourceType: model.ResourceSystem, Path: "role",
		Effect: model.EffectAllow, Actions: model.Actions{model.SystemAction("role", "view")},
	}
	if err := db.CreatePermission(viewPerm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(viewPerm.ID) })
	if err := db.AddRolePermission(r.ID, viewPerm.ID); err != nil {
		t.Fatal(err)
	}
	op.InvalidateRolePolicyForTest(r.ID)
	viewer := &model.User{ID: 7777, Username: "rp-viewer", Role: model.Roles{int(r.ID)}}

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest("GET", fmt.Sprintf("/api/admin/role/permissions?id=%d", r.ID), nil)
	c2.Set(ctxkey.User, viewer)
	middlewares.RequireSystem("role", "view")(c2)
	if c2.IsAborted() {
		t.Fatal("operator with role.view must pass the gate")
	}
}
