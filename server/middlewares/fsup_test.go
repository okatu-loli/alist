package middlewares

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:middlewares_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
	gin.SetMode(gin.TestMode)
}

// makeUpUser: role 带 /up 读写、/up/ro 只读(写 deny 不必要——无 write allow 即默认拒)
func makeUpUser(t *testing.T) *model.User {
	t.Helper()
	r := &model.Role{Name: "fsup-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/up", Permission: 1 << 3}, // write
	}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	return &model.User{ID: 4444, Username: "fsup", Role: model.Roles{int(r.ID)}}
}

// runFsUp returns the logical outcome code: FsUp's ErrorResp/ErrorStrResp
// always write real HTTP 200 (the semantic code only lives in the JSON body),
// so a denial is only observable via c.Abort() having been called without
// c.Next() running. w.Code stays the httptest.Recorder default (200) either
// way, which is why abort state — not w.Code — is what distinguishes the
// two outcomes here.
func runFsUp(t *testing.T, u *model.User, filePath, password string) int {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/fs/put", nil)
	c.Request.Header.Set("File-Path", url.PathEscape(filePath))
	if password != "" {
		c.Request.Header.Set("Password", password)
	}
	c.Set(ctxkey.User, u)
	FsUp(c)
	if c.IsAborted() {
		return 403
	}
	return w.Code
}

func TestFsUpAuthorize(t *testing.T) {
	u := makeUpUser(t)
	if code := runFsUp(t, u, "/up/new.txt", ""); code != 200 {
		t.Fatalf("granted write path: got %d", code)
	}
	if code := runFsUp(t, u, "/nowhere/new.txt", ""); code == 200 {
		t.Fatalf("no-scope path must be denied")
	}
	// 密码保护目录:没带密码拒,带对密码过
	mustMetaUp(t, &model.Meta{Path: "/up/locked", Password: "pw", PSub: true})
	if code := runFsUp(t, u, "/up/locked/f.txt", ""); code == 200 {
		t.Fatalf("password-protected upload without password must be denied")
	}
	if code := runFsUp(t, u, "/up/locked/f.txt", "pw"); code != 200 {
		t.Fatalf("correct password upload: got %d", code)
	}
}

func mustMetaUp(t *testing.T, m *model.Meta) {
	t.Helper()
	if err := op.CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })
}

// getResponseBody extracts the response message from FsUp's JSON response.
// Returns ("permission denied", true) on 403, or ("", false) if no abort.
func getResponseMessage(t *testing.T, u *model.User, filePath, password string) (string, bool) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/fs/put", nil)
	c.Request.Header.Set("File-Path", url.PathEscape(filePath))
	if password != "" {
		c.Request.Header.Set("Password", password)
	}
	c.Set(ctxkey.User, u)
	FsUp(c)
	if !c.IsAborted() {
		return "", false
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	return resp.Message, true
}

func TestFsUpDeniesReadOnlyUser(t *testing.T) {
	// Create a read-only role: no write bit, but scope sync adds ActRead.
	// This allows us to test the ActWrite-specific denial path.
	r := &model.Role{Name: "readonly-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/ro", Permission: 0}, // No bits set; sync appends ActRead
	}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })

	// User with read-only role at /ro
	roUser := &model.User{ID: 4445, Username: "readonly", Role: model.Roles{int(r.ID)}}

	// Main test: write fails with ActWrite denial message, not ActRead denial.
	// FsUp checks both Authorize(ActRead) and Authorize(ActWrite).
	// For a read-only user, ActRead passes but ActWrite fails with "permission denied".
	msg, denied := getResponseMessage(t, roUser, "/ro/file.txt", "")
	if !denied {
		t.Fatal("write should be denied")
	}
	if msg != "permission denied" {
		t.Fatalf("expected ActWrite denial 'permission denied', got: %s", msg)
	}

	// Sanity control: user WITH write bit still passes
	rw := &model.Role{Name: "readwrite-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/rw", Permission: 1 << 3}, // ActWrite bit
	}}
	if err := op.CreateRole(rw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(rw.ID) })
	rwUser := &model.User{ID: 4446, Username: "readwrite", Role: model.Roles{int(rw.ID)}}

	code := runFsUp(t, rwUser, "/rw/file.txt", "")
	if code != 200 {
		t.Fatalf("write with permission should pass, got %d", code)
	}
}
