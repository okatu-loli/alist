package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestInitRolesMarksBuiltinRolesProtected covers both paths that must set
// Protected=true on the built-in admin/guest roles:
//
//  1. create path — a fresh DB with no admin/guest rows yet.
//  2. self-heal path — rows that already exist but have Protected=false (e.g.
//     left over from a build that predates the Protected flag, or created by
//     the historical bug where initRoles() never set it).
//
// Without this, the delete/rename guard in server/handles/role.go DeleteRole
// (which gates on role.Protected) is a no-op for the built-in roles.
func TestInitRolesMarksBuiltinRolesProtected(t *testing.T) {
	builtins := []string{"admin", "guest"}

	// Create path: initRoles() is idempotent, so calling it here guarantees
	// the rows exist regardless of test run order within the package.
	initRoles()

	for _, name := range builtins {
		role, err := op.GetRoleByName(name)
		if err != nil {
			t.Fatalf("get role %q: %v", name, err)
		}
		if !role.Protected {
			t.Errorf("role %q Protected=false after initRoles create path, want true", name)
		}
	}

	// Self-heal path: force the flag back to false directly in the DB
	// (bypassing the op-layer, which would otherwise refuse to unprotect a
	// built-in role), then drop the op caches so the next read hits the DB.
	for _, name := range builtins {
		role, err := op.GetRoleByName(name)
		if err != nil {
			t.Fatalf("get role %q: %v", name, err)
		}
		role.Protected = false
		if err := db.UpdateRole(role); err != nil {
			t.Fatalf("force-unprotect role %q: %v", name, err)
		}
	}
	op.ResetCachesForTest()

	for _, name := range builtins {
		role, err := op.GetRoleByName(name)
		if err != nil {
			t.Fatalf("get role %q after forced unprotect: %v", name, err)
		}
		if role.Protected {
			t.Fatalf("setup: role %q still Protected=true after forced unprotect", name)
		}
	}

	initRoles()

	for _, name := range builtins {
		role, err := op.GetRoleByName(name)
		if err != nil {
			t.Fatalf("get role %q after self-heal: %v", name, err)
		}
		if !role.Protected {
			t.Errorf("role %q Protected=false after initRoles self-heal path, want true", name)
		}
	}
}
