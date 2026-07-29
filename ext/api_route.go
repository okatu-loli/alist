package ext

import "context"

// APIRoute lets a plugin mount HTTP sub-routes the host proxies to it: a
// webhook receiver, a share-management API, a public share landing page (design
// §2.5 FeatureBundle = APIRoute + PublicPage + …). The plugin declares its
// routes and one Handle entry point; the host owns the actual mounting, the
// reserved-prefix isolation, and the authentication boundary.
//
// Two boundaries the host enforces around this interface:
//
//   - Namespacing / reserved words: a plugin's routes mount under the plugin's
//     own namespace (/api/v4/ext/{plugin-id}/…), so they can never collide with
//     a core route. A route that instead asks for a top-level short prefix
//     (a share short-link) is checked against the host's reserved-prefix table
//     (CheckReservedPrefix) and refused on conflict — at install/registration.
//   - Authentication: a RouteDecl with Public=false mounts behind the auth
//     middleware (only logged-in callers reach Handle). Public=true mounts under
//     the unauthenticated public group AND is gated on the "public_routes"
//     grant; its Handle runs with a delegated-only Host (design §2.5), so a
//     public route can borrow the fs only through an owner-delegated principal,
//     never the plugin's own grant.
type APIRoute interface {
	Extension

	// Routes declares the sub-routes this plugin serves. The host validates and
	// mounts each; an empty/invalid prefix or one that escapes the plugin
	// namespace is rejected.
	Routes() []RouteDecl

	// Handle serves one proxied request. req.URL carries the sub-path BELOW the
	// plugin's mount (the host strips the /api/v4/ext/{id} prefix). The response
	// is written back to the client verbatim. Handle must not assume it can reach
	// the host beyond its granted capabilities.
	Handle(ctx context.Context, req *HTTPRequest) (*HTTPResponse, error)
}

// RouteDecl is one sub-route a plugin serves.
type RouteDecl struct {
	// Prefix is the sub-path under the plugin namespace this route owns, e.g.
	// "/webhook" or "/s". It must be a rooted, non-traversing path segment set
	// ("/a/b" is fine, "../x" is not).
	Prefix string `json:"prefix"`
	// Public mounts the route under the unauthenticated public group. A public
	// route requires the "public_routes" grant and runs with a delegated-only
	// Host. Default false: mounted behind auth.
	Public bool `json:"public,omitempty"`
}
