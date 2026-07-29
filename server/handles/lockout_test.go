package handles

import (
	"fmt"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// mkAdmin persists an admin user. The in-memory test DB never seeds the
// built-in roles, and role IDs double as the IsAdmin identifier (ADMIN=2), so
// an admin is simply a user whose role binding contains model.ADMIN. Cleanup
// deletes at the db layer to bypass the very guards under test.
func mkAdmin(t *testing.T, username string, disabled bool) *model.User {
	t.Helper()
	u := &model.User{Username: username, Role: model.Roles{int(model.ADMIN)}, Disabled: disabled}
	u.SetPassword("x")
	if err := op.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID) })
	return u
}

// mkPlainUser persists a non-admin user bound to a fresh non-reserved role.
func mkPlainUser(t *testing.T, username string) *model.User {
	t.Helper()
	role := createNonReservedRole(t, "lo-role")
	u := &model.User{Username: username, Role: model.Roles{int(role.ID)}}
	u.SetPassword("x")
	if err := op.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID) })
	return u
}

// TestCannotDeleteLastAdmin: the sole enabled admin cannot be deleted, else the
// whole admin surface (which short-circuits on IsAdmin) becomes unreachable.
func TestCannotDeleteLastAdmin(t *testing.T) {
	admin := mkAdmin(t, "lo-last-admin", false)
	w := postJSON(handlerAdmin(), DeleteUser,
		fmt.Sprintf("/api/admin/user/delete?id=%d", admin.ID), "")
	if code := respCode(t, w); code == 200 {
		t.Fatalf("deleting the last enabled admin must be refused, got 200: %s", w.Body.String())
	}
	if _, err := op.GetUserById(admin.ID); err != nil {
		t.Fatalf("refused delete must leave the admin intact: %v", err)
	}
}

// TestCanDeleteNonLastAdmin: with a second enabled admin present, an admin is
// freely deletable — the guard must not false-positive.
func TestCanDeleteNonLastAdmin(t *testing.T) {
	_ = mkAdmin(t, "lo-keep-admin", false)
	victim := mkAdmin(t, "lo-victim-admin", false)
	w := postJSON(handlerAdmin(), DeleteUser,
		fmt.Sprintf("/api/admin/user/delete?id=%d", victim.ID), "")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("deleting a non-last admin must succeed, got %d: %s", code, w.Body.String())
	}
	if _, err := op.GetUserById(victim.ID); err == nil {
		t.Fatalf("non-last admin should have been deleted")
	}
}

// TestCanDeleteNonAdminUser: a plain user is deletable even when only one admin
// exists in the whole system.
func TestCanDeleteNonAdminUser(t *testing.T) {
	_ = mkAdmin(t, "lo-solo-admin", false)
	victim := mkPlainUser(t, "lo-plain-user")
	w := postJSON(handlerAdmin(), DeleteUser,
		fmt.Sprintf("/api/admin/user/delete?id=%d", victim.ID), "")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("deleting a non-admin user must succeed, got %d: %s", code, w.Body.String())
	}
	if _, err := op.GetUserById(victim.ID); err == nil {
		t.Fatalf("plain user should have been deleted")
	}
}

// TestCannotDeleteAdminUser: the built-in "admin" account is structurally
// undeletable even when a second enabled admin exists (so the last-admin guard
// is not what fires here).
func TestCannotDeleteAdminUser(t *testing.T) {
	_ = mkAdmin(t, "lo-second-admin", false)
	adminAcct := mkAdmin(t, "admin", false)
	w := postJSON(handlerAdmin(), DeleteUser,
		fmt.Sprintf("/api/admin/user/delete?id=%d", adminAcct.ID), "")
	if code := respCode(t, w); code == 200 {
		t.Fatalf("the built-in admin user must be structurally undeletable, got 200")
	}
	if _, err := op.GetUserById(adminAcct.ID); err != nil {
		t.Fatalf("refused delete must leave the admin account intact: %v", err)
	}
}

// TestCannotRemoveAdminRoleFromLastAdmin: demoting the last admin (swapping the
// ADMIN role for a plain role) must be refused.
func TestCannotRemoveAdminRoleFromLastAdmin(t *testing.T) {
	admin := mkAdmin(t, "lo-demote-admin", false)
	plain := createNonReservedRole(t, "lo-plain-role")
	w := postJSON(handlerAdmin(), UpdateUser, "/api/admin/user/update",
		fmt.Sprintf(`{"id":%d,"username":"lo-demote-admin","role":[%d]}`, admin.ID, plain.ID))
	if code := respCode(t, w); code == 200 {
		t.Fatalf("removing the ADMIN role from the last admin must be refused, got 200")
	}
	got, err := op.GetUserById(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsAdmin() {
		t.Fatalf("refused demotion must leave the admin role intact, got %v", got.Role)
	}
}

// TestCannotDisableLastAdmin: disabling the last enabled admin must be refused.
func TestCannotDisableLastAdmin(t *testing.T) {
	admin := mkAdmin(t, "lo-disable-admin", false)
	w := postJSON(handlerAdmin(), UpdateUser, "/api/admin/user/update",
		fmt.Sprintf(`{"id":%d,"username":"lo-disable-admin","role":[%d],"disabled":true}`, admin.ID, model.ADMIN))
	if code := respCode(t, w); code == 200 {
		t.Fatalf("disabling the last admin must be refused, got 200")
	}
	got, err := op.GetUserById(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disabled {
		t.Fatalf("refused disable must leave the admin enabled")
	}
}

// TestCanDemoteNonLastAdmin: with a second admin present, demoting one admin
// succeeds — the guard must not false-positive.
func TestCanDemoteNonLastAdmin(t *testing.T) {
	_ = mkAdmin(t, "lo-other-admin", false)
	admin := mkAdmin(t, "lo-demotable-admin", false)
	plain := createNonReservedRole(t, "lo-target-role")
	w := postJSON(handlerAdmin(), UpdateUser, "/api/admin/user/update",
		fmt.Sprintf(`{"id":%d,"username":"lo-demotable-admin","role":[%d]}`, admin.ID, plain.ID))
	if code := respCode(t, w); code != 200 {
		t.Fatalf("demoting a non-last admin must succeed, got %d: %s", code, w.Body.String())
	}
	got, err := op.GetUserById(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsAdmin() {
		t.Fatalf("demotion should have removed the admin role, got %v", got.Role)
	}
}
