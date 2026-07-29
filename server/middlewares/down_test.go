package middlewares

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// mustLocalStorage creates a real Local storage mounted at mountPath, backed
// by a fresh temp dir, so ResolvePathAnchor resolves through the actual
// mount table instead of a mock.
func mustLocalStorage(t *testing.T, mountPath string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, root),
	})
	if err != nil {
		t.Fatalf("create storage %s: %v", mountPath, err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
}

// mustSignAllMeta gates every path behind a sign requirement (needSign ==
// true everywhere), the same effect conf.SignAll has, but scoped to this
// test via a root meta so it does not leak into other tests in the package.
func mustSignAllMeta(t *testing.T) {
	t.Helper()
	m := &model.Meta{Path: "/", Password: "pw", PSub: true}
	if err := op.CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })
}

// runDown drives the rewritten Down() middleware directly against a gin
// wildcard param, the same shape the real "/d/*path" and "/p/*path" routes
// populate, without needing a live HTTP router.
func runDown(t *testing.T, reqPath, sign string) *gin.Context {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Down reads the path from c.Param("path") (which real requests get from
	// gin AFTER it unescapes the URL) and the signature from c.Query("sign").
	// The request URL's own path is never read, so keep it neutral: embedding
	// reqPath there would let names with URL-unsafe chars ('%', '{', '[') break
	// url.Parse and corrupt the sign query — an artifact of the test URL, not of
	// the middleware. The already-unescaped path is supplied via c.Params.
	c.Request = httptest.NewRequest("GET", "/d/_?sign="+url.QueryEscape(sign), nil)
	c.Params = gin.Params{{Key: "path", Value: reqPath}}
	Down()(c)
	return c
}

// A sign minted by common.SignPath (the same helper FsGet uses) for a real
// mounted path must pass Down's verify: this is the sign-then-verify round
// trip both sides of the download flow depend on.
func TestDownAcceptsCommonSignPathOutput(t *testing.T) {
	mustLocalStorage(t, "/dt4a")
	mustSignAllMeta(t)

	reqPath := "/dt4a/f.txt"
	s := common.SignPath(reqPath)

	c := runDown(t, reqPath, s)
	if c.IsAborted() {
		t.Fatalf("valid sign for %s must pass Down verify, got aborted", reqPath)
	}
}

// A sign minted for one storage's relpath must NOT verify against a
// different storage exposing the same relpath: Down resolves the ref (and
// thus the canonical string sign.Verify checks) from the storage the
// REQUEST path resolves to, not the one the sign was originally minted for.
// Two distinct mounts sharing a relpath ("/f.txt") must anchor to two
// distinct StorageIDs, so the canonical strings — and therefore the
// signatures — differ.
func TestDownRejectsSignFromDifferentStorageSameRelpath(t *testing.T) {
	mustLocalStorage(t, "/dt4b1")
	mustLocalStorage(t, "/dt4b2")
	mustSignAllMeta(t)

	signForB1 := common.SignPath("/dt4b1/f.txt")

	// Same sign, but the request path now resolves to storage b2: the
	// canonical ref differs (different StorageID, same Anchor), so
	// verification must fail.
	c := runDown(t, "/dt4b2/f.txt", signForB1)
	if !c.IsAborted() {
		t.Fatal("sign minted for one storage must not verify for a different storage's same relpath")
	}
}

// A file whose name contains a literal '%' must sign-then-verify cleanly.
// Regression: parsePath used to call url.PathUnescape a second time (gin has
// already unescaped the param once), so a name like "%7PFD7BW{)…png" — which
// reaches gin URL-encoded as "%257PFD7BW%7B%29…" and is decoded once to
// "%7PFD7BW{)…" — was decoded AGAIN, choking on the bare "%7P" and corrupting
// the path so its canonical ref no longer matched the signed one ("sign invalid").
func TestDownAcceptsLiteralPercentInName(t *testing.T) {
	mustLocalStorage(t, "/dt4pct")
	mustSignAllMeta(t)

	reqPath := "/dt4pct/%7PFD7BW{)UQCKBXR9N[0MI.png"
	s := common.SignPath(reqPath)

	c := runDown(t, reqPath, s)
	if c.IsAborted() {
		t.Fatalf("literal-%% filename must sign/verify through Down, got aborted")
	}
}

// An empty ?sign= must be rejected when needSign is true, not silently
// treated as "no signature required" (needSign only decides whether a check
// runs at all; once it says yes, Verify must fail closed on garbage input).
func TestDownRejectsEmptySignWhenRequired(t *testing.T) {
	mustLocalStorage(t, "/dt4c")
	mustSignAllMeta(t)

	c := runDown(t, "/dt4c/f.txt", "")
	if !c.IsAborted() {
		t.Fatal("empty sign with needSign true must be rejected, not passed through")
	}
}
