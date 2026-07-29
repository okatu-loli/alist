package handles

// F1 red-team: APIRoute proxy request-context leakage.
//
// proxyExtRoute (ext_route.go) forwards the inbound request to the plugin's
// Handle with req.Header = c.Request.Header.Clone(). That clone carries EVERY
// inbound header verbatim — including the caller's `Authorization: Bearer …`
// alist session token and any `Cookie` — into third-party plugin code.
//
// FINDING: a plugin is exactly the
// untrusted code the capability model exists to contain, yet the authenticated
// caller's own alist credential is handed to it. A malicious APIRoute plugin can
// lift that bearer token and replay it against the core /api/v4/* as the user —
// a privilege escalation beyond its granted Host capabilities. The Host FS/HTTP
// gates are irrelevant once the plugin holds the user's session token.
//
// This test is written as a CHARACTERIZATION of the CURRENT (leaking) behavior:
// it asserts the plugin receives the Authorization/Cookie today, and is marked
// so that if the proxy starts stripping credentials (the recommended fix) the
// assertion is revisited deliberately. It does NOT modify product code.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// captureRoute records the headers its Handle was given.
type captureRoute struct {
	routes []ext.RouteDecl
	seen   http.Header
}

func (c *captureRoute) Manifest() ext.Manifest               { return ext.Manifest{} }
func (c *captureRoute) Init(context.Context, ext.Host) error { return nil }
func (c *captureRoute) Shutdown(context.Context) error       { return nil }
func (c *captureRoute) Routes() []ext.RouteDecl              { return c.routes }
func (c *captureRoute) Handle(_ context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	c.seen = req.Header
	return &ext.HTTPResponse{Status: 200, Body: []byte("ok")}, nil
}

func callExtRouteWithHeaders(handler gin.HandlerFunc, pluginID, subpath string, hdr http.Header) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "pluginid", Value: pluginID}, {Key: "proxypath", Value: subpath}}
	req := httptest.NewRequest("POST", "/x", nil)
	for k, vals := range hdr {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	c.Request = req
	handler(c)
	return w
}

// TestExtRouteStripsCallerCredentialsFromPlugin: FIXED. The authenticated
// caller's Authorization bearer + session Cookie must NOT reach the plugin's
// Handle — forwarding them would let untrusted plugin code lift the live session
// token and replay it as the user, past every host-capability gate.
func TestExtRouteStripsCallerCredentialsFromPlugin(t *testing.T) {
	cap := &captureRoute{routes: []ext.RouteDecl{{Prefix: "/webhook"}}}
	registerRoutePlugin(t, "h.api.leak", cap)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer USER-SESSION-TOKEN-abc123")
	hdr.Set("Cookie", "alist-token=USER-SESSION-TOKEN-abc123; other=1")
	hdr.Set("X-Real-Ip", "203.0.113.7") // a non-credential header still passes through

	w := callExtRouteWithHeaders(ExtRoute, "h.api.leak", "/webhook/hook", hdr)
	if w.Code != 200 {
		t.Fatalf("dispatch failed: code=%d", w.Code)
	}
	if cap.seen == nil {
		t.Fatal("plugin Handle was never invoked")
	}
	if got := cap.seen.Get("Authorization"); got != "" {
		t.Fatalf("REGRESSION: Authorization leaked to plugin: %q", got)
	}
	if got := cap.seen.Get("Cookie"); got != "" {
		t.Fatalf("REGRESSION: Cookie leaked to plugin: %q", got)
	}
	// A non-credential header is still forwarded (plugins legitimately need e.g.
	// the client IP / content type).
	if cap.seen.Get("X-Real-Ip") != "203.0.113.7" {
		t.Fatalf("non-credential header should pass through, got %q", cap.seen.Get("X-Real-Ip"))
	}
}

// TestExtPublicRouteStripsCookieFromPlugin: FIXED. The public (unauth) mount must
// likewise not ship a logged-in user's session Cookie into third-party plugin
// code.
func TestExtPublicRouteStripsCookieFromPlugin(t *testing.T) {
	cap := &captureRoute{routes: []ext.RouteDecl{{Prefix: "/s", Public: true}}}
	registerRoutePlugin(t, "h.api.pub.leak", cap, extension.CapPublicRoutes)

	hdr := http.Header{}
	hdr.Set("Cookie", "alist-token=LOGGED-IN-USER-TOKEN")

	w := callExtRouteWithHeaders(ExtPublicRoute, "h.api.pub.leak", "/s/landing", hdr)
	if w.Code != 200 {
		t.Fatalf("public dispatch failed: code=%d", w.Code)
	}
	if got := cap.seen.Get("Cookie"); got != "" {
		t.Fatalf("REGRESSION: Cookie leaked to public plugin Handle: %q", got)
	}
}
