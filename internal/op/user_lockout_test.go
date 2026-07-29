package op_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// createEnabledAdmin inserts an enabled admin user carrying the ADMIN role
// binding directly (the in-mem test DB doesn't seed built-in roles, so we
// assign model.ADMIN explicitly, matching the T4/T5 test pattern).
func createEnabledAdmin(t *testing.T, username string) *model.User {
	t.Helper()
	u := &model.User{
		Username: username,
		Role:     model.Roles{int(model.ADMIN)},
		Disabled: false,
	}
	u.SetPassword("x")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create admin %s: %v", username, err)
	}
	// Tear down at the db layer, not op.DeleteUserById — the latter refuses to
	// remove a last enabled admin, so an op-level cleanup would leave the row
	// behind and pollute the shared in-mem DB for sibling tests.
	t.Cleanup(func() {
		_ = db.DeleteUserById(u.ID)
		_ = db.DeleteUserRolesByUser(u.ID)
	})
	return u
}

// TestUpdateUserRefusesDisablingLastAdmin proves op.UpdateUser blocks disabling
// the last enabled admin (parity with DeleteUserById's LastAdmin backstop).
func TestUpdateUserRefusesDisablingLastAdmin(t *testing.T) {
	seedSentinelRoles()
	admin := createEnabledAdmin(t, "lockout-disable-last")

	upd := *admin
	upd.Disabled = true
	if err := op.UpdateUser(&upd); err != errs.LastAdmin {
		t.Fatalf("disabling last enabled admin: want errs.LastAdmin, got %v", err)
	}
}

// TestUpdateUserRefusesDemotingLastAdmin proves op.UpdateUser blocks removing
// the ADMIN role from the last enabled admin.
func TestUpdateUserRefusesDemotingLastAdmin(t *testing.T) {
	seedSentinelRoles()
	admin := createEnabledAdmin(t, "lockout-demote-last")

	upd := *admin
	upd.Role = model.Roles{} // no ADMIN role → not an admin anymore
	if err := op.UpdateUser(&upd); err != errs.LastAdmin {
		t.Fatalf("demoting last enabled admin: want errs.LastAdmin, got %v", err)
	}
}

// TestUpdateUserAllowsWhenSecondAdminExists proves the backstop only fires for
// the *last* enabled admin: with a second enabled admin present, both disabling
// and demoting the first are allowed.
func TestUpdateUserAllowsWhenSecondAdminExists(t *testing.T) {
	seedSentinelRoles()
	first := createEnabledAdmin(t, "lockout-first-admin")
	_ = createEnabledAdmin(t, "lockout-second-admin")

	// disabling is allowed while a second enabled admin remains
	disable := *first
	disable.Disabled = true
	if err := op.UpdateUser(&disable); err != nil {
		t.Fatalf("disabling admin with a second admin present: want nil, got %v", err)
	}

	// re-enable, then demote — also allowed with a second admin present
	reenable := *first
	reenable.Disabled = false
	if err := op.UpdateUser(&reenable); err != nil {
		t.Fatalf("re-enabling admin: want nil, got %v", err)
	}
	demote := *first
	demote.Role = model.Roles{}
	if err := op.UpdateUser(&demote); err != nil {
		t.Fatalf("demoting admin with a second admin present: want nil, got %v", err)
	}
}
