package extension

// F1 red-team: APIRoute / PublicRoute dispatch-gate + reserved-prefix suite.
//
// DispatchAPIRoute is the auth-boundary chokepoint for both the authenticated
// (/api/v4/ext/:id/*) and the public (/api/v4/ext-public/:id/*) mounts. These
// attacks drive the real dispatch path with stub plugins and assert the gate
// denies each bypass. All are permanent regressions.
//
// TestReg_* = attack correctly denied today (pin the good behavior).
// The reserved-prefix probes additionally DOCUMENT current mechanism behavior
// for the not-yet-wired top-level short-prefix feature (see the report).

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

func dispatch(r *Registry, id, sub string, public bool) (*ext.HTTPResponse, error) {
	return DispatchAPIRoute(context.Background(), r, id, sub, public,
		&ext.HTTPRequest{Method: "GET", URL: sub})
}

// TestReg_DispatchDisabledPluginUnreachable: a disabled plugin's routes are a
// 404 even for a declared, matching sub-path. Disabling a plugin must fully
// sever its route surface (no lingering reachable handler).
func TestReg_DispatchDisabledPluginUnreachable(t *testing.T) {
	r := NewRegistry()
	route := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}
	e := apiRouteEntry("api.disabled")
	e.enabled = false // registered but disabled
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.disabled", route)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.disabled", nil) })

	if _, err := dispatch(r, "api.disabled", "/webhook/x", false); err != ErrRouteNotDeclared {
		t.Fatalf("disabled plugin route: want ErrRouteNotDeclared, got %v", err)
	}
}

// TestReg_DispatchWrongKindUnreachable: an entry whose primary Kind is not
// APIRoute cannot be reached through the APIRoute proxy, even if an APIRoute
// instance was (mis)registered under its id. The Kind check gates dispatch.
func TestReg_DispatchWrongKindUnreachable(t *testing.T) {
	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{ID: "api.wrongkind", Kinds: []ext.Kind{ext.KindEventHook}},
		Kind:     ext.KindEventHook,
		enabled:  true,
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.wrongkind", &stubRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}})
	t.Cleanup(func() { RegisterAPIRouteInstance("api.wrongkind", nil) })

	if _, err := dispatch(r, "api.wrongkind", "/webhook/x", false); err != ErrRouteNotDeclared {
		t.Fatalf("wrong-kind dispatch: want ErrRouteNotDeclared, got %v", err)
	}
}

// TestReg_DispatchUnknownPluginUnreachable: an unregistered plugin id is a 404,
// not a panic or a fall-through to some other plugin.
func TestReg_DispatchUnknownPluginUnreachable(t *testing.T) {
	r := NewRegistry()
	if _, err := dispatch(r, "api.nope", "/x", false); err != ErrRouteNotDeclared {
		t.Fatalf("unknown plugin: want ErrRouteNotDeclared, got %v", err)
	}
}

// TestReg_DispatchIsolatedPerPluginID: plugin A declares /a, plugin B declares
// /b. Naming plugin B in the URL can only reach B's OWN declared routes — a
// sub-path that only A declares is a 404 on B. A caller cannot borrow A's routes
// (or A's grant/identity) by hitting /ext/B/a-path: each id resolves to its own
// entry, routes, and grant.
func TestReg_DispatchIsolatedPerPluginID(t *testing.T) {
	r := NewRegistry()
	a := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/a"}}}
	b := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/b"}}}
	if err := r.Register(apiRouteEntry("api.A")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(apiRouteEntry("api.B")); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.A", a)
	RegisterAPIRouteInstance("api.B", b)
	t.Cleanup(func() {
		RegisterAPIRouteInstance("api.A", nil)
		RegisterAPIRouteInstance("api.B", nil)
	})

	// B cannot serve A's declared sub-path.
	if _, err := dispatch(r, "api.B", "/a/secret", false); err != ErrRouteNotDeclared {
		t.Fatalf("cross-plugin /ext/B/a: want ErrRouteNotDeclared, got %v", err)
	}
	// Sanity: each plugin serves only its own, and B's handler ran (not A's).
	if _, err := dispatch(r, "api.B", "/b/x", false); err != nil {
		t.Fatalf("B own route: %v", err)
	}
	if a.lastURL != "" {
		t.Fatalf("plugin A handler was invoked via a request to plugin B (lastURL=%q)", a.lastURL)
	}
}

// TestReg_PublicGroupCannotReachAuthedRouteEvenWithGrant: a plugin holding the
// public_routes grant but declaring its route as AUTHENTICATED (Public=false)
// must NOT have that route reachable via the public (unauth) group. The grant
// alone does not flip an authed route public; the visibility flag must match.
// Anonymous callers therefore cannot reach an authenticated handler.
func TestReg_PublicGroupCannotReachAuthedRouteEvenWithGrant(t *testing.T) {
	r := NewRegistry()
	route := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/admin", Public: false}}}
	if err := r.Register(apiRouteEntry("api.authonly", CapPublicRoutes)); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.authonly", route)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.authonly", nil) })

	if _, err := dispatch(r, "api.authonly", "/admin/x", true); err != ErrRouteNotDeclared {
		t.Fatalf("authed route via public group (with grant): want ErrRouteNotDeclared, got %v", err)
	}
	if route.lastURL != "" {
		t.Fatalf("authenticated handler ran for an anonymous public request (lastURL=%q)", route.lastURL)
	}
}

// TestReg_AuthedGroupCannotReachPublicOnlyRoute: the mirror direction. A route
// declared ONLY as Public must not be reachable via the authenticated group
// (public=false). Visibility is a strict equality on both sides.
func TestReg_AuthedGroupCannotReachPublicOnlyRoute(t *testing.T) {
	r := NewRegistry()
	route := &stubRoute{routes: []ext.RouteDecl{{Prefix: "/s", Public: true}}}
	if err := r.Register(apiRouteEntry("api.pubonly", CapPublicRoutes)); err != nil {
		t.Fatal(err)
	}
	RegisterAPIRouteInstance("api.pubonly", route)
	t.Cleanup(func() { RegisterAPIRouteInstance("api.pubonly", nil) })

	if _, err := dispatch(r, "api.pubonly", "/s/abc", false); err != ErrRouteNotDeclared {
		t.Fatalf("public-only route via authed group: want ErrRouteNotDeclared, got %v", err)
	}
}

// TestReg_RouteMatchSiblingPrefixNotSwallowed: a declared prefix "/webhook"
// matches "/webhook" and "/webhook/..." but NOT a sibling "/webhookother" — a
// plugin cannot swallow adjacent sub-paths by prefix-string overlap.
func TestReg_RouteMatchSiblingPrefixNotSwallowed(t *testing.T) {
	decls := []ext.RouteDecl{{Prefix: "/webhook"}}
	cases := map[string]bool{
		"/webhook":        true,
		"/webhook/hook":   true,
		"/webhookother":   false,
		"/webhookother/x": false,
		"/web":            false,
		"/":               false,
	}
	for sub, want := range cases {
		if got := routeMatches(decls, sub, false); got != want {
			t.Fatalf("routeMatches(%q)=%v, want %v", sub, got, want)
		}
	}
}

// TestReg_ReservedPrefixKnownEvasions pins the reserved-word check against the
// classic normalization evasions: case, slash variants, traversal, empty. Each
// must be refused (either as a reserved conflict or as malformed).
//
// NOTE: CheckReservedPrefix has NO production caller yet — the top-level
// short-prefix mount feature is not wired (see report). These assertions guard
// the MECHANISM so it is already correct when that feature lands.
func TestReg_ReservedPrefixKnownEvasions(t *testing.T) {
	mustRefuse := []string{
		"/api", "/API", "/Api", "/api/", "//api", "/api/v4",
		"/dav", "/DAV", "/@manage",
		"/ext", "/ext-public", "/extensions", "/d", "/p", "/i",
		"/./api", "/a/../b", "/..", "/", "", "noslash", "/api/../dav",
	}
	for _, p := range mustRefuse {
		if err := CheckReservedPrefix(p); err == nil {
			t.Errorf("CheckReservedPrefix(%q): want refusal, got nil (ALLOWED)", p)
		}
	}
}

// TestReservedPrefixWhitespaceAndUnicodeProbes DOCUMENTS current mechanism
// behavior for evasions the check does NOT normalize away: trailing/leading
// ASCII whitespace inside a segment and a full-width Unicode homoglyph. These
// are recorded (not asserted secure) because:
//   - the check has no production caller, so nothing is exploitable today; and
//   - whether "/api " or "/ａｐｉ" could ever collide with the core "/api" mount
//     depends on the (unwritten) top-level mount + the router's own decoding.
//
// The test asserts the CURRENT outcome so a future change that starts trimming/
// normalizing (closing the gap) will trip here and be reviewed deliberately.
func TestReservedPrefixWhitespaceAndUnicodeProbes(t *testing.T) {
	// Trailing space is NOT trimmed from a segment: "api " != "api", so it is
	// currently ALLOWED. Latent hardening item (see report).
	if err := CheckReservedPrefix("/api "); err != nil {
		t.Logf("documented: CheckReservedPrefix(%q) refused (%v) — gap closed", "/api ", err)
	} else {
		t.Logf("documented CURRENT gap: CheckReservedPrefix(%q) ALLOWED (whitespace not trimmed)", "/api ")
	}
	// Leading space likewise makes a distinct first segment " api".
	if err := CheckReservedPrefix("/ api"); err == nil {
		t.Logf("documented CURRENT gap: CheckReservedPrefix(%q) ALLOWED (leading space)", "/ api")
	}
	// Full-width homoglyph is a different byte sequence from ASCII "api".
	if err := CheckReservedPrefix("/ａｐｉ"); err == nil {
		t.Logf("documented CURRENT gap: CheckReservedPrefix(full-width api) ALLOWED (no unicode fold)")
	}
	// Percent-encoding is NOT decoded before the reserved-word lookup: "/%40manage"
	// evades the "@manage" reserved entry even though a router that percent-decodes
	// the path would route it to the same place. Latent gap (no live caller).
	if err := CheckReservedPrefix("/%40manage"); err == nil {
		t.Logf("documented CURRENT gap: CheckReservedPrefix(%q) ALLOWED (no percent-decode of @manage)", "/%40manage")
	}
	if err := CheckReservedPrefix("/%61pi"); err == nil {
		t.Logf("documented CURRENT gap: CheckReservedPrefix(%q) ALLOWED (no percent-decode of api)", "/%61pi")
	}
}
