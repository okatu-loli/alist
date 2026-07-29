package grpcplugin

import (
	"log"

	ext "github.com/AlistGo/alist-ext"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// mountConfig is MountOption's target — see WithKinds.
type mountConfig struct {
	// kinds, when non-nil, narrows mountAll to only the listed Kinds (plus
	// assist, which is never gated by this list — see mountAll). nil (the
	// default, no options passed) means "every Kind impl satisfies".
	kinds []ext.Kind
}

// MountOption configures mountAll/ServeMulti.
type MountOption func(*mountConfig)

// WithKinds restricts ServeMulti/mountAll to dispensing only the listed
// Kinds, even if impl's concrete type satisfies more of the 14 Kind
// interfaces than that. This is an escape hatch, not a correctness
// requirement: auditing all 14 Kind interfaces' method sets (see the phG2
// report's containment table) found no case where one Kind's full interface
// is an accidental subset of another's — every Kind adds at least one
// distinctly-named method beyond Extension, so a plugin's concrete type
// satisfies exactly the Kinds it was actually written to implement, never
// one it wasn't. WithKinds exists for a plugin author who deliberately wants
// to hold a Kind back from a particular build/deployment (e.g. staging a
// second Kind's rollout) despite the underlying value already satisfying
// it, not to resolve any ambiguity mountAll itself cannot already resolve on
// its own.
func WithKinds(kinds ...ext.Kind) MountOption {
	return func(c *mountConfig) { c.kinds = kinds }
}

func (c *mountConfig) allows(k ext.Kind) bool {
	if c.kinds == nil {
		return true
	}
	for _, kk := range c.kinds {
		if kk == k {
			return true
		}
	}
	return false
}

// mountAll builds the combined go-plugin Plugin map for every Kind (plus the
// assist auxiliary leg) impl implements, so a single process can dispense
// several Kinds' gRPC services at once (G2: single-process, multi-Kind
// dispense). impl is type-asserted against each of the 14 Kind interfaces
// independently; a Kind whose interface impl does not satisfy is simply
// skipped, not an error — a plugin implementing one or two Kinds gets
// exactly those dispense names mounted, nothing else. Every one of the
// per-Kind PluginMap functions below already calls mountAssist itself
// (assistserve.go), so calling more than one of them for the same impl is
// harmless: each one (redundantly but correctly) re-adds the identical
// pb.AssistPluginName entry when impl also implements ext.Assist, and map
// assignment makes repeats a no-op.
//
// ONE pair is a real exception to "every Kind adds at least one distinctly
// named method, so satisfying one Kind never silently satisfies another"
// (see the phG2 report's containment table): BackgroundService.Run(ctx)
// error and ScheduledJob.Run(ctx) error share an IDENTICAL signature, but
// mutually exclusive contracts — BackgroundService.Run must block until ctx
// is cancelled (it IS the long-running task); ScheduledJob.Run must return
// promptly once per tick (the host calls it repeatedly on a schedule).
// mountAll deliberately does NOT double-mount both when the SAME impl
// satisfies both (unlike every other pair, where double-mounting is simply
// correct): whichever Kind's dispense wins the race to actually invoke Run
// would silently break the other's contract. Absent an explicit WithKinds
// choice, it warns and keeps BackgroundService, dropping ScheduledJob — an
// arbitrary but documented tie-break (BackgroundService's Run signature is
// what a plugin author is far more likely to have written first; there is no
// principled way to prefer one from the type system alone). Pass
// WithKinds(ext.KindScheduledJob) (without ext.KindBackgroundService) to
// choose the other one instead.
//
// nothing implemented (impl satisfies no Kind at all, e.g. it exists solely
// to provide ext.Assist standalone) falls back to mounting assist alone via
// AssistPluginMap directly, mirroring how a plugin author would call
// ServeAssist(impl) today for that shape.
func mountAll(impl any, opts ...MountOption) map[string]plugin.Plugin {
	cfg := &mountConfig{}
	for _, o := range opts {
		o(cfg)
	}

	out := map[string]plugin.Plugin{}
	merge := func(m map[string]plugin.Plugin) {
		for k, v := range m {
			out[k] = v
		}
	}

	if cfg.allows(ext.KindStorageDriver) {
		if d, ok := impl.(ext.StorageDriver); ok {
			merge(PluginMap(d))
		}
	}
	if cfg.allows(ext.KindAuthProvider) {
		if d, ok := impl.(ext.AuthProvider); ok {
			merge(AuthProviderPluginMap(d))
		}
	}
	if cfg.allows(ext.KindAuthNMethod) {
		if d, ok := impl.(ext.AuthNMethod); ok {
			merge(AuthNMethodPluginMap(d))
		}
	}

	// BackgroundService vs ScheduledJob: see the Run(ctx) error clash in the
	// doc comment above. mountBg/mountSchedJob start from what cfg.allows
	// (WithKinds) and impl's own type assertion would independently decide;
	// only when BOTH would otherwise be mounted for the SAME impl does the
	// tie-break kick in, so an explicit WithKinds naming only one of the two
	// never even reaches the warning (the other was already excluded).
	_, implBg := impl.(ext.BackgroundService)
	mountBg := cfg.allows(ext.KindBackgroundService) && implBg
	_, implSchedJob := impl.(ext.ScheduledJob)
	mountSchedJob := cfg.allows(ext.KindScheduledJob) && implSchedJob
	if mountBg && mountSchedJob {
		log.Printf("[plugin][warn] grpcplugin.mountAll: impl satisfies both Kind=BackgroundService and Kind=ScheduledJob, whose Run(ctx) error methods are signature-identical but contract-incompatible (block-forever vs return-promptly-per-tick); mounting BackgroundService only. Pass WithKinds(ext.KindScheduledJob) to choose ScheduledJob instead.")
		mountSchedJob = false
	}
	if mountBg {
		merge(BackgroundServicePluginMap(impl.(ext.BackgroundService)))
	}

	if cfg.allows(ext.KindAPIRoute) {
		if d, ok := impl.(ext.APIRoute); ok {
			merge(APIRoutePluginMap(d))
		}
	}
	if cfg.allows(ext.KindAction) {
		if d, ok := impl.(ext.Action); ok {
			merge(ActionPluginMap(d))
		}
	}
	if cfg.allows(ext.KindEventHook) {
		if d, ok := impl.(ext.EventHook); ok {
			merge(EventHookPluginMap(d))
		}
	}
	if mountSchedJob {
		merge(ScheduledJobPluginMap(impl.(ext.ScheduledJob)))
	}
	if cfg.allows(ext.KindNotification) {
		if d, ok := impl.(ext.Notification); ok {
			merge(NotificationPluginMap(d))
		}
	}
	if cfg.allows(ext.KindOfflineDownloadTool) {
		if d, ok := impl.(ext.OfflineDownloadTool); ok {
			merge(OfflineDownloadToolPluginMap(d))
		}
	}
	if cfg.allows(ext.KindSearchIndexer) {
		if d, ok := impl.(ext.SearchIndexer); ok {
			merge(SearchIndexerPluginMap(d))
		}
	}
	if cfg.allows(ext.KindArchiveTool) {
		if d, ok := impl.(ext.ArchiveTool); ok {
			merge(ArchiveToolPluginMap(d))
		}
	}
	if cfg.allows(ext.KindFileDecorator) {
		if d, ok := impl.(ext.FileDecorator); ok {
			merge(FileDecoratorPluginMap(d))
		}
	}
	if cfg.allows(ext.KindAccessProtocol) {
		if d, ok := impl.(ext.AccessProtocol); ok {
			merge(AccessProtocolPluginMap(d))
		}
	}

	if len(out) == 0 {
		if a, ok := impl.(ext.Assist); ok {
			merge(AssistPluginMap(a))
		}
	}

	return out
}

// ServeMulti runs impl as a go-plugin process serving every Kind (and the
// assist leg) it implements out of ONE process — the G2 single-process,
// multi-Kind dispense entry point. A plugin author whose impl satisfies
// several Kind interfaces on the same value calls this instead of one of
// the per-Kind ServeXxx entry points (ServeAction, Serve, ...), which remain
// available unchanged as single-Kind aliases for plugins that only ever
// implement one Kind (or that implement several but are deliberately
// launched one-Kind-per-process, e.g. during a staged migration).
//
// go-plugin's Serve already supports registering every Kind's GRPCServer
// onto the shared grpc.Server a single Plugins map produces (mountAll just
// points it at more than one Kind's entry — nothing new at the go-plugin
// layer); initOnce (initonce.go) is what makes that safe from the impl's
// point of view — it guarantees impl.Init runs exactly once no matter how
// many of the dispense names mountAll produced actually get Dispensed by
// the host.
func ServeMulti(impl any, opts ...MountOption) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         mountAll(impl, opts...),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// AllPluginMap is the go-plugin plugin set served/dispensed under every one
// of the 15 possible dispense names (14 Kinds + assist), with no Impl set on
// any of them (Impl is a plugin-side-only field — GRPCClient never reads
// it). host.LaunchShared uses this as its CLIENT-side Plugins map so
// Dispense(name) always succeeds locally regardless of which Kinds the
// actual subprocess implements: go-plugin's Dispense is a purely local
// lookup against the CALLER's own Plugins map (see LaunchShared's doc
// comment in the host package for the trap this shape exists to make safe
// — the peer's actual absence of a Kind only ever surfaces as a
// codes.Unimplemented gRPC status on a real RPC, never at Dispense time,
// when no reverse-host bridge is wired).
func AllPluginMap() map[string]plugin.Plugin {
	out := map[string]plugin.Plugin{}
	merge := func(m map[string]plugin.Plugin) {
		for k, v := range m {
			out[k] = v
		}
	}
	merge(PluginMap(nil))
	merge(AuthProviderPluginMap(nil))
	merge(AuthNMethodPluginMap(nil))
	merge(BackgroundServicePluginMap(nil))
	merge(APIRoutePluginMap(nil))
	merge(ActionPluginMap(nil))
	merge(EventHookPluginMap(nil))
	merge(ScheduledJobPluginMap(nil))
	merge(NotificationPluginMap(nil))
	merge(OfflineDownloadToolPluginMap(nil))
	merge(SearchIndexerPluginMap(nil))
	merge(ArchiveToolPluginMap(nil))
	merge(FileDecoratorPluginMap(nil))
	merge(AccessProtocolPluginMap(nil))
	merge(AssistPluginMap(nil))
	return out
}

// HostAllPluginMap is AllPluginMap's reverse-host-bridge counterpart: the
// host-side plugin set that wires the SAME reverse HostService factory
// (serverFactory) into every one of the 15 dispense names, so whichever
// Kind(s) the subprocess actually serves get a working host handed to Init
// via InitHost. Mirrors the individual HostXxxPluginMap functions, merged.
func HostAllPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	out := map[string]plugin.Plugin{}
	merge := func(m map[string]plugin.Plugin) {
		for k, v := range m {
			out[k] = v
		}
	}
	merge(HostPluginMap(serverFactory))
	merge(HostAuthProviderPluginMap(serverFactory))
	merge(HostAuthNMethodPluginMap(serverFactory))
	merge(HostBackgroundServicePluginMap(serverFactory))
	merge(HostAPIRoutePluginMap(serverFactory))
	merge(HostActionPluginMap(serverFactory))
	merge(HostEventHookPluginMap(serverFactory))
	merge(HostScheduledJobPluginMap(serverFactory))
	merge(HostNotificationPluginMap(serverFactory))
	merge(HostOfflineDownloadToolPluginMap(serverFactory))
	merge(HostSearchIndexerPluginMap(serverFactory))
	merge(HostArchiveToolPluginMap(serverFactory))
	merge(HostFileDecoratorPluginMap(serverFactory))
	merge(HostAccessProtocolPluginMap(serverFactory))
	merge(HostAssistPluginMap(serverFactory))
	return out
}
