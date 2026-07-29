package handles

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// echoRoute is an in-process APIRoute that echoes its sub-path.
type echoRoute struct{ routes []ext.RouteDecl }

func (e echoRoute) Manifest() ext.Manifest               { return ext.Manifest{} }
func (e echoRoute) Init(context.Context, ext.Host) error { return nil }
func (e echoRoute) Shutdown(context.Context) error       { return nil }
func (e echoRoute) Routes() []ext.RouteDecl              { return e.routes }
func (e echoRoute) Handle(_ context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{Status: 200, Body: []byte("served:" + req.URL)}, nil
}

func registerRoutePlugin(t *testing.T, id string, route ext.APIRoute, apis ...string) {
	t.Helper()
	e := &extension.Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindAPIRoute},
			Permissions: ext.Permissions{HostAPIs: apis}},
		Kind: ext.KindAPIRoute,
	}
	if err := extension.DefaultRegistry.Register(e); err != nil {
		t.Fatal(err)
	}
	if err := extension.DefaultRegistry.Enable(id); err != nil {
		t.Fatal(err)
	}
	extension.RegisterAPIRouteInstance(id, route)
	t.Cleanup(func() {
		extension.RegisterAPIRouteInstance(id, nil)
		extension.DefaultRegistry.Deregister(id)
	})
}

func callExtRoute(handler gin.HandlerFunc, pluginID, subpath, method string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "pluginid", Value: pluginID}, {Key: "proxypath", Value: subpath}}
	c.Request = httptest.NewRequest(method, "/x", nil)
	handler(c)
	return w
}

func TestExtRouteAuthenticatedDispatch(t *testing.T) {
	registerRoutePlugin(t, "h.api.wh", echoRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}})

	w := callExtRoute(ExtRoute, "h.api.wh", "/webhook/hook", "POST")
	if w.Code != 200 || w.Body.String() != "served:/webhook/hook" {
		t.Fatalf("authed route: code=%d body=%q", w.Code, w.Body.String())
	}

	// Undeclared sub-path is a 404, never forwarded.
	w = callExtRoute(ExtRoute, "h.api.wh", "/secret", "GET")
	if w.Code != 404 {
		t.Fatalf("undeclared subpath: want 404, got %d", w.Code)
	}
}

func TestExtPublicRouteAnonymousReachAndGrantGate(t *testing.T) {
	// Public route WITHOUT the public_routes grant → 403 even though the mount is
	// anonymous. The public group is not a backdoor around the grant.
	registerRoutePlugin(t, "h.api.pub.nogrant", echoRoute{routes: []ext.RouteDecl{{Prefix: "/s", Public: true}}})
	w := callExtRoute(ExtPublicRoute, "h.api.pub.nogrant", "/s/abc", "GET")
	if w.Code != 403 {
		t.Fatalf("public route without grant: want 403, got %d", w.Code)
	}

	// Public route WITH the grant → anonymously reachable (no auth middleware ran).
	registerRoutePlugin(t, "h.api.pub.ok", echoRoute{routes: []ext.RouteDecl{{Prefix: "/s", Public: true}}}, extension.CapPublicRoutes)
	w = callExtRoute(ExtPublicRoute, "h.api.pub.ok", "/s/abc", "GET")
	if w.Code != 200 || w.Body.String() != "served:/s/abc" {
		t.Fatalf("granted public route: code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestExtPublicRouteCannotReachAuthenticatedRoute(t *testing.T) {
	// A plugin exposes /webhook as an AUTHENTICATED route and holds public_routes.
	// The public mount must still refuse it (visibility mismatch → 404): a public
	// caller cannot reach an authenticated route by hitting the public group.
	registerRoutePlugin(t, "h.api.mix", echoRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}, extension.CapPublicRoutes)
	w := callExtRoute(ExtPublicRoute, "h.api.mix", "/webhook/x", "GET")
	if w.Code != 404 {
		t.Fatalf("authed route via public mount: want 404, got %d", w.Code)
	}
}

// registerPrefixRoutePlugin registers+enables an APIRoute plugin whose
// manifest requests the given public_prefix (claiming it via Register), so
// ExtPublicPrefixDispatch's NoRoute-chain dispatch can be exercised end to
// end (real gin engine + NoRoute, not a hand-built gin.Context).
func registerPrefixRoutePlugin(t *testing.T, id, prefix string, route ext.APIRoute, apis ...string) {
	t.Helper()
	e := &extension.Entry{
		Manifest: ext.Manifest{
			ID:            id,
			Kinds:         []ext.Kind{ext.KindAPIRoute},
			Permissions:   ext.Permissions{HostAPIs: apis},
			Contributions: ext.Contributions{PublicPrefix: prefix},
		},
		Kind: ext.KindAPIRoute,
	}
	if err := extension.DefaultRegistry.Register(e); err != nil {
		t.Fatal(err)
	}
	if err := extension.DefaultRegistry.Enable(id); err != nil {
		t.Fatal(err)
	}
	extension.RegisterAPIRouteInstance(id, route)
	t.Cleanup(func() {
		extension.RegisterAPIRouteInstance(id, nil)
		extension.DefaultRegistry.Deregister(id) // also releases the prefix claim
	})
}

// noRouteEngine builds a gin engine whose NoRoute chain mirrors production
// (server/router.go): ExtPublicPrefixDispatch first, then a stand-in SPA
// fallback that records whether it ran and what path it saw.
func noRouteEngine(spaHit *bool) *gin.Engine {
	engine := gin.New()
	engine.NoRoute(ExtPublicPrefixDispatch, func(c *gin.Context) {
		*spaHit = true
		c.String(200, "spa-fallback:"+c.Request.URL.Path)
	})
	return engine
}

// TestExtPublicPrefixDispatchHit proves the core wiring: a request whose
// top-level segment matches a claimed public_prefix is proxied to that
// plugin's public route, and the SPA fallback chained after
// ExtPublicPrefixDispatch in NoRoute must NOT also run (no double response).
func TestExtPublicPrefixDispatchHit(t *testing.T) {
	registerPrefixRoutePlugin(t, "h.api.prefix.hit", "/s",
		echoRoute{routes: []ext.RouteDecl{{Prefix: "/share", Public: true}}}, extension.CapPublicRoutes)

	var spaHit bool
	engine := noRouteEngine(&spaHit)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s/share/abc123", nil)
	engine.ServeHTTP(w, req)

	if w.Code != 200 || w.Body.String() != "served:/share/abc123" {
		t.Fatalf("prefix dispatch: code=%d body=%q", w.Code, w.Body.String())
	}
	if spaHit {
		t.Fatal("SPA fallback must not run on a public_prefix hit")
	}
}

// TestExtPublicPrefixDispatchMissFallsBackToSPA is the hard SPA-fallback
// constraint: any path whose top-level segment is NOT a claimed prefix must
// behave exactly as if ExtPublicPrefixDispatch were never chained in — the
// SPA fallback handler runs and answers.
func TestExtPublicPrefixDispatchMissFallsBackToSPA(t *testing.T) {
	// No plugin claims any prefix here (a fresh, unrelated path).
	var spaHit bool
	engine := noRouteEngine(&spaHit)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/some/unrelated/spa/route", nil)
	engine.ServeHTTP(w, req)

	if !spaHit {
		t.Fatal("SPA fallback must run when no prefix claims the request")
	}
	if w.Code != 200 || w.Body.String() != "spa-fallback:/some/unrelated/spa/route" {
		t.Fatalf("SPA fallback: code=%d body=%q", w.Code, w.Body.String())
	}
}

// TestExtPublicPrefixDispatchDisabledPluginFallsBack covers a claim that
// outlives a soft-disable (Registry.Disable only flips e.enabled — see its
// doc comment — it does not release the prefix): a disabled claimant must
// not intercept the request; it must fall back to the SPA handler exactly
// like a genuine miss.
func TestExtPublicPrefixDispatchDisabledPluginFallsBack(t *testing.T) {
	registerPrefixRoutePlugin(t, "h.api.prefix.disabled", "/s2",
		echoRoute{routes: []ext.RouteDecl{{Prefix: "/share", Public: true}}}, extension.CapPublicRoutes)
	if err := extension.DefaultRegistry.Disable("h.api.prefix.disabled"); err != nil {
		t.Fatal(err)
	}

	var spaHit bool
	engine := noRouteEngine(&spaHit)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s2/share/abc", nil)
	engine.ServeHTTP(w, req)

	if !spaHit {
		t.Fatal("a disabled claimant must fall back to the SPA handler")
	}
	if w.Code != 200 || w.Body.String() != "spa-fallback:/s2/share/abc" {
		t.Fatalf("SPA fallback for disabled claimant: code=%d body=%q", w.Code, w.Body.String())
	}
}

// withBasePath sets conf.URL to a parsed base path for the duration of the
// test (conf.URL is a process-global normally set once by
// bootstrap.initURL()), restoring the prior value on cleanup so this does not
// leak into other tests in the package.
func withBasePath(t *testing.T, path string) {
	t.Helper()
	prev := conf.URL
	conf.URL = &url.URL{Path: path}
	t.Cleanup(func() { conf.URL = prev })
}

// TestExtPublicPrefixDispatchHonorsBasePath is the base_path regression
// (review finding): server/router.go mounts every route under
// e.Group(conf.URL.Path), but gin's NoRoute chain runs at the ENGINE level, so
// the raw path it sees under a non-root base_path deployment still carries
// that prefix (e.g. "/alist/s/xyz", not "/s/xyz"). Without stripping it first,
// ExtPublicPrefixDispatch would look up "alist" (the base path's own segment)
// against the claim table instead of "s", so public_prefix dispatch would
// silently never match under any non-root base_path deployment.
func TestExtPublicPrefixDispatchHonorsBasePath(t *testing.T) {
	withBasePath(t, "/alist")
	registerPrefixRoutePlugin(t, "h.api.prefix.basepath", "/s",
		echoRoute{routes: []ext.RouteDecl{{Prefix: "/share", Public: true}}}, extension.CapPublicRoutes)

	var spaHit bool
	engine := noRouteEngine(&spaHit)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/alist/s/share/abc123", nil)
	engine.ServeHTTP(w, req)

	if w.Code != 200 || w.Body.String() != "served:/share/abc123" {
		t.Fatalf("prefix dispatch under base_path: code=%d body=%q", w.Code, w.Body.String())
	}
	if spaHit {
		t.Fatal("SPA fallback must not run on a base_path-aware prefix hit")
	}

	// A request that merely SHARES a prefix with the base path but diverges at
	// the next segment ("/alistx/...") is outside the mounted root and must
	// keep falling back to the SPA handler, not be misparsed as claim "x".
	spaHit = false
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/alistx/s/share/abc123", nil)
	engine.ServeHTTP(w, req)
	if !spaHit {
		t.Fatal("a path merely sharing base_path's prefix must fall back to SPA, not dispatch")
	}
}

// TestExtPublicPrefixDispatchReleasedAfterDeregisterFallsBack covers the
// uninstall/deregister release path: after Deregister, the prefix is free and
// a request under it must fall back to the SPA handler (a genuine miss).
func TestExtPublicPrefixDispatchReleasedAfterDeregisterFallsBack(t *testing.T) {
	id := "h.api.prefix.gone"
	e := &extension.Entry{
		Manifest: ext.Manifest{
			ID:            id,
			Kinds:         []ext.Kind{ext.KindAPIRoute},
			Contributions: ext.Contributions{PublicPrefix: "/s3"},
		},
		Kind: ext.KindAPIRoute,
	}
	if err := extension.DefaultRegistry.Register(e); err != nil {
		t.Fatal(err)
	}
	extension.DefaultRegistry.Deregister(id)

	var spaHit bool
	engine := noRouteEngine(&spaHit)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s3/whatever", nil)
	engine.ServeHTTP(w, req)

	if !spaHit || w.Body.String() != "spa-fallback:/s3/whatever" {
		t.Fatalf("released prefix must fall back to SPA: spaHit=%v body=%q", spaHit, w.Body.String())
	}
}
