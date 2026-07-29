package handles

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// End-to-end gate tests for the core-feature built-in extensions (Project A-2):
// the RequireExtensionEnabled middleware is the real boundary, so a disabled
// feature returns a 403 "feature disabled" BEFORE the handler runs, and enabling
// it restores the original behavior. Each mounts the same middleware+handler the
// v4 router uses, driving through a real gin engine so the gate placement is
// exercised exactly as wired.

// registerFeature registers a Kind=Feature built-in into the process-wide
// DefaultRegistry (what the middleware reads) and returns it to enabled + removed
// on cleanup, so tests can toggle it without leaking state across the package.
func registerFeature(t *testing.T, id string) {
	t.Helper()
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID: id, Version: "builtin", Kinds: []ext.Kind{ext.KindFeature},
	}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(id) })
}

// setUser is a fake auth middleware that plants u in context so a gated authed
// handler sees a caller (the real Auth middleware is out of scope here).
func setUser(u *model.User) gin.HandlerFunc {
	return func(c *gin.Context) { c.Set(ctxkey.User, u); c.Next() }
}

func isFeatureDisabled(w *httptest.ResponseRecorder) bool {
	return strings.Contains(strings.ToLower(w.Body.String()), "feature disabled")
}

// TestSearchFeatureGate: disabling builtin.search makes GET /fs/search 403
// "feature disabled"; enabling it restores the original 200 + hit behavior.
func TestSearchFeatureGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registerFeature(t, "builtin.search")
	u, mount := setupSearchFixture(t)

	r := gin.New()
	r.GET("/fs/search", setUser(u),
		middlewares.RequireExtensionEnabled("builtin.search"), FsSearch)

	hit := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		q := url.Values{"parent": {mount}, "keyword": {"alpha"}}
		r.ServeHTTP(w, httptest.NewRequest("GET", "/fs/search?"+q.Encode(), nil))
		return w
	}

	if err := extension.DisableBuiltin("builtin.search"); err != nil {
		t.Fatal(err)
	}
	w := hit()
	if got := respCode(t, w); got != 403 || !isFeatureDisabled(w) {
		t.Fatalf("disabled search must 403 feature-disabled, got %d: %s", got, w.Body.String())
	}

	if err := extension.EnableBuiltin("builtin.search"); err != nil {
		t.Fatal(err)
	}
	w = hit()
	if got := respCode(t, w); got != 200 {
		t.Fatalf("enabled search must 200, got %d: %s", got, w.Body.String())
	}
	if isFeatureDisabled(w) {
		t.Fatalf("enabled search must not be gate-blocked: %s", w.Body.String())
	}
}

// TestOfflineDownloadFeatureGate: disabling builtin.offline_download makes
// POST /fs/add_offline_download 403 "feature disabled"; enabling it lets the
// request past the gate into the handler (which then does its own authz).
func TestOfflineDownloadFeatureGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registerFeature(t, "builtin.offline_download")
	ensureOfflineDownloadManager(t)
	mount := setupOfflineFixture(t)
	// A caller with no role: enough to prove the gate runs before the handler.
	u := &model.User{ID: 90001, Username: "offgateuser"}

	r := gin.New()
	r.POST("/fs/add_offline_download", setUser(u),
		middlewares.RequireExtensionEnabled("builtin.offline_download"), AddOfflineDownload)

	body := `{"path":"` + mount + `/dst","urls":["http://example.com/x"],"tool":"SimpleHttp"}`
	hit := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/fs/add_offline_download", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	if err := extension.DisableBuiltin("builtin.offline_download"); err != nil {
		t.Fatal(err)
	}
	w := hit()
	if got := respCode(t, w); got != 403 || !isFeatureDisabled(w) {
		t.Fatalf("disabled offline must 403 feature-disabled, got %d: %s", got, w.Body.String())
	}

	if err := extension.EnableBuiltin("builtin.offline_download"); err != nil {
		t.Fatal(err)
	}
	// Enabled: the gate passes; whatever the handler answers, it must NOT be the
	// gate's "feature disabled".
	w = hit()
	if isFeatureDisabled(w) {
		t.Fatalf("enabled offline must clear the gate, got %s", w.Body.String())
	}
}

// TestSharePublicFeatureGate: the public /s surface is gated on builtin.share,
// so a disabled feature makes even a valid token 403 "feature disabled" (old
// links stop resolving); enabling it restores the 200 info response.
func TestSharePublicFeatureGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registerFeature(t, "builtin.share")
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	sg := r.Group("/s", middlewares.RequireExtensionEnabled("builtin.share"))
	sg.GET("/:token", ShareInfo)

	hit := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/s/"+s.Token, nil))
		return w
	}

	if err := extension.DisableBuiltin("builtin.share"); err != nil {
		t.Fatal(err)
	}
	w := hit()
	if got := respCode(t, w); got != 403 || !isFeatureDisabled(w) {
		t.Fatalf("disabled share must 403 feature-disabled on a valid token, got %d: %s", got, w.Body.String())
	}

	if err := extension.EnableBuiltin("builtin.share"); err != nil {
		t.Fatal(err)
	}
	w = hit()
	if got := respCode(t, w); got != 200 {
		t.Fatalf("enabled share must 200 on a valid token, got %d: %s", got, w.Body.String())
	}
	if isFeatureDisabled(w) {
		t.Fatalf("enabled share must not be gate-blocked: %s", w.Body.String())
	}
}
