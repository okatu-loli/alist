package extension

import (
	"context"
	"fmt"
	"strings"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// reservedPrefixes are the top-level URL prefixes a plugin may NOT claim for a
// public short-link route (design §3 PublicRouter reserved-word table). A plugin
// asking for one of these at install/registration is refused. The plugin
// namespace mount (/api/v4/ext/{id}/…) never needs this list — it cannot collide
// with a core route — but a plugin that requests a top-level short prefix (a
// share link like /s/:id) is checked here.
//
// The set covers every real top-level segment mounted directly on the base-
// path group `g` in server/router.go + apiV4 (server/router_v4.go, which
// mounts everything under "api" — none of its inner groups add a second
// top-level segment): the API tree, the download/proxy/plist short paths, the
// WebDAV mount, the frontend admin/asset roots, the plugin asset root, and —
// since this list gates a NoRoute-dispatched prefix and gin's static file
// routes (server/static/static.go) win any real request before NoRoute is
// ever reached — the static asset folders and the standalone favicon.svg file
// too (a plugin claiming one of those would previously have silently lost
// every request to the static mount, since a plugin's claim only matters once
// nothing else answers first).
var reservedPrefixes = map[string]bool{
	"api":           true,
	"dav":           true,
	"assets":        true, // static.go folders: assets/images/streamer/static
	"images":        true,
	"streamer":      true,
	"static":        true,
	"favicon.svg":   true, // static.go's standalone top-level file route
	"@manage":       true,
	"plugin-assets": true,
	"ext":           true, // authenticated plugin route namespace
	"ext-public":    true, // public plugin route namespace
	"extensions":    true, // extension contributions / ui / wizard group
	"d":             true, // /d/*path download
	"p":             true, // /p/*path proxy
	"i":             true, // /i/:link_name plist
	"ping":          true,
	"favicon.ico":   true,
	"robots.txt":    true,
	"debug":         true,
}

// CheckReservedPrefix validates a top-level route prefix a plugin wants to claim
// (a public short-link prefix). It returns an error when the prefix is malformed
// (empty, not rooted, or traversing) or its first segment collides with a
// reserved core prefix. Matching is case-insensitive on the first segment, so
// "/API" is refused as readily as "/api". This is the install/registration-time
// conflict check the design (§3, §4) requires; the caller rejects the install on
// error.
func CheckReservedPrefix(prefix string) error {
	seg, err := normalizePrefix(prefix)
	if err != nil {
		return err
	}
	first := seg[0]
	if reservedPrefixes[strings.ToLower(first)] {
		return fmt.Errorf("extension: route prefix %q conflicts with reserved prefix %q", prefix, "/"+first)
	}
	return nil
}

// normalizePrefix cleans and validates a route prefix, returning its non-empty
// path segments. It rejects an empty prefix, one not starting with "/", and any
// prefix whose cleaned form escapes root (a "../" traversal) — a route prefix is
// a rooted, non-traversing path.
func normalizePrefix(prefix string) ([]string, error) {
	if prefix == "" || !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("extension: route prefix %q must be rooted (start with /)", prefix)
	}
	// Split the RAW path and validate each literal segment: a route prefix is a
	// set of literal segments, so an empty (//), "." or ".." segment is rejected
	// outright rather than being silently collapsed by path.Clean (which would
	// let "/a/../b" masquerade as "/b").
	var segs []string
	for _, s := range strings.Split(strings.Trim(prefix, "/"), "/") {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("extension: route prefix %q has an empty or traversing segment", prefix)
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("extension: route prefix %q has no segments", prefix)
	}
	return segs, nil
}

// ErrRouteNotDeclared is returned when a request's sub-path does not fall under
// any route the plugin declared with a matching public/authenticated visibility.
// Callers map it to 404: an undeclared route is indistinguishable from a missing
// one.
var ErrRouteNotDeclared = fmt.Errorf("extension: route not declared")

// apiRouteFactory is the gRPC-dispatch seam for APIRoute (mirrors
// eventHookFactory). nil until a gRPC bridge lands; in-process instances are the
// tested F1 path.
var apiRouteFactory func(e *Entry) (ext.APIRoute, func(), error)

// SetAPIRouteFactory installs the gRPC APIRoute provider.
func SetAPIRouteFactory(fn func(e *Entry) (ext.APIRoute, func(), error)) {
	apiRouteFactory = fn
}

var (
	apiRouteMu        sync.RWMutex
	apiRouteInstances = map[string]ext.APIRoute{}
)

// RegisterAPIRouteInstance registers an in-process ext.APIRoute under id (a
// built-in feature bundle or a test double). A nil value deregisters.
func RegisterAPIRouteInstance(id string, a ext.APIRoute) {
	apiRouteMu.Lock()
	defer apiRouteMu.Unlock()
	if a == nil {
		delete(apiRouteInstances, id)
		return
	}
	apiRouteInstances[id] = a
}

func resolveAPIRoute(e *Entry) (ext.APIRoute, func()) {
	apiRouteMu.RLock()
	if a, ok := apiRouteInstances[e.Manifest.ID]; ok {
		apiRouteMu.RUnlock()
		return a, func() {}
	}
	apiRouteMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && apiRouteFactory != nil {
		if a, release, err := apiRouteFactory(e); err == nil {
			return a, release
		}
	}
	return nil, func() {}
}

// DispatchAPIRoute forwards a proxied request to an enabled Kind=APIRoute
// plugin, enforcing the F1 route gates before the plugin's Handle runs:
//
//   - enablement: only an enabled plugin's routes are reachable;
//   - visibility match: subpath must fall under a route the plugin declared with
//     the SAME public flag as the group the request arrived on. An authenticated
//     route is therefore unreachable via the public group and vice-versa
//     (ErrRouteNotDeclared → 404);
//   - public grant: a public dispatch additionally requires the plugin to hold
//     the "public_routes" grant (design §6.1), else ErrPermissionDenied.
//
// The plugin then borrows host capabilities through the F0 layer, still
// Grant-gated + Authorize-enforced; a public route's Handle is expected to run
// with a delegated-only Host (hostcap Options.DelegatedOnly) so it cannot reach
// the plugin's own fs grant.
func DispatchAPIRoute(ctx context.Context, r *Registry, pluginID, subpath string, public bool, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	e, ok := r.Get(pluginID)
	// Unlike AccessProtocol/BackgroundService/FeatureBundle above, APIRoute
	// DOES have a legitimate FeatureBundle-expansion path: ExpandFeatureBundle
	// AddKindIndex's a bundle onto KindAPIRoute purely from an ext.APIRoute
	// interface assertion (featurebundle.go), with no obligation for the
	// bundle's own Manifest.Kinds to list "api_route" too (see
	// TestExpandFeatureBundleWiresAPIRouteAndEventHook). So e.HasKind must stay
	// in the gate to keep that reachable. But it must not be the ONLY check:
	// a plain (non-bundle) plugin that legitimately declares APIRoute
	// alongside another primary Kind (Kinds: ["access_protocol",
	// "api_route"]) never gets an extraKinds entry either (extraKinds is
	// populated solely by FeatureBundle indexing) and would be wrongly
	// 404'd by HasKind alone — DeclaresKind (Manifest truth source) is
	// ORed in to cover that case too.
	if !ok || !e.Enabled() || !(e.HasKind(ext.KindAPIRoute) || e.DeclaresKind(ext.KindAPIRoute)) {
		return nil, ErrRouteNotDeclared
	}
	if public && !e.GrantsCapability(CapPublicRoutes) {
		return nil, ext.ErrPermissionDenied
	}
	route, release := resolveAPIRoute(e)
	if route == nil {
		return nil, ext.NewError(ext.ErrCodeNotImplement,
			fmt.Sprintf("APIRoute %q has no host-side handler wired", pluginID))
	}
	defer release()
	if !routeMatches(route.Routes(), subpath, public) {
		return nil, ErrRouteNotDeclared
	}
	return route.Handle(ctx, req)
}

// routeMatches reports whether subpath falls under a declared route whose Public
// flag equals public. A prefix "/webhook" matches "/webhook" and "/webhook/…"
// but not "/webhookother".
func routeMatches(decls []ext.RouteDecl, subpath string, public bool) bool {
	segs, err := normalizePrefix(subpath)
	if err != nil {
		return false
	}
	sp := "/" + strings.Join(segs, "/")
	for _, d := range decls {
		if d.Public != public {
			continue
		}
		ps, err := normalizePrefix(d.Prefix)
		if err != nil {
			continue
		}
		pfx := "/" + strings.Join(ps, "/")
		if sp == pfx || strings.HasPrefix(sp, pfx+"/") {
			return true
		}
	}
	return false
}
