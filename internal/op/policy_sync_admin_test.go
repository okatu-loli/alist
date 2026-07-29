package op

import (
	"fmt"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// seedAdminRoleForSyncTest ensures the real "admin" role (sentinel ID
// model.ADMIN) exists, without depending on op_test's seedSentinelRoles
// (different package, and test execution order between the two isn't
// guaranteed). Idempotent: safe to call from multiple tests.
func seedAdminRoleForSyncTest(t *testing.T) *model.Role {
	t.Helper()
	r, err := db.GetRoleByName("admin")
	if err != nil {
		r = &model.Role{ID: uint(model.ADMIN), Name: "admin", PermissionScopes: []model.PermissionEntry{
			{Path: "/", Permission: 0xFFFF},
		}}
		if err := CreateRole(r); err != nil {
			t.Fatalf("seed admin role: %v", err)
		}
	}
	return r
}

// TestEnforceAdminRoleDefaultsSyncsScopePermissions is a regression test for
// enforceAdminRoleDefaults rewriting a non-canonical admin scope. Without the
// fix, the roles row is canonicalized but the normalized permissions table
// keeps serving the stale scope forever.
func TestEnforceAdminRoleDefaultsSyncsScopePermissions(t *testing.T) {
	admin := seedAdminRoleForSyncTest(t)

	t.Cleanup(func() {
		canonical, err := db.GetRole(admin.ID)
		if err != nil {
			t.Fatalf("cleanup: get admin role: %v", err)
		}
		canonical.PermissionScopes = []model.PermissionEntry{{Path: "/", Permission: 0xFFFF}}
		if err := db.UpdateRoleWithScopePermissions(canonical, scopePermissionRows(canonical)); err != nil {
			t.Fatalf("cleanup: restore admin role: %v", err)
		}
		roleCache.Del(fmt.Sprint(canonical.ID))
		roleCache.Del(canonical.Name)
	})

	// Simulate legacy/non-canonical state: a scope that predates the
	// canonical-admin-scope invariant, written straight through db (mirrors
	// data that was never touched by op's dual-write path).
	admin.PermissionScopes = []model.PermissionEntry{{Path: "/only-a", Permission: 1 << 3}}
	if err := db.UpdateRoleWithScopePermissions(admin, scopePermissionRows(admin)); err != nil {
		t.Fatalf("seed non-canonical admin scope: %v", err)
	}

	// Force a cache miss so GetRoleByName re-runs enforceAdminRoleDefaults.
	roleCache.Del(fmt.Sprint(admin.ID))
	roleCache.Del(admin.Name)

	if _, err := GetRoleByName("admin"); err != nil {
		t.Fatalf("GetRoleByName(admin): %v", err)
	}

	ps, err := db.GetPermissionsByRoleIDs([]uint{admin.ID})
	if err != nil {
		t.Fatalf("GetPermissionsByRoleIDs: %v", err)
	}
	if len(ps) != 1 || ps[0].Path != "/" {
		t.Fatalf("stale scope permissions after admin canonicalization: %+v", ps)
	}
	if !ps[0].Actions.Contains(model.ActWrite) || !ps[0].Actions.Contains(model.ActRead) {
		t.Errorf("expected canonical actions, got %v", ps[0].Actions)
	}
	if ps[0].Origin != model.OriginScope {
		t.Errorf("expected scope origin, got %v", ps[0].Origin)
	}
}
