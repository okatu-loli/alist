package handles

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// FsGet's `related` sibling-file list came straight from an unfiltered
// fs.List (fsread.go ~:412) after the fs.list hide seam was removed in Step
// 5 T4: hidden siblings that share the requested file's name prefix leaked
// their name/size/hash to callers without see-hides. related must go
// through the same op.FilterListing gate as every other listing entry
// point.
func TestFsGetRelatedFiltersHiddenSiblings(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.txt"), []byte("visible"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "doc_hidden.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: "/t4fix",
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, root),
	}); err != nil {
		t.Fatalf("create storage: %v", err)
	}

	m := &model.Meta{Path: "/t4fix", Hide: "doc_hidden.*", HSub: true}
	if err := op.CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })

	// Permission 0: scope covers the path so read is implied (see model.ActRead
	// doc comment), but no ActSeeHides bit -> hidden siblings must stay hidden.
	r := &model.Role{Name: "s5t4-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/t4fix", Permission: 0},
	}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })

	u := &model.User{ID: 7772, Username: "s5t4-user", Role: model.Roles{int(r.ID)}}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"path":"/t4fix/doc.txt"}`)
	c.Request = httptest.NewRequest("POST", "/api/fs/get", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.User, u)

	FsGet(c)

	if w.Code != 200 {
		t.Fatalf("FsGet returned %d: %s", w.Code, w.Body.String())
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "doc.txt") {
		t.Fatalf("response must contain the requested file's own fields: %s", respBody)
	}
	if strings.Contains(respBody, "doc_hidden.txt") {
		t.Fatalf("hidden sibling leaked into related listing: %s", respBody)
	}
}
