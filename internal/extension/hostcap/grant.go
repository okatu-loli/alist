// Package hostcap implements the host side of the plugin capability layer:
// the concrete
// FS/Auth/Store/Sign/HTTP/Cache abilities a plugin borrows through the Host
// callback, each bound to the calling plugin's id + runtime Grant + restricted
// principal, and each enforcing a security invariant:
//
//  1. FS runs core op.Authorize on EVERY operation, against the ACTING
//     principal — borrowing FS never bypasses authorization. For the plugin's
//     own (self/empty) principal that acting identity is the restricted bound
//     principal Grant.FS defines. But a conduit protocol (AccessProtocol) that
//     authenticated an end user via host.Auth() acts AS THAT USER, so their full
//     permissions apply and Grant.FS does NOT further narrow the reach — a narrow
//     FS grant bounds only what the plugin itself can touch, not what a user it
//     logs in can. (F2 red-team FindingC: state this in consent UI/docs.)
//  2. HTTP dials only hosts on the effective allowlist (global outbound policy
//     ∩ Grant.Network) — an unlisted host is refused before the request leaves.
//  3. Store is namespaced by plugin id — plugin A cannot read plugin B's data
//     and no plugin can address a core table.
//  4. Sign only signs an ObjRef the plugin's principal can read — no
//     over-scoped or core links.
//  5. A capability the plugin was not granted (Grant.HostAPIs) returns
//     ErrPermissionDenied from its Host method.
//
// The Grant is a host-side fact: it is passed to the capability constructors and
// closed over, never taken as a wire input, so a subprocess plugin cannot alter
// what it was granted.
package hostcap

import (
	"strings"
)

// Canonical Host capability (host_api) names a Grant may carry. They match the
// Manifest.Permissions.HostAPIs vocabulary of the plugin-extensions design (§3,
// §6.1).
const (
	CapFS      = "host.fs"
	CapAuth    = "host.auth"
	CapStore   = "store"
	CapSign    = "sign"
	CapNetwork = "network"
	CapCache   = "cache"
)

// GrantItem is one approved capability (Name plus an optional Scope refinement,
// e.g. fs "read"/"write"). Mirrors the design's GrantItem (§6.2): a flat string
// cannot express "granted fs:read, denied fs:write".
type GrantItem struct {
	Name  string `json:"name"`
	Scope string `json:"scope,omitempty"`
}

// FSScope is one borrowed filesystem subtree the plugin's restricted principal
// is confined to. Write flips the grant from read-only to read-write. These
// rows ARE the plugin's fs authorization policy (see principal.go).
type FSScope struct {
	Path  string `json:"path"`
	Write bool   `json:"write,omitempty"`
}

// Grant is what the admin actually approved for a plugin, the runtime gate reads
// (design §6.2/§6.3). It is the effective, persisted authorization — the host
// consults this, never the Manifest, so revoking a capability or narrowing a
// network host takes effect without re-reading the package.
type Grant struct {
	PluginID string      `json:"plugin_id"`
	HostAPIs []GrantItem `json:"host_apis,omitempty"` // granted Host methods
	Network  []string    `json:"network,omitempty"`   // effective outbound host globs
	FS       []FSScope   `json:"fs,omitempty"`        // borrowed fs subtrees → principal
}

// HasAPI reports whether the plugin was granted the named Host capability.
func (g Grant) HasAPI(name string) bool {
	for _, it := range g.HostAPIs {
		if it.Name == name {
			return true
		}
	}
	return false
}

// NetworkAllows reports whether host matches any glob in Grant.Network. An empty
// allowlist denies everything (privacy baseline: default no outbound, design
// Step 2a). Matching is case-insensitive on the host label.
func (g Grant) NetworkAllows(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, pat := range g.Network {
		if hostGlobMatch(strings.ToLower(strings.TrimSpace(pat)), host) {
			return true
		}
	}
	return false
}

// hostGlobMatch matches a host against a network allowlist pattern: an exact
// host, "*" (any host — used only when the admin explicitly grants open
// egress), or a "*.example.com" wildcard that matches both any subdomain and
// the bare apex ("example.com"). Deliberately narrow: a network allowlist is a
// security boundary, so it does not accept arbitrary glob metacharacters that
// could widen a pattern unexpectedly.
func hostGlobMatch(pattern, host string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		apex := pattern[2:]
		if host == apex || strings.HasSuffix(host, "."+apex) {
			return true
		}
	}
	return false
}
