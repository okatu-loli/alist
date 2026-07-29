package hostcap

// This file maps an admin's PERSISTED consent (design §6.2) into the runtime
// Grant the borrowed-capability layer enforces (§6.3). The plugin's Manifest is
// never consulted here: host.FS/HTTP/Store/Sign gate on what the admin APPROVED.
//
// SEAM: there is no production caller of hostcap.New yet — the reverse Host
// bridge is not wired into the plugin launch path (grpc/host Client.Init is a
// no-op; F0 report "Grant门接线点"). When that lands, the launcher reads the
// plugin's persisted extension.Grant, copies its host_apis/network into the
// hostcap shapes (a trivial field copy — the two GrantItem types are identical
// by design, restated across the import boundary), and calls GrantFromConsent to
// obtain the runtime Grant it closes over in hostcap.New. Until then this is the
// tested mapping primitive that wiring will call, keeping the fs-LEVEL → subtree
// policy in one reviewable place.

// GrantFromConsent builds the runtime Grant a plugin's borrowed-capability layer
// enforces from the admin's approved consent. host_apis and the network
// allowlist pass straight through; the declarative filesystem LEVEL
// (none|temp|full) is expanded into the concrete borrowed subtree(s) the
// plugin's restricted principal is confined to:
//
//   - "none" (or empty): no fs scope — the plugin can borrow no files.
//   - "temp": one writable scratch subtree (tempDir), for extract/transform.
//   - "full": the whole virtual tree ("/"), writable — the broad grant the
//     consent page marks high-risk.
//
// tempDir is the host's per-plugin scratch anchor; an empty tempDir with level
// "temp" yields NO scope (fail-closed) rather than silently widening to "/".
func GrantFromConsent(pluginID string, hostAPIs []GrantItem, network []string, fsLevel, tempDir string) Grant {
	g := Grant{PluginID: pluginID, HostAPIs: hostAPIs, Network: network}
	switch fsLevel {
	case "temp":
		if tempDir != "" {
			g.FS = []FSScope{{Path: tempDir, Write: true}}
		}
	case "full":
		g.FS = []FSScope{{Path: "/", Write: true}}
	}
	return g
}
