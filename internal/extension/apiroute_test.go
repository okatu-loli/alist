package extension

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

func TestCheckReservedPrefix(t *testing.T) {
	reserved := []string{
		"/api", "/dav", "/assets", "/@manage", "/plugin-assets", "/d", "/p", "/api/v4",
		// server/static/static.go's other static-file folders + the standalone
		// favicon.svg route: a real gin route wins over a plugin's NoRoute
		// prefix claim before it is ever consulted, so these must be reserved
		// too (review finding: previously missing, a plugin claiming one of
		// these would silently never be reachable).
		"/images", "/streamer", "/static", "/favicon.svg",
	}
	for _, p := range reserved {
		if err := CheckReservedPrefix(p); err == nil {
			t.Errorf("CheckReservedPrefix(%q): want conflict error, got nil", p)
		}
	}
	// Case/slash-insensitive: "/API/" still conflicts.
	if err := CheckReservedPrefix("/API/"); err == nil {
		t.Errorf("CheckReservedPrefix(/API/): want conflict, got nil")
	}
	// Non-reserved short prefixes are allowed.
	for _, p := range []string{"/s", "/share", "/webhook"} {
		if err := CheckReservedPrefix(p); err != nil {
			t.Errorf("CheckReservedPrefix(%q): want nil, got %v", p, err)
		}
	}
	// Malformed prefixes are rejected regardless of the reserved table.
	for _, p := range []string{"", "noslash", "/a/../b", "/.."} {
		if err := CheckReservedPrefix(p); err == nil {
			t.Errorf("CheckReservedPrefix(%q): want malformed error, got nil", p)
		}
	}
}

// stubRoute is an in-process APIRoute that echoes the sub-path and records the
// last request URL it saw.
type stubRoute struct {
	routes  []ext.RouteDecl
	lastURL string
}

func (s *stubRoute) Manifest() ext.Manifest               { return ext.Manifest{} }
func (s *stubRoute) Init(context.Context, ext.Host) error { return nil }
func (s *stubRoute) Shutdown(context.Context) error       { return nil }
func (s *stubRoute) Routes() []ext.RouteDecl              { return s.routes }
func (s *stubRoute) Handle(_ context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	s.lastURL = req.URL
	return &ext.HTTPResponse{Status: 201, Body: []byte("ok:" + req.URL)}, nil
}

func apiRouteEntry(id string, apis ...string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindAPIRoute},
			Permissions: ext.Permissions{HostAPIs: apis}},
		Kind:    ext.KindAPIRoute,
		enabled: true,
	}
}

func TestDispatchAPIRouteAuthenticated(t *testing.T) {
	r := NewRegistry()
	route := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}
	if err := r.Register(apiRouteEntry("api.wh")); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.wh", route)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.wh", nil) })

	resp, err := DispatchAPIRoute(context.Background(), r, "api.wh", "/webhook/hook", false,
		&ext.HTTPRequest{Method: "POST", URL: "/webhook/hook"})
	if err != nil {
		t.Fatalf("DispatchAPIRoute: %v", err)
	}
	if resp.Status != 201 || string(resp.Body) != "ok:/webhook/hook" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
}

func TestDispatchAPIRouteUndeclaredSubpathIsNotFound(t *testing.T) {
	r := NewRegistry()
	route := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}
	if err := r.Register(apiRouteEntry("api.wh2")); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.wh2", route)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.wh2", nil) })

	// A sub-path under no declared prefix is rejected (not forwarded).
	if _, err := DispatchAPIRoute(context.Background(), r, "api.wh2", "/secret", false,
		&ext.HTTPRequest{Method: "GET", URL: "/secret"}); err != ErrRouteNotDeclared {
		t.Fatalf("undeclared subpath: want ErrRouteNotDeclared, got %v", err)
	}
}

func TestDispatchAPIRoutePublicRequiresGrantAndPublicDecl(t *testing.T) {
	r := NewRegistry()
	// Route declares /s as PUBLIC but the plugin lacks the public_routes grant.
	ungranted := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/s", Public: true}}}
	if err := r.Register(apiRouteEntry("api.pub.nogrant")); err != nil { // no public_routes
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.pub.nogrant", ungranted)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.pub.nogrant", nil) })

	if _, err := DispatchAPIRoute(context.Background(), r, "api.pub.nogrant", "/s/abc", true,
		&ext.HTTPRequest{Method: "GET", URL: "/s/abc"}); err != ext.ErrPermissionDenied {
		t.Fatalf("public route without public_routes grant: want ErrPermissionDenied, got %v", err)
	}

	// Granted + declared public → dispatched.
	granted := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/s", Public: true}}}
	if err := r.Register(apiRouteEntry("api.pub.ok", CapPublicRoutes)); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.pub.ok", granted)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.pub.ok", nil) })

	resp, err := DispatchAPIRoute(context.Background(), r, "api.pub.ok", "/s/abc", true,
		&ext.HTTPRequest{Method: "GET", URL: "/s/abc"})
	if err != nil || resp.Status != 201 {
		t.Fatalf("granted public route: resp=%+v err=%v", resp, err)
	}
}

// TestResolveAPIRouteUsesFactory pins the gRPC-dispatch seam: an ExecGRPC
// entry with a binPath and no in-process instance must resolve through the
// installed apiRouteFactory (the plugins.go init wires this to
// pluginproc.Default.AcquireAPIRoute). Without a factory the same entry
// resolves to nil (no host-side handler). Mirrors
// TestResolveBackgroundServiceUsesFactory one Kind over.
func TestResolveAPIRouteUsesFactory(t *testing.T) {
	saved := apiRouteFactory
	t.Cleanup(func() { apiRouteFactory = saved })

	e := &Entry{
		Manifest: ext.Manifest{ID: "api.grpc", Kinds: []ext.Kind{ext.KindAPIRoute}},
		Kind:     ext.KindAPIRoute,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-apiroute",
	}

	// No factory wired → no host-side handler → nil.
	apiRouteFactory = nil
	if route, _ := resolveAPIRoute(e); route != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", route)
	}

	// Factory wired → the ExecGRPC branch is hit and its route is returned.
	stub := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}
	var released bool
	apiRouteFactory = func(got *Entry) (ext.APIRoute, func(), error) {
		if got.ID() != "api.grpc" || got.BinPath() != "/bin/example-apiroute" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	route, release := resolveAPIRoute(e)
	if route != stub {
		t.Fatalf("resolveAPIRoute must return the factory's route, got %T", route)
	}
	release()
	if !released {
		t.Fatal("resolveAPIRoute must return the factory's release func")
	}
}

// TestDispatchAPIRouteMultiKindManifestUsesManifestTruthSource mirrors
// TestInvokeActionMultiKindManifestUsesManifestTruthSource (actions_test.go):
// an entry registered under a different primary Kind (Kind=AccessProtocol)
// whose Manifest additionally declares APIRoute (Kinds: [AccessProtocol,
// APIRoute]) never gets an extraKinds(APIRoute) entry — extraKinds is
// populated solely by FeatureBundle indexing (featurebundle.go) — so
// e.HasKind(APIRoute) alone would wrongly 404 it. DeclaresKind (ORed into
// DispatchAPIRoute's gate) must still let it through.
func TestDispatchAPIRouteMultiKindManifestUsesManifestTruthSource(t *testing.T) {
	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{ID: "api.multikind", Kinds: []ext.Kind{ext.KindAccessProtocol, ext.KindAPIRoute}},
		Kind:     ext.KindAccessProtocol, // registered/indexed under its primary Kind
		enabled:  true,
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	route := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}
	RegisterAPIRouteInstance("api.multikind", route)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.multikind", nil) })

	resp, err := DispatchAPIRoute(context.Background(), r, "api.multikind", "/webhook/x", false,
		&ext.HTTPRequest{Method: "GET", URL: "/webhook/x"})
	if err != nil {
		t.Fatalf("DispatchAPIRoute to multi-Kind manifest: %v", err)
	}
	if resp.Status != 201 || string(resp.Body) != "ok:/webhook/x" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
}

// TestDispatchAPIRouteBundleExpansionStillReachableWithoutManifestDeclaration
// pins the OTHER leg of the DispatchAPIRoute gate: a FeatureBundle whose own
// Manifest.Kinds lists only FeatureBundle (NOT "api_route" — bundle authors
// are not required to redeclare a sub-Kind the host expands for them,
// TestExpandFeatureBundleWiresAPIRouteAndEventHook) must still dispatch
// through the extraKinds index HasKind sees. If the gate were narrowed to
// DeclaresKind alone (dropping the HasKind leg), this would regress to 404.
func TestDispatchAPIRouteBundleExpansionStillReachableWithoutManifestDeclaration(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bundleEntry("bundle.apiroute.only")); err != nil {
		t.Fatal(err)
	}
	bundle := &stubBundle{routes: []ext.RouteDecl{{Prefix: "/manage"}}}
	RegisterFeatureBundleInstance("bundle.apiroute.only", bundle)
	t.Cleanup(func() { CollapseFeatureBundle("bundle.apiroute.only") })

	if _, err := ExpandFeatureBundle(r, "bundle.apiroute.only"); err != nil {
		t.Fatalf("ExpandFeatureBundle: %v", err)
	}

	resp, err := DispatchAPIRoute(context.Background(), r, "bundle.apiroute.only", "/manage/x", false,
		&ext.HTTPRequest{Method: "GET", URL: "/manage/x"})
	if err != nil {
		t.Fatalf("DispatchAPIRoute to bundle (no manifest declaration): %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != "bundle:/manage/x" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
}

func TestDispatchAPIRouteAuthedRouteNotReachableAsPublic(t *testing.T) {
	r := NewRegistry()
	// /webhook is an AUTHENTICATED route; a request arriving on the public group
	// must not reach it (public dispatch only matches Public=true prefixes).
	route := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}
	if err := r.Register(apiRouteEntry("api.mix", CapPublicRoutes)); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.mix", route)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.mix", nil) })

	if _, err := DispatchAPIRoute(context.Background(), r, "api.mix", "/webhook/x", true,
		&ext.HTTPRequest{Method: "GET", URL: "/webhook/x"}); err != ErrRouteNotDeclared {
		t.Fatalf("authed route via public group: want ErrRouteNotDeclared, got %v", err)
	}
}
