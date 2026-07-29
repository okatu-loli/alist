package middlewares

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

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

// makeStorageOperator builds a NON-admin user whose only privileges are
// system.storage.{view,manage} — the storage-manager preset. Tests drive
// RequireSystem/RequireAdmin through the real op.Authorize engine (not the
// IsAdmin short-circuit), which is the only way §2/§5 gaps surface: an admin
// short-circuit masks every control-plane bug (spec §12 F8). The system rows
// are written straight through db.CreatePermission so they keep the empty
// anchor a system row must have (spec §2.4).
func makeStorageOperator(t *testing.T) *model.User {
	t.Helper()
	r := createNonReservedRole(t, "op-storage-role")
	p := &model.Permission{
		Name:         "op-storage",
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
	return &model.User{ID: 5555, Username: "op-storage", Role: model.Roles{int(r.ID)}}
}

func makeAdmin() *model.User {
	return &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}
}

// runGate runs a single gin middleware against a user set in the context and
// reports whether it aborted. alist denials write HTTP 200 with the real code
// in the body and call c.Abort(), so abort state — not w.Code — is what
// distinguishes allow from deny here.
func runGate(t *testing.T, h gin.HandlerFunc, u *model.User) *gin.Context {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x", nil)
	c.Set(ctxkey.User, u)
	h(c)
	return c
}

// TestOperatorCanStorageOnly: a storage-manager operator passes storage
// view/manage but is denied on any other domain — the core "can manage
// storage but not touch users" guarantee resolved through the engine.
func TestOperatorCanStorageOnly(t *testing.T) {
	u := makeStorageOperator(t)
	if c := runGate(t, RequireSystem("storage", "view"), u); c.IsAborted() {
		t.Fatal("operator with system.storage.view must pass storage view")
	}
	if c := runGate(t, RequireSystem("storage", "manage"), u); c.IsAborted() {
		t.Fatal("operator with system.storage.manage must pass storage manage")
	}
	if c := runGate(t, RequireSystem("user", "view"), u); !c.IsAborted() {
		t.Fatal("storage operator must be denied on the user domain")
	}
}

// TestOperatorCannotBindRole: role↔permission binding is IsAdmin-only (spec
// §5.2), so it uses RequireAdmin — a non-admin operator is denied.
func TestOperatorCannotBindRole(t *testing.T) {
	u := makeStorageOperator(t)
	if c := runGate(t, RequireAdmin, u); !c.IsAborted() {
		t.Fatal("role/set_permissions is IsAdmin-only: operator must be denied")
	}
}

// TestOperatorCannotMintToken: admin-token minting is IsAdmin-only (spec
// §3.2/§5.4) and rides RequireAdmin, not setting.manage.
func TestOperatorCannotMintToken(t *testing.T) {
	u := makeStorageOperator(t)
	if c := runGate(t, RequireAdmin, u); !c.IsAborted() {
		t.Fatal("token mint is IsAdmin-only: operator must be denied")
	}
}

// TestDriverGated: driver was previously ungated within the admin group; it
// must now require system.driver.view, so a non-admin without it is denied
// (spec §2.5/§8 — driver schema must not leak to non-admins).
func TestDriverGated(t *testing.T) {
	u := makeStorageOperator(t)
	if c := runGate(t, RequireSystem("driver", "view"), u); !c.IsAborted() {
		t.Fatal("driver must be gated: a non-admin without driver view is denied")
	}
}

// TestUnmappedAdminRouteFailsClosed: the /api/admin group carries AuthAdmin as
// a fail-closed backstop, so a route that forgets its RequireSystem mapping
// still denies a non-admin rather than fail-open.
func TestUnmappedAdminRouteFailsClosed(t *testing.T) {
	u := makeStorageOperator(t)
	r := gin.New()
	grp := r.Group("/admin",
		func(c *gin.Context) { c.Set(ctxkey.User, u); c.Next() },
		AuthAdmin)
	grp.GET("/unmapped", func(c *gin.Context) { c.String(200, "reached-handler") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/admin/unmapped", nil))
	if strings.Contains(w.Body.String(), "reached-handler") {
		t.Fatal("an unmapped admin route must fail closed for a non-admin, not reach the handler")
	}
}

// TestAdminShortCircuits: an admin bypasses RequireSystem and RequireAdmin on
// every domain (spec §5/§14) — the escape hatch that keeps the fail-closed
// backstop from locking real admins out.
func TestAdminShortCircuits(t *testing.T) {
	admin := makeAdmin()
	for _, dom := range []string{"storage", "user", "driver", "role", "signing_key"} {
		if c := runGate(t, RequireSystem(dom, "manage"), admin); c.IsAborted() {
			t.Fatalf("admin must short-circuit RequireSystem(%s,manage)", dom)
		}
	}
	if c := runGate(t, RequireAdmin, admin); c.IsAborted() {
		t.Fatal("admin must pass RequireAdmin")
	}
}
