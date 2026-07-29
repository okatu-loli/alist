package ext

import "context"

// Assist fulfils one of an extension's declared assist forms (SDK §4.1): a
// field-level "assist" completion flow (x-assist on a JSONSchema field
// references an AssistForm; submitting it refills the originating field).
// It is an independent, optional interface — not embedded in Extension —
// because it always arrives alongside the impl of whatever Kind the plugin
// actually declares (StorageDriver, Action, ...); a plugin implements Assist
// on the same value it already hands to ServeXxx, and every Serve* entry
// point auto-detects it (mirrors how Capability works for StorageDriver).
//
// CONTRACT — your binary may run as two separate OS processes at once. The
// host's assist gRPC leg (internal/extension/pluginproc, host side) launches
// a DEDICATED subprocess that only ever dispenses "assist", independent of
// whatever process is already running for your extension's primary Kind
// (StorageDriver/Action/...). Concretely: if your plugin implements both
// ext.Action and ext.Assist on the same value, the host may at the same time
// run one process serving ActionService (impl.Init called once there) and a
// second, entirely separate process serving only AssistService (impl.Init
// called once there too) — two live instances of your impl, each Init'd
// independently, possibly concurrently. See Extension.Init's doc comment for
// what this requires of Init/Shutdown and any package-level state.
type Assist interface {
	// Assist runs the form named formID for principal, with opaque input
	// from the client. principal is the host-issued identity handle of the
	// acting user (design §3) — the empty Principal means the plugin's own
	// bound identity. The returned value is serialized back to the client.
	Assist(ctx context.Context, principal Principal, formID string, input map[string]any) (any, error)
}
