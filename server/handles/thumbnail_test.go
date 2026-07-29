package handles

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/thumbnail"
	"github.com/gin-gonic/gin"
)

// FsThumb handler tests. The db/migration bootstrap for this package's whole
// test binary lives in permission_test.go's init(); mirrors search_test.go /
// labels_test.go for the Local-storage fixture shape.

func init() {
	// Mirror internal/bootstrap.InitThumbnailers (Phase E wave 3): the
	// handler below dispatches through internal/extension now, so the
	// built-in image Thumbnailer must be registered here exactly like
	// production bootstrap does.
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    thumbnail.ImageThumbnailerExtensionID,
		Kinds: []ext.Kind{ext.KindThumbnailer},
	}); err != nil {
		panic(err)
	}
	extension.RegisterTrustedThumbnailerInstance(thumbnail.ImageThumbnailerExtensionID, thumbnail.NewImageThumbnailer())
}

var thumbHandlerFixtureSeq int64

// thumbFixturePNG is a tiny, deterministic 40x20 PNG shared by every fixture.
func thumbFixturePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x * 5), G: uint8(y * 5), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

// setupThumbHandlerFixture mounts an isolated Local storage with a readable
// photo.png and an unreadable (no ActRead grant) secret.png, returning the
// scoped caller and the mount path. flags.DataDir is redirected to a
// throwaway dir for the duration of the test.
func setupThumbHandlerFixture(t *testing.T) (*model.User, string) {
	t.Helper()
	n := atomic.AddInt64(&thumbHandlerFixtureSeq, 1)
	mount := fmt.Sprintf("/thumbh%d", n)

	oldDataDir := flags.DataDir
	flags.DataDir = t.TempDir()
	t.Cleanup(func() { flags.DataDir = oldDataDir })

	dir := t.TempDir()
	png := thumbFixturePNG(t)
	if err := os.WriteFile(filepath.Join(dir, "photo.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sec", "secret.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	// Scoped caller: recursive ActRead on the mount, then an explicit recursive
	// deny on the "sec" subtree -- mirrors setupSearchFixture's deny-nested
	// pattern, so the 403 case is a real policy denial, not just "no grant at
	// all" (already covered separately by an ungranted user in
	// TestFsThumbUnauthorized).
	user := &model.User{
		ID:       91000 + uint(n),
		Username: fmt.Sprintf("thumbhreader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{
			{Path: mount, Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
			{Path: mount + "/sec", Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true},
		},
	}
	return user, mount
}

// driveThumb runs FsThumb as a GET with query params and the caller in
// context, mirroring driveSearch in search_test.go.
func driveThumb(u *model.User, q url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?"+q.Encode(), nil)
	c.Set(ctxkey.User, u)
	FsThumb(c)
	return w
}

func TestFsThumbSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupThumbHandlerFixture(t)

	w := driveThumb(user, url.Values{"path": {mount + "/photo.png"}, "w": {"64"}})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", ct)
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("expected an ETag header on a successful thumbnail response")
	}
	if cc := w.Header().Get("Cache-Control"); cc == "" {
		t.Fatal("expected a Cache-Control header on a successful thumbnail response")
	}
	img, format, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %q, want jpeg", format)
	}
	b := img.Bounds()
	if b.Dx() > 64 || b.Dy() > 64 {
		t.Fatalf("dims %dx%d exceed requested w=64", b.Dx(), b.Dy())
	}
}

func TestFsThumbMissingPathIs400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, _ := setupThumbHandlerFixture(t)
	w := driveThumb(user, url.Values{})
	if w.Code != 400 {
		t.Fatalf("expected 400 for missing path, got %d", w.Code)
	}
}

func TestFsThumbUnauthorizedIs403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupThumbHandlerFixture(t)
	w := driveThumb(user, url.Values{"path": {mount + "/sec/secret.png"}})
	if w.Code != 403 {
		t.Fatalf("expected 403 for a read-denied path, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFsThumbNonImageIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupThumbHandlerFixture(t)
	w := driveThumb(user, url.Values{"path": {mount + "/notes.txt"}})
	if w.Code != 404 {
		t.Fatalf("expected 404 for a non-image file, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFsThumbNoSuchPathIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupThumbHandlerFixture(t)
	w := driveThumb(user, url.Values{"path": {mount + "/does-not-exist.png"}})
	if w.Code != 404 {
		t.Fatalf("expected 404 for a nonexistent path, got %d: %s", w.Code, w.Body.String())
	}
}

// TestFsThumbIfNoneMatch304 pins the ETag/If-None-Match round trip: a second
// request carrying the ETag the first response returned gets a bodyless 304.
func TestFsThumbIfNoneMatch304(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, mount := setupThumbHandlerFixture(t)

	w1 := driveThumb(user, url.Values{"path": {mount + "/photo.png"}})
	if w1.Code != 200 {
		t.Fatalf("first request: expected 200, got %d", w1.Code)
	}
	etag := w1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response carried no ETag")
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?path="+url.QueryEscape(mount+"/photo.png"), nil)
	c.Request.Header.Set("If-None-Match", etag)
	c.Set(ctxkey.User, user)
	FsThumb(c)

	if w.Code != 304 {
		t.Fatalf("expected 304 with a matching If-None-Match, got %d", w.Code)
	}
	if len(w.Body.Bytes()) != 0 {
		t.Fatalf("expected an empty body on 304, got %d bytes", len(w.Body.Bytes()))
	}
}
