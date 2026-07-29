package extension

import ext "github.com/AlistGo/alist-ext"

// This file is the F-consent persisted-consent model: the record of what an
// admin actually APPROVED for a plugin, which the runtime reads as the sole
// authority. The Manifest
// only says what a plugin REQUESTS; the Grant says what it GOT. A capability the
// Manifest declares but the Grant omits is denied — "declared != granted".

// GrantItem is one approved capability: a Name plus an optional Scope refinement
// (e.g. fs "read"/"write"). Mirrors design §6.2 GrantItem and hostcap.GrantItem.
// The shape is restated here rather than shared because the extension root
// package cannot import internal/extension/hostcap (hostcap → internal/op →
// internal/extension would cycle) — the same reason grant_gate.go restates
// hostcap's capability-name constants.
type GrantItem struct {
	Name  string `json:"name"`
	Scope string `json:"scope,omitempty"`
}

// Grant is the persisted record of what the admin approved for a plugin (design
// §6.2). It — not the Manifest — is the runtime authority: the dispatch gate
// (grant_gate.go) and the borrowed-capability layer (hostcap) read the Grant, so
// a capability the admin did not approve is denied even though the Manifest
// declared it, and a revocation or a narrowed network host takes effect without
// re-reading the package. Persisted inside PluginState in state.json.
type Grant struct {
	// ReviewedVersion is the plugin version whose declared permission set this
	// Grant was approved against (design §6.2 reviewed_version). Update compares
	// the new version's requests against it to decide whether re-consent is due.
	ReviewedVersion string      `json:"reviewed_version,omitempty"`
	HostAPIs        []GrantItem `json:"host_apis,omitempty"`      // approved Host methods
	Network         []string    `json:"network,omitempty"`        // approved outbound host globs
	StorageTokens   []GrantItem `json:"storage_tokens,omitempty"` // approved storage-token scopes
	Filesystem      string      `json:"filesystem,omitempty"`     // approved fs level: none|temp|full
}

// HasAPI reports whether the Grant approves the named Host capability.
func (g *Grant) HasAPI(name string) bool {
	if g == nil {
		return false
	}
	for _, it := range g.HostAPIs {
		if it.Name == name {
			return true
		}
	}
	return false
}

// hostAPINames flattens the approved host_apis to their names, the shape the
// dispatch-gate resolver (GrantedHostAPIs) hands to grant_gate.go. A nil Grant
// yields an empty (non-nil) slice: default-deny, never "unset". The nil guard is
// load-bearing — a pre-F-consent state.json entry (or a hand-written one) has no
// `grant` field, so it deserializes to a nil Grant that LoadEnabled re-registers
// and the FIRST dispatch resolves through here; without the guard that was a nil
// dereference on the fs-mutation/event path (F-consent red-team F1).
func (g *Grant) hostAPINames() []string {
	if g == nil {
		return []string{}
	}
	out := make([]string, 0, len(g.HostAPIs))
	for _, it := range g.HostAPIs {
		out = append(out, it.Name)
	}
	return out
}

// clampToManifest returns a copy of g with every capability the Manifest does
// NOT request removed: the Manifest is the CEILING (what the plugin declared it
// needs), the Grant is what the admin approved WITHIN it, so a Grant must never
// exceed the manifest even if an admin-authored /grant call (a buggy console, a
// CSRF'd admin session, API misuse) submits more. Drops off-manifest host_apis,
// network hosts, and storage tokens, and caps the fs level to the manifest's
// (F-consent red-team F2/F3). A nil Grant clamps to an empty grant.
//
// allowExtraNetwork is the one intentional exception: when true (an admin has
// explicitly acknowledged the risk — see server SetPluginGrant), a submitted
// network host that the manifest did NOT declare is kept rather than dropped.
// host_apis, storage_tokens, and the fs level are clamped to the manifest
// regardless of allowExtraNetwork — network is the only axis an admin may
// extend beyond what the plugin declared it needs.
func (g *Grant) clampToManifest(m ext.Manifest, allowExtraNetwork bool) *Grant {
	reqAPI := map[string]bool{}
	for _, n := range m.Permissions.HostAPIs {
		reqAPI[n] = true
	}
	reqHost := map[string]bool{}
	for _, h := range m.Permissions.Network {
		reqHost[h] = true
	}
	reqTok := map[string]bool{}
	for _, s := range m.Permissions.StorageTokens {
		reqTok[s] = true
	}
	out := &Grant{ReviewedVersion: m.Version}
	if g != nil {
		out.ReviewedVersion = g.ReviewedVersion
		for _, it := range g.HostAPIs {
			if reqAPI[it.Name] {
				out.HostAPIs = append(out.HostAPIs, it)
			}
		}
		for _, h := range g.Network {
			// network is the ONE axis an admin may extend beyond the manifest,
			// but only after an explicit risk acknowledgement (allowExtraNetwork);
			// host_apis / storage_tokens / fs stay clamped no matter what.
			if allowExtraNetwork || reqHost[h] {
				out.Network = append(out.Network, h)
			}
		}
		for _, it := range g.StorageTokens {
			if reqTok[it.Name] {
				out.StorageTokens = append(out.StorageTokens, it)
			}
		}
		if fsLevelRank(g.Filesystem) <= fsLevelRank(m.Permissions.Filesystem) {
			out.Filesystem = g.Filesystem
		} else {
			out.Filesystem = m.Permissions.Filesystem
		}
	}
	return out
}

// grantFromManifest builds the default Grant recorded at a direct (non-wizard)
// install: it approves exactly what the Manifest requests. The flat v4 Manifest
// permission form carries no per-item required/optional marker, so every
// requested capability is treated as required and approved by the admin's
// explicit install action — an authenticated admin install call is itself the
// consent, mirroring EnablePlugin's "calling this implies informed consent". A
// wizard-driven install overwrites this with the admin's actual selection via
// SetGrant. When the Manifest gains the structured §6.1 required/optional form,
// the optional-default-deny refinement lands here; until then this is the honest
// behavior (documented in the F-consent report).
func grantFromManifest(m ext.Manifest) *Grant {
	g := &Grant{ReviewedVersion: m.Version, Filesystem: m.Permissions.Filesystem}
	for _, name := range m.Permissions.HostAPIs {
		g.HostAPIs = append(g.HostAPIs, GrantItem{Name: name})
	}
	if len(m.Permissions.Network) > 0 {
		g.Network = append([]string(nil), m.Permissions.Network...)
	}
	for _, s := range m.Permissions.StorageTokens {
		g.StorageTokens = append(g.StorageTokens, GrantItem{Name: s})
	}
	return g
}

// fsLevelRank orders the filesystem permission levels so an update can tell a
// widening (temp→full) from a narrowing. Unknown/empty == none == 0.
func fsLevelRank(level string) int {
	switch level {
	case "temp":
		return 1
	case "full":
		return 2
	default: // "", "none"
		return 0
	}
}

// manifestExceedsGrant reports whether m requests any host_api, network host,
// storage-token scope, or filesystem level BEYOND what g already approves — i.e.
// whether an update needs re-consent (design §6.6). A pure downgrade (the update
// drops permissions) does not: the approved Grant already covers everything the
// new version asks for. A nil Grant with any request exceeds (nothing approved).
func manifestExceedsGrant(m ext.Manifest, g *Grant) bool {
	if g == nil {
		g = &Grant{}
	}
	for _, name := range m.Permissions.HostAPIs {
		if !g.HasAPI(name) {
			return true
		}
	}
	for _, host := range m.Permissions.Network {
		if !containsStr(g.Network, host) {
			return true
		}
	}
	granted := map[string]bool{}
	for _, it := range g.StorageTokens {
		granted[it.Name] = true
	}
	for _, s := range m.Permissions.StorageTokens {
		if !granted[s] {
			return true
		}
	}
	return fsLevelRank(m.Permissions.Filesystem) > fsLevelRank(g.Filesystem)
}
