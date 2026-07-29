package op

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// boundScopeIDs returns the ids of the role's bound scope-origin rows.
func boundScopeIDs(t *testing.T, roleID uint) []uint {
	t.Helper()
	bound, err := db.GetPermissionsByRoleIDs([]uint{roleID})
	if err != nil {
		t.Fatalf("load bindings: %v", err)
	}
	var ids []uint
	for _, p := range bound {
		if p.Origin == model.OriginScope {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// TestSetRolePermissionsPreservesScopeBindings is the regression test for the
// admin-filesystem-lockout: SetRolePermissions used to replace ALL bindings
// with the caller's set, so a caller that didn't round-trip scope ids orphaned
// the role's scope rows (its whole fs grant). The server must preserve
// scope-origin bindings no matter what the caller sends.
func TestSetRolePermissionsPreservesScopeBindings(t *testing.T) {
	role := &model.Role{Name: "scope-preserve-test", PermissionScopes: []model.PermissionEntry{
		{Path: "/", Permission: 0xFFFF},
	}}
	if err := CreateRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _ = DeleteRole(role.ID) })

	scopeBefore := boundScopeIDs(t, role.ID)
	if len(scopeBefore) != 1 {
		t.Fatalf("expected 1 scope binding after create, got %d", len(scopeBefore))
	}

	manual := &model.Permission{
		Name: "scope-preserve-manual", Path: "/sub",
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow,
	}
	if err := CreatePermission(manual); err != nil {
		t.Fatalf("create manual perm: %v", err)
	}
	t.Cleanup(func() { _ = DeletePermission(manual.ID) })

	// The hostile/buggy caller: manual ids only, scope ids omitted.
	if err := SetRolePermissions(role.ID, []uint{manual.ID}); err != nil {
		t.Fatalf("set role permissions: %v", err)
	}

	scopeAfter := boundScopeIDs(t, role.ID)
	if len(scopeAfter) != 1 || scopeAfter[0] != scopeBefore[0] {
		t.Fatalf("scope binding lost: before=%v after=%v", scopeBefore, scopeAfter)
	}
	bound, _ := db.GetPermissionsByRoleIDs([]uint{role.ID})
	if len(bound) != 2 {
		t.Fatalf("expected scope+manual bound, got %d rows", len(bound))
	}
}

// TestSyncRoleScopePermissionsHealsOrphans simulates the damaged-database
// state (scope row exists but its binding was wiped) and verifies the boot
// self-heal path restores a working scope grant.
func TestSyncRoleScopePermissionsHealsOrphans(t *testing.T) {
	role := &model.Role{Name: "scope-heal-test", PermissionScopes: []model.PermissionEntry{
		{Path: "/", Permission: 0xFFFF},
	}}
	if err := CreateRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _ = DeleteRole(role.ID) })

	// Damage: wipe every binding directly (bypassing the now-guarded op API),
	// leaving the scope row orphaned.
	if err := db.SetRolePermissions(role.ID, nil); err != nil {
		t.Fatalf("simulate wipe: %v", err)
	}
	if got := boundScopeIDs(t, role.ID); len(got) != 0 {
		t.Fatalf("setup: expected no scope bindings, got %v", got)
	}

	if err := SyncRoleScopePermissions(role.ID); err != nil {
		t.Fatalf("heal: %v", err)
	}
	healed := boundScopeIDs(t, role.ID)
	if len(healed) != 1 {
		t.Fatalf("expected 1 scope binding after heal, got %d", len(healed))
	}
	// The orphaned row must not survive as a duplicate.
	var count int64
	if err := db.CountPermissionsByNamePrefix("role:scope-heal-test:scope:", &count); err == nil && count != 1 {
		t.Fatalf("expected exactly 1 scope row after heal, got %d", count)
	}
}
