package handles

import (
	"errors"
	"io"
	"net/http"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// maxExtRouteBody caps the request body forwarded to a plugin APIRoute handler.
// Plugin routes are webhooks / management APIs / share pages, not bulk upload
// endpoints (those go through the fs upload path), so a modest cap keeps a
// single proxied request from buffering an unbounded body into memory.
const maxExtRouteBody = 8 << 20 // 8 MiB

// ExtRoute proxies an AUTHENTICATED plugin API route
// (/api/v4/ext/:pluginid/*proxypath). It is mounted behind the auth middleware,
// so only a logged-in caller reaches a plugin's Handle. The plugin's declared
// route prefix + enablement are checked in extension.DispatchAPIRoute; a
// sub-path under no declared (authenticated) prefix is a 404.
//
// This is a RAW proxy, not the JSON envelope: the plugin's HTTPResponse
// (status/headers/body) is written back verbatim, the way /d and /p stream a
// download — a plugin route may serve JSON, a redirect, or bytes.
// @Summary Proxy an authenticated plugin API route
// @Tags extension
// @Security ApiKeyAuth
// @Param pluginid path string true "extension id"
// @Param proxypath path string true "sub-path below the plugin mount"
// @Router /v4/ext/{pluginid}/{proxypath} [get]
func ExtRoute(c *gin.Context) {
	proxyExtRoute(c, false)
}

// ExtPublicRoute proxies a PUBLIC plugin route
// (/api/v4/ext-public/:pluginid/*proxypath). It is mounted OUTSIDE the auth
// middleware (anonymous share landing pages, design §2.5). Reaching a plugin
// here additionally requires the plugin to hold the "public_routes" grant and to
// have declared the sub-path as a Public route; the plugin's Handle is expected
// to run with a delegated-only Host so it cannot borrow the plugin's own fs
// grant (hostcap Options.DelegatedOnly). Same reserved-prefix + namespace
// isolation as the authenticated mount.
// @Summary Proxy a public (unauthenticated) plugin route
// @Tags extension
// @Param pluginid path string true "extension id"
// @Param proxypath path string true "sub-path below the plugin public mount"
// @Router /v4/ext-public/{pluginid}/{proxypath} [get]
func ExtPublicRoute(c *gin.Context) {
	proxyExtRoute(c, true)
}

func proxyExtRoute(c *gin.Context, public bool) {
	subpath := c.Param("proxypath")
	if subpath == "" {
		subpath = "/"
	}
	dispatchExtRoute(c, c.Param("pluginid"), subpath, public)
}

// dispatchExtRoute is the shared body of ExtRoute/ExtPublicRoute/
// ExtPublicPrefixDispatch: read the (size-capped) body, strip alist's own
// auth material, forward to extension.DispatchAPIRoute, and write the raw
// response back. It always leaves c aborted on return (success or error) —
// callers reached from a NoRoute chain (ExtPublicPrefixDispatch) rely on this
// to stop the SPA fallback handler chained after it from ALSO running and
// double-writing the response; it is a harmless no-op for ExtRoute/
// ExtPublicRoute, which are already terminal handlers on their own route.
func dispatchExtRoute(c *gin.Context, pluginID, subpath string, public bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxExtRouteBody))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	// Strip alist's OWN authentication material before handing the request to
	// untrusted plugin code. Forwarding the caller's `Authorization: Bearer
	// <session token>` (auth.go reads the token from this header) or their
	// `Cookie` would let the plugin lift the live credential and replay it against
	// the core API as the user — an escalation past every host-capability gate
	// (host.FS is Authorize-scoped, host.HTTP allowlisted, Store namespaced), so
	// it must never leave the host. A plugin that legitimately needs the caller's
	// identity gets it out-of-band (a host-minted, non-replayable handle), not the
	// raw token. (Reported by the F1 red team.)
	header := c.Request.Header.Clone()
	header.Del("Authorization")
	header.Del("Cookie")
	header.Del("X-Api-Key")
	req := &ext.HTTPRequest{
		Method: c.Request.Method,
		URL:    subpath,
		Header: header,
		Body:   body,
	}
	resp, err := extension.DispatchAPIRoute(c.Request.Context(),
		extension.DefaultRegistry, pluginID, subpath, public, req)
	if err != nil {
		writeExtRouteError(c, err)
		return
	}
	for k, vals := range resp.Header {
		for _, v := range vals {
			c.Writer.Header().Add(k, v)
		}
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	c.Writer.WriteHeader(status)
	_, _ = c.Writer.Write(resp.Body)
	c.Abort()
}

// ExtPublicPrefixDispatch resolves a top-level short public_prefix (design §3
// PublicRouter, Contributions.PublicPrefix) against the registry's claim
// table and dispatches to the claiming plugin's public route, exactly like
// ExtPublicRoute (same DispatchAPIRoute call, same enablement/route-match/
// public_routes-grant gates) — the only difference is HOW the plugin id is
// found: a claimed top-level segment instead of the :pluginid path param.
//
// It is meant to be chained FIRST in gin's NoRoute handler list, ahead of the
// SPA fallback (server/router.go): gin runs NoRoute handlers in registration
// order and only stops early on Abort, so a MISS here — no segment, no claim,
// or the claiming plugin is not currently enabled — must return WITHOUT
// aborting or writing anything, letting the SPA fallback run unchanged. This
// preserves the demo frontend's catch-all behavior exactly (a hard
// constraint): every path this handler does not recognize behaves identically
// to before it existed.
func ExtPublicPrefixDispatch(c *gin.Context) {
	first, rest, ok := firstPathSegment(trimBasePath(c.Request.URL.Path))
	if !ok {
		return
	}
	pluginID, ok := extension.DefaultRegistry.ResolvePublicPrefix(first)
	if !ok {
		return
	}
	e, ok := extension.DefaultRegistry.Get(pluginID)
	if !ok || !e.Enabled() {
		// A claim can outlive a soft-disable (Registry.Disable only flips
		// e.enabled, it does not release the prefix — see registry.go); such a
		// disabled claim must behave like no claim at all, not dispatch to a
		// dead plugin.
		return
	}
	subpath := rest
	if subpath == "" {
		subpath = "/"
	}
	dispatchExtRoute(c, pluginID, subpath, true)
}

// trimBasePath strips the deployment's configured base path (conf.URL.Path —
// the same prefix server/router.go mounts every route under via
// e.Group(conf.URL.Path), a reverse-proxy sub-path deployment like
// "/alist") from an incoming request path, before ExtPublicPrefixDispatch
// extracts the top-level segment to resolve against the public_prefix claim
// table.
//
// This matters because gin's NoRoute is registered at the ENGINE level, not
// on the base-path group: the raw path NoRoute sees for a request under a
// non-root base path is "/alist/s/xxx", not "/s/xxx" — the same shape
// firstPathSegment expects and the same shape a plugin's claimed prefix ("/s")
// is declared/validated against (CheckReservedPrefix/
// claimPublicPrefixLocked never see or account for a base path either). Left
// unstripped, "alist" (the base path's own first segment) would be looked up
// as the claimed segment instead of "s", so public_prefix dispatch would
// silently never match under ANY non-root base_path deployment — the
// underlying bug this fixes.
//
// A base path of "" or "/" (the common case, no reverse-proxy sub-path) is a
// no-op. Matching requires a segment boundary (the base path itself, or the
// base path followed by "/"): a path that merely starts with the same
// characters but diverges at the next segment (base "/alist", path
// "/alistx/…") is NOT a match and is returned unchanged — such a request is
// outside the deployment's mounted root entirely and was never reachable
// before this handler existed either, so it must keep behaving like a miss
// (fall through to the SPA handler), not be misparsed as a claim lookup for
// "x".
func trimBasePath(path string) string {
	if conf.URL == nil {
		// Not yet initialized (bootstrap.initURL runs before server.Init in
		// production; some test packages never call it at all) — treat as
		// the default root base path, a no-op.
		return path
	}
	base := conf.URL.Path
	if base == "" || base == "/" {
		return path
	}
	base = strings.TrimSuffix(base, "/")
	if path == base {
		return "/"
	}
	if rest, ok := strings.CutPrefix(path, base+"/"); ok {
		return "/" + rest
	}
	return path
}

// firstPathSegment splits a URL path into its first segment and the
// (still-rooted) remainder, e.g. "/s/abc/def" -> ("s", "/abc/def", true) and
// "/s" -> ("s", "", true). An empty or rootless path (bare "/", "") reports
// ok=false — nothing to resolve a prefix claim against.
func firstPathSegment(p string) (first, rest string, ok bool) {
	trimmed := strings.TrimPrefix(p, "/")
	if trimmed == "" {
		return "", "", false
	}
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i], trimmed[i:], true
	}
	return trimmed, "", true
}

// writeExtRouteError maps a dispatch error to a real HTTP status (these routes
// are raw proxies, not the JSON envelope): an undeclared route is a 404, a
// permission denial (missing public_routes grant) is a 403, an unwired handler
// is 501, anything else 502 (the plugin failed).
func writeExtRouteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, extension.ErrRouteNotDeclared):
		c.AbortWithStatus(http.StatusNotFound)
	case errors.Is(err, ext.ErrPermissionDenied):
		c.AbortWithStatus(http.StatusForbidden)
	default:
		if code, ok := ext.CodeOf(err); ok && code == ext.ErrCodeNotImplement {
			c.AbortWithStatus(http.StatusNotImplemented)
			return
		}
		c.AbortWithStatus(http.StatusBadGateway)
	}
}
