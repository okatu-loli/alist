package webdav

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

var handlerMountSeq int64

// newDavRouter mounts the full production chain (extension gate + Basic
// Auth + Handler) at "/dav/*path", exactly as server/router.go wires it, so
// these tests exercise the real request path rather than calling alistFS
// methods directly. It also registers a fresh builtin.<name> extension
// (enabled) so the gate test can flip it off/on without touching the
// process-wide builtin.webdav entry other tests might share.
func newDavRouter(t *testing.T, gateID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID: gateID, Version: "builtin", Kinds: []ext.Kind{ext.KindFeature},
	}); err != nil {
		t.Fatalf("register gate extension: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(gateID) })

	r := gin.New()
	davMethods := []string{
		http.MethodOptions, "PROPFIND", http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, "MKCOL", http.MethodDelete, "MOVE", "COPY", "LOCK", "UNLOCK", "PROPPATCH",
	}
	r.Match(davMethods, "/dav/*path", middlewares.RequireExtensionEnabled(gateID), BasicAuth, Handler)
	return r
}

func doReq(r *gin.Engine, method, target, username, password string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newHandlerTestFixture(t *testing.T) (gateID, mountPath string) {
	t.Helper()
	n := atomic.AddInt64(&handlerMountSeq, 1)
	gateID = fmt.Sprintf("builtin.webdavtest%d", n)
	mountPath = fmt.Sprintf("/davmnt%d", n)
	setupTempDir(t)
	setupLocalStorage(t, mountPath)
	return gateID, mountPath
}

// TestWebdavPutGetRoundTrip proves the core data path: PUT a file, read it
// back byte-for-byte via GET, including a Range request.
func TestWebdavPutGetRoundTrip(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	content := []byte("hello webdav world, this is the file body")
	target := mount + "/hello.txt"

	w := doReq(r, http.MethodPut, "/dav"+target, u.Username, "pw", content, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT: want 201, got %d body=%s", w.Code, w.Body.String())
	}

	w = doReq(r, http.MethodGet, "/dav"+target, u.Username, "pw", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(content) {
		t.Fatalf("GET body mismatch: got %q want %q", got, string(content))
	}

	// Ranged GET: bytes 6-10 inclusive ("webda").
	w = doReq(r, http.MethodGet, "/dav"+target, u.Username, "pw", nil, map[string]string{"Range": "bytes=6-10"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("ranged GET: want 206, got %d body=%s", w.Code, w.Body.String())
	}
	if got, want := w.Body.String(), string(content[6:11]); got != want {
		t.Fatalf("ranged GET body mismatch: got %q want %q", got, want)
	}
}

// TestWebdavMkcolPropfindDelete proves MKCOL creates a collection, PROPFIND
// lists its child, and DELETE removes it (with a subsequent GET 404ing).
func TestWebdavMkcolPropfindDelete(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	dir := mount + "/sub"
	w := doReq(r, "MKCOL", "/dav"+dir, u.Username, "pw", nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("MKCOL: want 201, got %d body=%s", w.Code, w.Body.String())
	}

	w = doReq(r, http.MethodPut, "/dav"+dir+"/child.txt", u.Username, "pw", []byte("x"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT child: want 201, got %d", w.Code)
	}

	w = doReq(r, "PROPFIND", "/dav"+dir, u.Username, "pw", nil, map[string]string{"Depth": "1"})
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: want 207, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "child.txt") {
		t.Fatalf("PROPFIND response must list child.txt, got %s", w.Body.String())
	}

	w = doReq(r, http.MethodDelete, "/dav"+dir, u.Username, "pw", nil, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: want 204, got %d body=%s", w.Code, w.Body.String())
	}

	w = doReq(r, http.MethodGet, "/dav"+dir+"/child.txt", u.Username, "pw", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: want 404, got %d", w.Code)
	}
}

// TestWebdavMove proves a same-directory rename and a cross-directory move
// both land the content at the new location.
func TestWebdavMove(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	src := mount + "/a.txt"
	w := doReq(r, http.MethodPut, "/dav"+src, u.Username, "pw", []byte("move me"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT: want 201, got %d", w.Code)
	}

	// same-directory rename
	renamed := mount + "/b.txt"
	w = doReq(r, "MOVE", "/dav"+src, u.Username, "pw", nil, map[string]string{"Destination": "/dav" + renamed})
	if w.Code != http.StatusCreated {
		t.Fatalf("MOVE (rename): want 201, got %d body=%s", w.Code, w.Body.String())
	}
	w = doReq(r, http.MethodGet, "/dav"+renamed, u.Username, "pw", nil, nil)
	if w.Code != http.StatusOK || w.Body.String() != "move me" {
		t.Fatalf("GET after rename: want 200/'move me', got %d/%q", w.Code, w.Body.String())
	}

	// cross-directory move
	if code := doReq(r, "MKCOL", "/dav"+mount+"/dst", u.Username, "pw", nil, nil).Code; code != http.StatusCreated {
		t.Fatalf("MKCOL dst: want 201, got %d", code)
	}
	dst := mount + "/dst/b.txt"
	w = doReq(r, "MOVE", "/dav"+renamed, u.Username, "pw", nil, map[string]string{"Destination": "/dav" + dst})
	if w.Code != http.StatusCreated {
		t.Fatalf("MOVE (cross-dir): want 201, got %d body=%s", w.Code, w.Body.String())
	}
	w = doReq(r, http.MethodGet, "/dav"+dst, u.Username, "pw", nil, nil)
	if w.Code != http.StatusOK || w.Body.String() != "move me" {
		t.Fatalf("GET after cross-dir move: want 200/'move me', got %d/%q", w.Code, w.Body.String())
	}
}

// TestWebdavReadOnlyScopeDeniedWriteOps pins design W5's central security
// property: a user whose granted scope carries only ActRead gets 403 on
// PUT/MKCOL/DELETE, while GET/PROPFIND still work.
func TestWebdavReadOnlyScopeDeniedWriteOps(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	admin := newTestUser(t, "pw", "")
	grantFullAccess(t, admin, mount)
	ro := newTestUser(t, "pw", "")
	grantReadOnlyAccess(t, ro, mount)
	r := newDavRouter(t, gateID)

	// Seed a file as the full-access user.
	seeded := mount + "/seed.txt"
	if code := doReq(r, http.MethodPut, "/dav"+seeded, admin.Username, "pw", []byte("seed"), nil).Code; code != http.StatusCreated {
		t.Fatalf("seed PUT: want 201, got %d", code)
	}

	if w := doReq(r, http.MethodPut, "/dav"+mount+"/blocked.txt", ro.Username, "pw", []byte("nope"), nil); w.Code != http.StatusForbidden {
		t.Fatalf("read-only PUT: want 403, got %d body=%s", w.Code, w.Body.String())
	}
	if w := doReq(r, "MKCOL", "/dav"+mount+"/blockeddir", ro.Username, "pw", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("read-only MKCOL: want 403, got %d body=%s", w.Code, w.Body.String())
	}
	if w := doReq(r, http.MethodDelete, "/dav"+seeded, ro.Username, "pw", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("read-only DELETE: want 403, got %d body=%s", w.Code, w.Body.String())
	}

	// Read still works for the read-only user.
	if w := doReq(r, http.MethodGet, "/dav"+seeded, ro.Username, "pw", nil, nil); w.Code != http.StatusOK || w.Body.String() != "seed" {
		t.Fatalf("read-only GET: want 200/'seed', got %d/%q", w.Code, w.Body.String())
	}
}

// TestWebdavOutOfScopeDenied proves a user with no grant at all on a second
// mount cannot read it via WebDAV -- op.Authorize confines the caller to
// exactly the paths its policy covers, same as a browser session.
func TestWebdavOutOfScopeDenied(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	// A second mount the user has no permission row for at all.
	n := atomic.AddInt64(&handlerMountSeq, 1)
	otherMount := fmt.Sprintf("/davother%d", n)
	setupLocalStorage(t, otherMount)

	if w := doReq(r, http.MethodGet, "/dav"+otherMount+"/secret.txt", u.Username, "pw", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope GET: want 403, got %d body=%s", w.Code, w.Body.String())
	}

	// The same effective path reached through a ".." segment must collapse
	// (utils.FixAndCleanPath) to the very same denied path -- never escape to
	// serve content from outside the caller's granted scope.
	traversal := "/dav" + mount + "/../.." + otherMount + "/secret.txt"
	if w := doReq(r, http.MethodGet, traversal, u.Username, "pw", nil, nil); w.Code == http.StatusOK {
		t.Fatalf("traversal path must not leak out-of-scope content, got 200 body=%q", w.Body.String())
	}
}

// TestWebdavAuthFailures pins the Basic Auth boundary itself: no
// credentials, and wrong credentials, both 401 with the WWW-Authenticate
// challenge (design W5 §①: "统一失败 401").
func TestWebdavAuthFailures(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	w := doReq(r, http.MethodGet, "/dav"+mount+"/x.txt", "", "", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: want 401, got %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}

	w = doReq(r, http.MethodGet, "/dav"+mount+"/x.txt", u.Username, "wrong-password", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d", w.Code)
	}
}

// TestWebdavPropfindFiltersDeniedChild pins Finding 1 (W5 security review):
// PROPFIND/Readdir must run every child through op.FilterListing, the same
// per-child RBAC gate server/handles/fsread.go and hostcap.List use, so a
// directory listing never leaks the name/size/modtime of a child the caller
// is denied ActRead on.
func TestWebdavPropfindFiltersDeniedChild(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	if code := doReq(r, http.MethodPut, "/dav"+mount+"/visible.txt", u.Username, "pw", []byte("v"), nil).Code; code != http.StatusCreated {
		t.Fatalf("seed visible.txt: want 201, got %d", code)
	}
	if code := doReq(r, http.MethodPut, "/dav"+mount+"/secret.txt", u.Username, "pw", []byte("s"), nil).Code; code != http.StatusCreated {
		t.Fatalf("seed secret.txt: want 201, got %d", code)
	}

	denyReadAccess(t, u, mount+"/secret.txt")

	w := doReq(r, "PROPFIND", "/dav"+mount, u.Username, "pw", nil, map[string]string{"Depth": "1"})
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: want 207, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "visible.txt") {
		t.Fatalf("PROPFIND must still list the permitted sibling, got %s", body)
	}
	if strings.Contains(body, "secret.txt") {
		t.Fatalf("PROPFIND must not leak a child denied ActRead, got %s", body)
	}

	// Sanity: the deny grant is real (not just coincidentally absent from
	// the listing) -- a direct GET on the denied child is also forbidden.
	if w := doReq(r, http.MethodGet, "/dav"+mount+"/secret.txt", u.Username, "pw", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("direct GET of denied child: want 403, got %d", w.Code)
	}
}

// TestWebdavPutOversizeAbortsWithoutCommittingTruncatedObject pins Finding 3
// (W5 security review): once a PUT body exceeds maxWebdavPutBytes,
// writeFile.Write's ceiling error must mark the handle aborted so Close
// refuses to call fs.PutDirectly -- x/net/webdav's vendored handlePut always
// calls f.Close() after io.Copy regardless of the copy error, so Close is
// the only place left to veto committing a truncated object.
func TestWebdavPutOversizeAbortsWithoutCommittingTruncatedObject(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	setMaxPutBytesForTest(t, 8) // tiny ceiling; the body below exceeds it
	r := newDavRouter(t, gateID)

	target := mount + "/oversize.bin"
	body := bytes.Repeat([]byte("x"), 64)

	w := doReq(r, http.MethodPut, "/dav"+target, u.Username, "pw", body, nil)
	if w.Code == http.StatusCreated {
		t.Fatalf("oversize PUT must not report success (201), got body=%s", w.Body.String())
	}

	// No truncated object must have been committed at the destination.
	if w := doReq(r, http.MethodGet, "/dav"+target, u.Username, "pw", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("GET after oversize PUT: want 404 (nothing committed), got %d body=%q", w.Code, w.Body.String())
	}
}

// envelopeCode reads the {"code":...} envelope middlewares.RequireExtensionEnabled
// answers with via common.ErrorResp -- that helper always writes raw HTTP 200
// and carries the real status in the JSON body (the alist API-envelope
// convention every server/handles endpoint follows; extension_gate_test.go
// pins the same shape). /dav reuses that exact gate verbatim (design W5 §④),
// so a disabled builtin.webdav is observed here, not as a raw 403 status.
func envelopeCode(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v (body=%s)", err, w.Body.String())
	}
	return env.Code
}

// TestWebdavGateDisabled pins the kill-switch: disabling the builtin.*
// extension the route is gated behind rejects every /dav request with the
// gate's "feature disabled" envelope, regardless of valid credentials.
func TestWebdavGateDisabled(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	if w := doReq(r, "PROPFIND", "/dav"+mount, u.Username, "pw", nil, map[string]string{"Depth": "0"}); w.Code != http.StatusMultiStatus {
		t.Fatalf("sanity: gate must let an enabled feature's PROPFIND through, got %d body=%s", w.Code, w.Body.String())
	}

	if err := extension.DisableBuiltin(gateID); err != nil {
		t.Fatalf("disable gate: %v", err)
	}
	w := doReq(r, "PROPFIND", "/dav"+mount, u.Username, "pw", nil, map[string]string{"Depth": "0"})
	if got := envelopeCode(t, w); got != http.StatusForbidden {
		t.Fatalf("disabled gate: want envelope code 403, got %d body=%s", got, w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "feature disabled") {
		t.Fatalf("disabled gate body must say feature disabled, got %s", w.Body.String())
	}
}
