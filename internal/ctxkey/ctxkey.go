// Package ctxkey is the single source of truth for request-context keys shared
// across layers (middlewares set them, fs/op read them).
//
// The keys stay plain strings because gin.Context.Set/Get only accepts string
// keys and gin.Context.Value resolves those same keys. Migrating to typed keys
// requires moving the middlewares onto c.Request.WithContext, which is planned
// together with the auth middleware rework (rollout Step 7/8).
package ctxkey

const (
	// User is the authenticated *model.User for the current request.
	User = "user"
	// Meta is the *model.Meta resolved for the requested path.
	Meta = "meta"
	// Path is the raw request path stored by the down middleware.
	Path = "path"
	// DeviceKey is the device session key set by the auth middleware.
	DeviceKey = "device_key"
	// SessionInactive marks a request whose device session was evicted.
	SessionInactive = "session_inactive"
	// RequestHeader carries the inbound http.Header into proxied range requests.
	RequestHeader = "request_header"
	// Lang is the negotiated response locale (e.g. "en", "zh-CN") for the
	// current request, set by the I18n middleware and read by the envelope.
	Lang = "lang"
)
