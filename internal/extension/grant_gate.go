package extension

import "sync"

// This file is the F1 dispatch-time grant gate: the check the host runs before
// it DISPATCHES to a plugin (fires an EventHook, ticks a ScheduledJob, forwards
// an APIRoute/PublicPage request). It is distinct from — and layered above — the
// F0 hostcap gate: hostcap gates what a plugin may BORROW back (host.FS/HTTP/…)
// once it is running, enforcing Authorize and the network allowlist. This gate
// decides whether the plugin should be invoked at all.
//
// The two share one vocabulary: the canonical host_api capability names (design
// §3, §6.1). The names below duplicate hostcap's constants intentionally — the
// extension root package cannot import internal/extension/hostcap (hostcap →
// internal/op → internal/extension would cycle), so the frozen string contract
// is restated, not shared through a type.
const (
	// CapEvents gates EventHook delivery: receiving the shape of fs mutations
	// (paths, actor) is itself an information-disclosure capability, so a plugin
	// must have been granted it to be subscribed (design §3 Events() bus).
	CapEvents = "events"
	// CapPublicRoutes gates a plugin's PUBLIC (unauthenticated) routes (design
	// §3 PublicRouter, §6.1 "public_routes"). An authenticated APIRoute needs no
	// extra host_api beyond enablement; a public route does, because it is
	// reachable by anonymous callers.
	CapPublicRoutes = "public_routes"
)

// grantOverride is the production consent-backed resolver: given a plugin id it
// returns the granted host_api names and true (design §6.2). Since F-consent it
// is wired at DefaultManager construction to Manager.GrantedHostAPIs, so the
// persisted admin-approved Grant — not the Manifest — decides dispatch gating.
// It stays a settable seam (nil until wired) so tests can install their own
// resolver, and so an unknown id (a built-in, which the Manager does not track)
// falls through to the interim Manifest rule below.
var (
	grantOverrideMu sync.RWMutex
	grantOverride   func(pluginID string) ([]string, bool)
)

// SetGrantOverride installs (or, with nil, clears) the persisted-Grant resolver.
func SetGrantOverride(fn func(pluginID string) ([]string, bool)) {
	grantOverrideMu.Lock()
	defer grantOverrideMu.Unlock()
	grantOverride = fn
}

// GrantsCapability reports whether the plugin's EFFECTIVE grant includes the
// named host capability, for dispatch gating.
//
// Resolution order:
//   - persisted consent Grant via grantOverride (authoritative when the resolver
//     recognizes the id): the admin-approved host_apis decide, so a capability
//     the Manifest declares but the Grant omits is DENIED, and one the admin
//     revoked stops being dispatched immediately. An installed plugin with no
//     recorded Grant resolves to the empty set (default-deny).
//   - Manifest fallback (declared == granted): reached only when no resolver is
//     wired, or the resolver does not recognize the id (a built-in — which
//     declares no plugin permissions, so it is granted nothing anyway). A plugin
//     that did NOT declare a capability is still correctly denied it.
//
// A built-in (empty manifest permissions) declares nothing, so it is granted
// nothing here — built-ins do not participate in the plugin dispatch Kinds.
func (e *Entry) GrantsCapability(name string) bool {
	grantOverrideMu.RLock()
	fn := grantOverride
	grantOverrideMu.RUnlock()
	if fn != nil {
		if apis, ok := fn(e.Manifest.ID); ok {
			return containsStr(apis, name)
		}
	}
	return containsStr(e.Manifest.Permissions.HostAPIs, name)
}
