package handles

import (
	"fmt"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// createDefaultRole persists a non-reserved role and marks it the system
// default the same way GetDefaultRoleID resolves it (the `default = true`
// fallback query). Written at the db layer to sidestep op.CreateRole's default
// path, which requires the DefaultRole setting row that the migration-only test
// DB never seeds.
func createDefaultRole(t *testing.T) *model.Role {
	t.Helper()
	r := createNonReservedRole(t, "cu-default-role")
	if err := db.GetDb().Model(&model.Role{}).Where("id = ?", r.ID).Update("default", true).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.GetDb().Model(&model.Role{}).Where("id = ?", r.ID).Update("default", false).Error
	})
	if got := op.GetDefaultRoleID(); got != int(r.ID) {
		t.Fatalf("default role setup failed: GetDefaultRoleID()=%d, want %d", got, r.ID)
	}
	return r
}

// TestOperatorCannotCreateUserWithPrivilegedRole: a non-admin user-manager
// operator calling CreateUser with an operator-chosen role (not the default) is
// the CREATE twin of the self-escalation vector UpdateUser's role-binding gate
// closes (spec §5.3, decision A: role assignment is IsAdmin-only). It must be
// 403 and must not persist a user.
func TestOperatorCannotCreateUserWithPrivilegedRole(t *testing.T) {
	_ = createDefaultRole(t)
	operator := makeHandlerOperator(t)
	privileged := createNonReservedRole(t, "cu-editor-role")

	w := postJSON(operator, CreateUser, "/api/admin/user/create",
		fmt.Sprintf(`{"username":"cu-puppet","password":"x","base_path":"/","role":[%d]}`, privileged.ID))
	if code := respCode(t, w); code != 403 {
		t.Fatalf("non-admin creating a user with a chosen role must be 403, got %d: %s", code, w.Body.String())
	}
	if u, err := db.GetUserByName("cu-puppet"); err == nil {
		_ = db.DeleteUserById(u.ID)
		t.Fatalf("denied create must not persist a user, but %q exists", u.Username)
	}
}

// TestOperatorCanCreateUserWithDefaultRole: the operator retains user lifecycle
// — creating a user with the default role (or an empty role that defaults to
// it) must succeed. The gate must not false-positive on the default binding.
func TestOperatorCanCreateUserWithDefaultRole(t *testing.T) {
	def := createDefaultRole(t)
	operator := makeHandlerOperator(t)

	// explicit default role
	w := postJSON(operator, CreateUser, "/api/admin/user/create",
		fmt.Sprintf(`{"username":"cu-plain-explicit","password":"x","base_path":"/","role":[%d]}`, def.ID))
	if code := respCode(t, w); code != 200 {
		t.Fatalf("operator creating a user with the default role must be 200, got %d: %s", code, w.Body.String())
	}
	assertUserCreatedWithRole(t, "cu-plain-explicit", int(def.ID))

	// empty role -> defaults to the default role
	w = postJSON(operator, CreateUser, "/api/admin/user/create",
		`{"username":"cu-plain-empty","password":"x","base_path":"/"}`)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("operator creating a user with empty role must be 200, got %d: %s", code, w.Body.String())
	}
	assertUserCreatedWithRole(t, "cu-plain-empty", int(def.ID))
}

// TestAdminCanCreateUserWithAnyNonAdminRole: an admin caller keeps full freedom
// — binding any non-admin role at create time is unchanged (regression: the
// gate must never fire for IsAdmin).
func TestAdminCanCreateUserWithAnyNonAdminRole(t *testing.T) {
	_ = createDefaultRole(t)
	privileged := createNonReservedRole(t, "cu-admin-chosen-role")

	w := postJSON(handlerAdmin(), CreateUser, "/api/admin/user/create",
		fmt.Sprintf(`{"username":"cu-admin-made","password":"x","base_path":"/","role":[%d]}`, privileged.ID))
	if code := respCode(t, w); code != 200 {
		t.Fatalf("admin creating a user with a chosen non-admin role must be 200, got %d: %s", code, w.Body.String())
	}
	assertUserCreatedWithRole(t, "cu-admin-made", int(privileged.ID))
}

func assertUserCreatedWithRole(t *testing.T, username string, roleID int) {
	t.Helper()
	u, err := db.GetUserByName(username)
	if err != nil {
		t.Fatalf("expected user %q to be created: %v", username, err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID) })
	if len(u.Role) != 1 || u.Role[0] != roleID {
		t.Fatalf("user %q role = %v, want [%d]", username, u.Role, roleID)
	}
}
