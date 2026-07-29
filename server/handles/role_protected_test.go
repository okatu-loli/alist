package handles

import (
	"fmt"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// createProtectedRole makes a non-reserved role and flips Protected=true at the
// db layer (the create API does not accept Protected). This mirrors migration
// 0005, which marks the built-in admin/guest roles protected=true.
func createProtectedRole(t *testing.T, name string) *model.Role {
	t.Helper()
	for i := 0; ; i++ {
		r := &model.Role{Name: fmt.Sprintf("%s-%d", name, i)}
		if err := op.CreateRole(r); err != nil {
			t.Fatal(err)
		}
		if r.ID <= model.ADMIN {
			// Role IDs double as IsAdmin/IsGuest identifiers; skip reserved IDs.
			_ = op.DeleteRole(r.ID)
			continue
		}
		r.Protected = true
		if err := db.UpdateRole(r); err != nil {
			t.Fatal(err)
		}
		id := r.ID
		t.Cleanup(func() {
			// Undo protection so the row can actually be removed.
			if rr, err := db.GetRole(id); err == nil {
				rr.Protected = false
				_ = db.UpdateRole(rr)
			}
			_ = db.DeleteRole(id)
		})
		return r
	}
}

// TestCannotDeleteOrRenameProtectedRole: a protected role refuses both rename
// (UpdateRole with a different name) and delete.
func TestCannotDeleteOrRenameProtectedRole(t *testing.T) {
	r := createProtectedRole(t, "prot-role")

	w := postJSON(handlerAdmin(), UpdateRole, "/api/admin/role/update",
		fmt.Sprintf(`{"id":%d,"name":"prot-role-renamed"}`, r.ID))
	if code := respCode(t, w); code == 200 {
		t.Fatalf("renaming a protected role must be refused, got 200")
	}
	got, err := op.GetRole(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != r.Name {
		t.Fatalf("refused rename must leave the name intact: got %q want %q", got.Name, r.Name)
	}

	w = postJSON(handlerAdmin(), DeleteRole,
		fmt.Sprintf("/api/admin/role/delete?id=%d", r.ID), "")
	if code := respCode(t, w); code == 200 {
		t.Fatalf("deleting a protected role must be refused, got 200")
	}
	if _, err := op.GetRole(r.ID); err != nil {
		t.Fatalf("refused delete must leave the role intact: %v", err)
	}
}

// TestCanDeleteAndRenameNonProtectedRole: a non-protected role can still be
// renamed and deleted — the Protected guard must not false-positive.
func TestCanDeleteAndRenameNonProtectedRole(t *testing.T) {
	r := createNonReservedRole(t, "unprot-role")
	newName := r.Name + "-renamed"

	w := postJSON(handlerAdmin(), UpdateRole, "/api/admin/role/update",
		fmt.Sprintf(`{"id":%d,"name":%q}`, r.ID, newName))
	if code := respCode(t, w); code != 200 {
		t.Fatalf("renaming a non-protected role must succeed, got %d: %s", code, w.Body.String())
	}
	got, err := op.GetRole(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != newName {
		t.Fatalf("rename should have applied: got %q want %q", got.Name, newName)
	}

	w = postJSON(handlerAdmin(), DeleteRole,
		fmt.Sprintf("/api/admin/role/delete?id=%d", r.ID), "")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("deleting a non-protected role must succeed, got %d: %s", code, w.Body.String())
	}
	if _, err := op.GetRole(r.ID); err == nil {
		t.Fatalf("non-protected role should have been deleted")
	}
}
