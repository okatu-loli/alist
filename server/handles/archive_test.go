package handles

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/archive"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// FsArchiveMeta/List/Extract handler tests. The db/migration bootstrap for
// this package's whole test binary lives in permission_test.go's init().

func init() {
	// Mirror internal/bootstrap.InitArchiveTools (Phase E wave 2): the
	// handlers below dispatch through internal/extension now, so the
	// built-in zip ArchiveTool must be registered here exactly like
	// production bootstrap does.
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    archive.ZipToolExtensionID,
		Kinds: []ext.Kind{ext.KindArchiveTool},
	}); err != nil {
		panic(err)
	}
	extension.RegisterTrustedArchiveToolInstance(archive.ZipToolExtensionID, archive.NewZipArchiveTool())
}

var archiveHandlerFixtureSeq int64

// buildHandlerFixtureZip mirrors internal/archive/archive_test.go's fixture
// layout (a.txt, dir1/b.txt, dir1/dir2/c.txt, emptydir/) minus the
// package-internal-only encrypted/zip-slip entries, which are pinned at the
// internal/archive level already -- this layer only needs to pin the HTTP
// plumbing (status/envelope mapping, streaming).
func buildHandlerFixtureZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip Create(%q): %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write(%q): %v", name, err)
		}
	}
	write("a.txt", []byte("root-a"))
	write("dir1/b.txt", []byte("dir1-b"))
	if _, err := zw.Create("emptydir/"); err != nil {
		t.Fatalf("zip Create(emptydir/): %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

// setupArchiveHandlerFixture mounts an isolated Local storage with a
// readable archive.zip and secret/archive.zip (read-denied), plus a plain
// notes.txt, returning the scoped caller and the mount path.
func setupArchiveHandlerFixture(t *testing.T) (*model.User, string) {
	t.Helper()
	n := atomic.AddInt64(&archiveHandlerFixtureSeq, 1)
	mount := fmt.Sprintf("/archiveh%d", n)

	dir := t.TempDir()
	zipBytes := buildHandlerFixtureZip(t)
	if err := os.WriteFile(filepath.Join(dir, "archive.zip"), zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sec", "archive.zip"), zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	user := &model.User{
		ID:       92000 + uint(n),
		Username: fmt.Sprintf("archivehreader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{
			{Path: mount, Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
			{Path: mount + "/sec", Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true},
		},
	}
	return user, mount
}

func driveArchive(u *model.User, q url.Values, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?"+q.Encode(), nil)
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

func TestFsArchiveMetaSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)

	w := driveArchive(user, url.Values{"path": {mount + "/archive.zip"}}, FsArchiveMeta)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("expected code 200, got %d: %s", got, w.Body.String())
	}
	var env struct {
		Data archive.ArchiveMeta `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse response: %v (%s)", err, w.Body.String())
	}
	if env.Data.EntryCount != 2 {
		t.Fatalf("EntryCount = %d, want 2", env.Data.EntryCount)
	}
	if env.Data.Encrypted {
		t.Fatal("Encrypted = true, want false (no encrypted entries in the handler fixture)")
	}
}

func TestFsArchiveMetaMissingPathIs400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, _ := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{}, FsArchiveMeta)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("expected code 400 for missing path, got %d: %s", got, w.Body.String())
	}
}

func TestFsArchiveMetaUnauthorizedIs403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{"path": {mount + "/sec/archive.zip"}}, FsArchiveMeta)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("expected code 403 for a read-denied path, got %d: %s", got, w.Body.String())
	}
}

func TestFsArchiveMetaNonZipIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{"path": {mount + "/notes.txt"}}, FsArchiveMeta)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("expected code 404 for a non-zip file, got %d: %s", got, w.Body.String())
	}
}

func TestFsArchiveListRoot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)

	w := driveArchive(user, url.Values{"path": {mount + "/archive.zip"}}, FsArchiveList)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("expected code 200, got %d: %s", got, w.Body.String())
	}
	var env struct {
		Data []archive.Entry `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse response: %v (%s)", err, w.Body.String())
	}
	if len(env.Data) != 3 {
		t.Fatalf("List(root) = %d entries, want 3 (a.txt, dir1, emptydir): %+v", len(env.Data), env.Data)
	}
}

func TestFsArchiveListInnerNotFoundIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{"path": {mount + "/archive.zip"}, "inner": {"nope"}}, FsArchiveList)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("expected code 404 for a missing inner path, got %d: %s", got, w.Body.String())
	}
}

func TestFsArchiveExtractStreamsBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)

	w := driveArchive(user, url.Values{"path": {mount + "/archive.zip"}, "inner": {"a.txt"}}, FsArchiveExtract)
	if w.Code != 200 {
		t.Fatalf("expected real HTTP 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "root-a" {
		t.Fatalf("extracted bytes = %q, want %q", got, "root-a")
	}
	if cd := w.Header().Get("Content-Disposition"); cd == "" {
		t.Fatal("expected a Content-Disposition header on a successful extract")
	}
}

func TestFsArchiveExtractMissingPathIs400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, _ := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{"inner": {"a.txt"}}, FsArchiveExtract)
	if w.Code != 400 {
		t.Fatalf("expected real HTTP 400 for missing path, got %d", w.Code)
	}
}

func TestFsArchiveExtractUnauthorizedIs403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{"path": {mount + "/sec/archive.zip"}, "inner": {"a.txt"}}, FsArchiveExtract)
	if w.Code != 403 {
		t.Fatalf("expected real HTTP 403 for a read-denied path, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFsArchiveExtractOnDirectoryIs400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{"path": {mount + "/archive.zip"}, "inner": {"emptydir"}}, FsArchiveExtract)
	if w.Code != 400 {
		t.Fatalf("expected real HTTP 400 for a directory inner path, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFsArchiveExtractInnerNotFoundIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupArchiveHandlerFixture(t)
	w := driveArchive(user, url.Values{"path": {mount + "/archive.zip"}, "inner": {"nope.txt"}}, FsArchiveExtract)
	if w.Code != 404 {
		t.Fatalf("expected real HTTP 404 for a missing inner path, got %d: %s", w.Code, w.Body.String())
	}
}
