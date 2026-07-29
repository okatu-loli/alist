package ext

import "context"

// Action is a plugin operation the user triggers from the UI: a file/dir
// context-menu action or a global action (design §2, contributions MenuAction).
// The plugin declares the actions it offers as Contributions.MenuActions
// (id + label + target); this interface is how the host invokes the one the
// user picked.
//
// The host executes an action only AFTER authorizing the caller (the
// /api/v4/extensions/{id}/actions/{aid} endpoint is admin-gated; per-action
// permission scopes are a later refinement). The action does its filesystem
// work through the Grant-gated Host.FS keyed off principal, so an action can
// never reach outside what the plugin was granted, and the core Authorize still
// runs on every borrowed operation.
type Action interface {
	Extension

	// Invoke runs the action named actionID for principal, with opaque args from
	// the client. principal is the host-issued identity handle of the acting user
	// (design §3) — the empty Principal means the plugin's own bound identity.
	// The returned value is serialized back to the client.
	Invoke(ctx context.Context, principal Principal, actionID string, args map[string]any) (any, error)
}
