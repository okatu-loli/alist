package handles

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// A directory whose only children are invisible to the caller (permission
// deny, not literal emptiness) must NOT be treated as empty by
// FsRemoveEmptyDirectory: commit 7f448647 ran the emptiness check on a
// FilterListing'd listing, so such a directory looked empty and was
// physically deleted along with its hidden content.
func TestFsRemoveEmptyDirectorySkipsHiddenChildren(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sweep", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sweep", "keep", "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: "/t3fix",
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, root),
	}); err != nil {
		t.Fatalf("create storage: %v", err)
	}

	r := &model.Role{Name: "s5t3-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/t3fix", Permission: 1<<3 | 1<<7}, // write + remove
	}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })

	// Manual deny-read on the file inside "keep", NOT on "keep" itself: "keep"
	// must stay visible in its parent's listing (so it's queued for the
	// emptiness check) while its only child is hidden from that check. A deny
	// on "keep" itself would hide the directory entry from its parent listing
	// before the sweep ever queues it, masking the bug this test targets.
	deny := &model.Permission{
		Name:      "s5t3-deny-secret",
		Path:      "/t3fix/sweep/keep/secret.txt",
		Actions:   model.Actions{model.ActRead},
		Effect:    model.EffectDeny,
		Recursive: true,
	}
	if err := op.CreatePermission(deny); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(deny.ID) })

	existingIDs, err := db.GetPermissionIDsByRole(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.SetRolePermissions(r.ID, append(existingIDs, deny.ID)); err != nil {
		t.Fatal(err)
	}

	u := &model.User{ID: 7771, Username: "s5t3-user", Role: model.Roles{int(r.ID)}}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"src_dir":"/t3fix/sweep"}`)
	c.Request = httptest.NewRequest("POST", "/api/fs/remove_empty_directory", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.User, u)

	FsRemoveEmptyDirectory(c)

	if w.Code != 200 {
		t.Fatalf("FsRemoveEmptyDirectory returned %d: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(root, "sweep", "keep")); err != nil {
		t.Fatalf("sweep/keep must survive: its content is only hidden from this caller, "+
			"not actually empty, but the directory is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sweep", "keep", "secret.txt")); err != nil {
		t.Fatalf("secret.txt must survive along with its parent directory: %v", err)
	}
}
