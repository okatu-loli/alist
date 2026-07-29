package ext

import "context"

// Extension is the base interface of every extension. Init runs AT MOST ONCE
// PER PROCESS, guaranteed by the SDK — not "once per plugin binary" or "once
// per installed extension," and not something an implementation has to
// arrange for itself. Shutdown is process-level too, running once when that
// process's instance of the extension is torn down; per-instance lifecycle
// for storage drivers is added in StorageDriver.
//
// The SDK-guaranteed part matters because a single process can end up
// dispensing the same impl under more than one Kind name (or the same Kind
// name more than once): a plugin that implements Assist (assist.go)
// alongside its primary Kind gets its Assist leg auto-mounted as a second
// dispense name on the SAME process serving its primary Kind, and a future
// multi-Kind single-process dispense can mount several primary Kinds
// together the same way. Each dispense independently calls InitHost the
// first time it sees a live host — without a guarantee at the SDK layer,
// that would mean impl.Init running more than once in that process. The
// SDK (ext/grpcplugin's initOnce, wired into every Kind's InitHost handler)
// is what closes that gap: no matter how many InitHost calls land on the
// same impl in the same process, sequential or racing, Init runs exactly
// once and every caller observes that one call's outcome.
//
// "At most once per PROCESS" still is not "once per plugin binary" or "once
// per installed extension," though: a plugin that implements Assist can have
// its impl.Init called from TWO separate OS processes started from the same
// binary — the host's ordinary Kind subprocess, and a second, dedicated
// assist-only subprocess the assist gRPC leg launches independently
// (pluginproc keeps assist in its own process pool precisely so it never
// shares a process — and, absent the SDK-side dispense sharing described
// above, would never share an Init call either — with any other Kind).
// Nothing guarantees those two processes' Init calls are sequenced or even
// mutually exclusive in time; the initOnce guarantee is scoped to one
// process, never across the process boundary.
//
// Consequences for an implementation:
//   - Init/Shutdown must be safe to run in a second, independent process
//     instance of the same binary — i.e. idempotent/re-entrant at the level
//     of "a fresh process calling this once," not merely safe against being
//     called twice within one process (the SDK already rules that out).
//   - Init must not assume it owns exclusive process-wide resources: no PID
//     files, no fixed listening ports, no singleton lock files that a second
//     concurrently-running instance would collide on. Any real singleton
//     resource (a local cache dir, a vendor SDK client) must tolerate being
//     constructed twice, in two processes, at once.
//   - Side effects performed from Init (writing local state, opening
//     connections) must be safe under that same multi-instance assumption —
//     a plugin that starts a background poller in Init, for example, may end
//     up running two of them concurrently (one per OS process).
type Extension interface {
	Manifest() Manifest
	// Init is called with the Host callback set the core injects for this
	// process. See the interface doc comment above: the SDK guarantees this
	// runs at most once per process even if the impl is dispensed under
	// several Kind/assist names sharing that process, but a second,
	// independent OS process running the same binary (the assist gRPC leg's
	// dedicated subprocess, most notably) is its own process and gets its
	// own, separate Init call.
	Init(ctx context.Context, host Host) error
	// Shutdown is called once per process when that process's instance of the
	// extension is torn down.
	Shutdown(ctx context.Context) error
}

// Host is the set of capabilities the core exposes back to an extension
// (SDK doc §3, design v4-plugin-extensions-design.md §3). Every capability is
// gated at the host boundary by the plugin's runtime Grant (§6.3): a plugin
// that was not granted a capability sees ErrPermissionDenied from the
// corresponding accessor / its methods.
//
// The capability sub-interfaces (FS/Auth/Store/Sign/HTTP/Cache) and their DTOs
// live in host.go. F0 wires FS/Store/HTTP end-to-end over the reverse gRPC
// bridge; Auth/Sign/Cache have host-side implementations (the reverse-bridge
// leg for those follows in a later wave). An in-process (native) plugin
// receives a Host that calls the core directly; a subprocess (gRPC) plugin
// receives a Host whose calls travel back over the plugin→host bridge.
type Host interface {
	Logger() Logger
	FS() FS
	Auth() Auth
	Store() Store
	Sign() Signer
	HTTP() HTTPClient
	Cache() Cache
	// LoadConfig returns the plugin's persisted config_form value as JSON, or
	// nil if none has been set.
	LoadConfig(ctx context.Context) ([]byte, error)
	// SaveConfig persists the plugin's own config_form value as JSON (cfg must
	// be valid JSON; an empty cfg clears it). Mirrors LoadConfig in the write
	// direction. A Host built without a config saver wired (e.g. no admin-
	// editable config_form) returns an ErrCodeNotImplement error rather than
	// silently discarding the write.
	SaveConfig(ctx context.Context, cfg []byte) error
	// Capabilities returns the names of the Host APIs (host_api vocabulary,
	// e.g. "host.fs"/"store"/"sign") this plugin was granted — introspection
	// over its own Grant, so a plugin can adapt its behavior to what it was
	// actually approved for instead of guessing from its own manifest request.
	Capabilities(ctx context.Context) ([]string, error)
}

// Logger is the host's logger, namespaced to the extension.
type Logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}
