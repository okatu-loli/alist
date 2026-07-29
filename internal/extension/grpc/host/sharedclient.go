package host

import (
	"context"
	"fmt"
	"os/exec"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SharedClient is a host-side proxy over ONE go-plugin subprocess that may
// serve several Kinds' gRPC services at once (G2: single-process, multi-Kind
// dispense). go-plugin already lets a single Plugins map register every
// Kind's GRPCServer on the SAME shared grpc.Server (nothing new on the
// plugin/server side — see ext/grpcplugin's mountAll); SharedClient is the
// HOST side's counterpart: one plugin.Client whose ClientConfig.Plugins
// carries a stub for all 15 possible dispense names (grpcplugin.AllPluginMap
// / HostAllPluginMap), so Dispense(name) always succeeds LOCALLY no matter
// which Kinds the actual subprocess implements — see LaunchShared's doc
// comment for why that alone is not enough to safely tell what the peer
// actually serves.
//
// Each DispenseXxx method memoizes its result (dispensed), so repeated calls
// for the same Kind against the same SharedClient return the identical
// wrapped client rather than re-dispensing — mirrors every non-shared
// LaunchXxx's one-dispense-per-process shape, just spread across many Kinds
// sharing one process instead of one Kind owning it outright.
type SharedClient struct {
	pc  *plugin.Client
	rpc plugin.ClientProtocol

	// manifest is loaded once, at LaunchShared time, off whichever Kind the
	// caller identified as this process's primaryKind (see bootstrapManifest)
	// — every Kind's GetManifest RPC returns byte-identical JSON (there is
	// exactly one impl.Manifest() call in the whole subprocess), so any one
	// working Kind is as good as any other for this purpose.
	manifest ext.Manifest

	mu        sync.Mutex
	dispensed map[string]any // keyed by pb.XxxPluginName
}

// LaunchShared starts the plugin binary described by cmd, performs the
// go-plugin handshake against a Plugins map covering every one of the 15
// possible dispense names, and bootstraps the manifest off primaryKind (the
// Kind the FIRST caller actually wants — see pluginproc's shared pool, which
// always knows this: an AcquireXxx call names its own Kind). primaryKind
// must be one of the 14 real Kinds (ext.KindStorageDriver, ext.KindAction,
// ...) — never the assist auxiliary leg, which has no manifest RPC of its
// own and is never declared in Manifest.Kinds (see DispenseAssist).
//
// THE TRAP THIS EXISTS TO AVOID: go-plugin's Dispense(name) is a purely
// LOCAL lookup against the CALLER's own ClientConfig.Plugins map — it never
// asks the peer whether it actually registered a GRPCServer for that name.
// With a Plugins map covering all 15 names (as SharedClient's must, since it
// doesn't know ahead of time which Kinds the peer implements), Dispense
// would "succeed" for every Kind whether or not the peer serves it; the
// peer's actual absence of that Kind would only surface as a
// codes.Unimplemented gRPC status on the FIRST real RPC call — silently
// late, and only reliably distinguishable from an ordinary transport error
// by checking the gRPC status code (see AssistClient.Assist for this
// package's existing precedent, and the TestAssistGRPCNotMounted... tests in
// internal/extension/grpc/assistparity_test.go for the exact call shapes
// that trap takes). Rather than pay that trap on every DispenseXxx call,
// each one checks the ALREADY-LOADED manifest's Kinds field first (hasKind)
// and fails fast with ext.ErrCodeNotImplement when the Kind was never
// declared — no wasted Dispense, no waiting on a real RPC only to learn the
// peer never registered the service. Assist is the one exception: it isn't
// declared in Manifest.Kinds at all (see assist.go's doc comment), so
// DispenseAssist keeps the existing reactive Unimplemented-mapping shape
// AssistClient.Assist already implements, unchanged.
func LaunchShared(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host, primaryKind ext.Kind) (*SharedClient, error) {
	plugins := grpcplugin.AllPluginMap()
	if hostBuilder != nil {
		plugins = grpcplugin.HostAllPluginMap(NewHostServiceServer(hostBuilder()))
	}
	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  grpcplugin.Handshake,
		Plugins:          plugins,
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           hclog.NewNullLogger(),
	})
	rpcClient, err := pc.Client()
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: shared plugin handshake failed: %w", err)
	}
	sc := &SharedClient{pc: pc, rpc: rpcClient, dispensed: map[string]any{}}
	if err := sc.bootstrapManifest(ctx, primaryKind); err != nil {
		pc.Kill()
		return nil, err
	}
	return sc, nil
}

// bootstrapManifest dispenses primaryKind's service directly (bypassing the
// hasKind gate every other DispenseXxx applies — sc.manifest is exactly what
// this call is here to populate, so it cannot be checked yet) and caches the
// result, so a later DispenseXxx call for the same Kind reuses it instead of
// dispensing twice.
func (sc *SharedClient) bootstrapManifest(ctx context.Context, primaryKind ext.Kind) error {
	name, build, err := sc.specFor(ctx, primaryKind)
	if err != nil {
		return err
	}
	v, err := sc.dispenseOnce(name, build)
	if err != nil {
		return fmt.Errorf("host: shared plugin bootstrap dispense (kind=%s): %w", primaryKind, err)
	}
	if mh, ok := v.(interface{ Manifest() ext.Manifest }); ok {
		sc.manifest = mh.Manifest()
	}
	return nil
}

// Manifest returns the manifest loaded at LaunchShared time.
func (sc *SharedClient) Manifest() ext.Manifest { return sc.manifest }

// hasKind reports whether the loaded manifest declares Kind k.
func (sc *SharedClient) hasKind(k ext.Kind) bool {
	for _, kk := range sc.manifest.Kinds {
		if kk == k {
			return true
		}
	}
	return false
}

// dispenseOnce returns the cached client for name if one exists, else builds
// and caches it. Guards sc.dispensed with sc.mu so concurrent DispenseXxx
// calls for the same Kind (e.g. two storages sharing a pluginID both
// Acquire-ing at once under the shared pool) collapse into a single
// dispense rather than racing.
func (sc *SharedClient) dispenseOnce(name string, build func() (any, error)) (any, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if v, ok := sc.dispensed[name]; ok {
		return v, nil
	}
	v, err := build()
	if err != nil {
		return nil, err
	}
	sc.dispensed[name] = v
	return v, nil
}

// dispenseKind is the shared body every non-assist public DispenseXxx method
// below delegates to: hasKind-gate, then dispenseOnce.
func (sc *SharedClient) dispenseKind(ctx context.Context, kind ext.Kind) (any, error) {
	if !sc.hasKind(kind) {
		return nil, ext.NewError(ext.ErrCodeNotImplement,
			fmt.Sprintf("host: plugin manifest does not declare Kind=%s", kind))
	}
	name, build, err := sc.specFor(ctx, kind)
	if err != nil {
		return nil, err
	}
	v, err := sc.dispenseOnce(name, build)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			// The manifest DECLARED this Kind (hasKind above passed), but
			// the subprocess never actually registered its gRPC service —
			// a lying or buggy plugin (Manifest.Kinds says one thing, the
			// binary's mountAll-produced Plugins map serves another; go
			// through EVERY layer that could produce this — the InitHost
			// round trip GRPCClient does during Dispense when a
			// reverse-host bridge is wired, or the first real RPC
			// (GetManifest, inside buildXxx's loadManifest call) when it
			// isn't — and it always surfaces as codes.Unimplemented from
			// the peer's grpc.Server, since status.Code unwraps through
			// every %w-wrapped layer in between (errors.As-based, not a
			// naked type assertion) to find it). Review round 1, item 2:
			// this used to propagate as a bare wrapped transport error;
			// mapping it to ErrCodeNotImplement mirrors the existing
			// precedent for the exact same peer-shaped gap —
			// AssistClient.Assist's own status.Code(err)==Unimplemented
			// check, just applied at the dispense layer instead of a
			// business-call layer (assist has no manifest to pre-check
			// against, so it can only ever detect this reactively; a real
			// Kind can and should detect the common case (never declared)
			// proactively via hasKind above, but a declared-and-still-not-
			// served Kind needs this reactive fallback too).
			return nil, ext.NewError(ext.ErrCodeNotImplement,
				fmt.Sprintf("host: plugin manifest declares Kind=%s but its subprocess never registered the service: %v", kind, err))
		}
		return nil, err
	}
	return v, nil
}

// specFor maps a Kind to its dispense name and build func. The single
// source of truth both bootstrapManifest and dispenseKind read from, so the
// 14-Kind mapping is written exactly once.
func (sc *SharedClient) specFor(ctx context.Context, kind ext.Kind) (string, func() (any, error), error) {
	switch kind {
	case ext.KindStorageDriver:
		return pb.PluginName, func() (any, error) { return sc.buildStorageDriver(ctx) }, nil
	case ext.KindAuthProvider:
		return pb.AuthPluginName, func() (any, error) { return sc.buildAuthProvider(ctx) }, nil
	case ext.KindAuthNMethod:
		return pb.AuthNPluginName, func() (any, error) { return sc.buildAuthNMethod(ctx) }, nil
	case ext.KindBackgroundService:
		return pb.BgServicePluginName, func() (any, error) { return sc.buildBackgroundService(ctx) }, nil
	case ext.KindAPIRoute:
		return pb.APIRoutePluginName, func() (any, error) { return sc.buildAPIRoute(ctx) }, nil
	case ext.KindAction:
		return pb.ActionPluginName, func() (any, error) { return sc.buildAction(ctx) }, nil
	case ext.KindEventHook:
		return pb.EventHookPluginName, func() (any, error) { return sc.buildEventHook(ctx) }, nil
	case ext.KindScheduledJob:
		return pb.ScheduledJobPluginName, func() (any, error) { return sc.buildScheduledJob(ctx) }, nil
	case ext.KindNotification:
		return pb.NotificationPluginName, func() (any, error) { return sc.buildNotification(ctx) }, nil
	case ext.KindOfflineDownloadTool:
		return pb.OfflineDownloadToolPluginName, func() (any, error) { return sc.buildOfflineDownloadTool(ctx) }, nil
	case ext.KindSearchIndexer:
		return pb.SearchIndexerPluginName, func() (any, error) { return sc.buildSearchIndexer(ctx) }, nil
	case ext.KindArchiveTool:
		return pb.ArchiveToolPluginName, func() (any, error) { return sc.buildArchiveTool(ctx) }, nil
	case ext.KindFileDecorator:
		return pb.FileDecoratorPluginName, func() (any, error) { return sc.buildFileDecorator(ctx) }, nil
	case ext.KindAccessProtocol:
		return pb.AccessProtocolPluginName, func() (any, error) { return sc.buildAccessProtocol(ctx) }, nil
	default:
		return "", nil, fmt.Errorf("host: shared dispense: unknown kind %q", kind)
	}
}

// dispenseTyped dispenses name off sc's shared client and asserts the raw
// result as T (always one of the pb.XxxServiceClient interfaces) — the one
// piece of every buildXxx below that is genuinely uniform across Kinds.
func dispenseTyped[T any](sc *SharedClient, name, wantType string) (T, error) {
	var zero T
	raw, err := sc.rpc.Dispense(name)
	if err != nil {
		return zero, err
	}
	svc, ok := raw.(T)
	if !ok {
		return zero, fmt.Errorf("host: dispensed %T is not a %s", raw, wantType)
	}
	return svc, nil
}

func (sc *SharedClient) buildStorageDriver(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.DriverServiceClient](sc, pb.PluginName, "DriverServiceClient")
	if err != nil {
		return nil, err
	}
	c := &Client{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildAuthProvider(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.AuthProviderServiceClient](sc, pb.AuthPluginName, "AuthProviderServiceClient")
	if err != nil {
		return nil, err
	}
	c := &AuthClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildAuthNMethod(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.AuthNMethodServiceClient](sc, pb.AuthNPluginName, "AuthNMethodServiceClient")
	if err != nil {
		return nil, err
	}
	c := &AuthNClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildBackgroundService(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.BackgroundServiceServiceClient](sc, pb.BgServicePluginName, "BackgroundServiceServiceClient")
	if err != nil {
		return nil, err
	}
	c := &BgClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildAPIRoute(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.APIRouteServiceClient](sc, pb.APIRoutePluginName, "APIRouteServiceClient")
	if err != nil {
		return nil, err
	}
	c := &APIRouteClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildAction(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.ActionServiceClient](sc, pb.ActionPluginName, "ActionServiceClient")
	if err != nil {
		return nil, err
	}
	c := &ActionClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildEventHook(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.EventHookServiceClient](sc, pb.EventHookPluginName, "EventHookServiceClient")
	if err != nil {
		return nil, err
	}
	c := &EventHookClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildScheduledJob(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.ScheduledJobServiceClient](sc, pb.ScheduledJobPluginName, "ScheduledJobServiceClient")
	if err != nil {
		return nil, err
	}
	c := &ScheduledJobClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildNotification(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.NotificationServiceClient](sc, pb.NotificationPluginName, "NotificationServiceClient")
	if err != nil {
		return nil, err
	}
	c := &NotificationClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildOfflineDownloadTool(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.OfflineDownloadToolServiceClient](sc, pb.OfflineDownloadToolPluginName, "OfflineDownloadToolServiceClient")
	if err != nil {
		return nil, err
	}
	c := &OfflineDownloadToolClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildSearchIndexer(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.SearchIndexerServiceClient](sc, pb.SearchIndexerPluginName, "SearchIndexerServiceClient")
	if err != nil {
		return nil, err
	}
	c := &SearchIndexerClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildArchiveTool(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.ArchiveToolServiceClient](sc, pb.ArchiveToolPluginName, "ArchiveToolServiceClient")
	if err != nil {
		return nil, err
	}
	c := &ArchiveToolClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildFileDecorator(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.FileDecoratorServiceClient](sc, pb.FileDecoratorPluginName, "FileDecoratorServiceClient")
	if err != nil {
		return nil, err
	}
	c := &FileDecoratorClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (sc *SharedClient) buildAccessProtocol(ctx context.Context) (any, error) {
	svc, err := dispenseTyped[pb.AccessProtocolServiceClient](sc, pb.AccessProtocolPluginName, "AccessProtocolServiceClient")
	if err != nil {
		return nil, err
	}
	c := &AccessProtocolClient{pc: sc.pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// buildAssist dispenses the assist auxiliary service without any manifest
// load — AssistService deliberately has no GetManifest RPC (the manifest
// handshake already happened on whichever primary Kind bootstrapped this
// SharedClient); see AssistClient's doc comment.
func (sc *SharedClient) buildAssist() (any, error) {
	svc, err := dispenseTyped[pb.AssistServiceClient](sc, pb.AssistPluginName, "AssistServiceClient")
	if err != nil {
		return nil, err
	}
	return &AssistClient{pc: sc.pc, svc: svc}, nil
}

// DispenseStorageDriver dispenses (or returns the already-cached) client for
// this shared process's StorageDriver Kind, or ext.ErrCodeNotImplement if
// the manifest never declared it.
func (sc *SharedClient) DispenseStorageDriver(ctx context.Context) (*Client, error) {
	v, err := sc.dispenseKind(ctx, ext.KindStorageDriver)
	if err != nil {
		return nil, err
	}
	return v.(*Client), nil
}

// DispenseAuthProvider is DispenseStorageDriver's counterpart for Kind=AuthProvider.
func (sc *SharedClient) DispenseAuthProvider(ctx context.Context) (*AuthClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindAuthProvider)
	if err != nil {
		return nil, err
	}
	return v.(*AuthClient), nil
}

// DispenseAuthNMethod is DispenseStorageDriver's counterpart for Kind=AuthNMethod.
func (sc *SharedClient) DispenseAuthNMethod(ctx context.Context) (*AuthNClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindAuthNMethod)
	if err != nil {
		return nil, err
	}
	return v.(*AuthNClient), nil
}

// DispenseBackgroundService is DispenseStorageDriver's counterpart for Kind=BackgroundService.
func (sc *SharedClient) DispenseBackgroundService(ctx context.Context) (*BgClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindBackgroundService)
	if err != nil {
		return nil, err
	}
	return v.(*BgClient), nil
}

// DispenseAPIRoute is DispenseStorageDriver's counterpart for Kind=APIRoute.
func (sc *SharedClient) DispenseAPIRoute(ctx context.Context) (*APIRouteClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindAPIRoute)
	if err != nil {
		return nil, err
	}
	return v.(*APIRouteClient), nil
}

// DispenseAction is DispenseStorageDriver's counterpart for Kind=Action.
func (sc *SharedClient) DispenseAction(ctx context.Context) (*ActionClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindAction)
	if err != nil {
		return nil, err
	}
	return v.(*ActionClient), nil
}

// DispenseEventHook is DispenseStorageDriver's counterpart for Kind=EventHook.
func (sc *SharedClient) DispenseEventHook(ctx context.Context) (*EventHookClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindEventHook)
	if err != nil {
		return nil, err
	}
	return v.(*EventHookClient), nil
}

// DispenseScheduledJob is DispenseStorageDriver's counterpart for Kind=ScheduledJob.
func (sc *SharedClient) DispenseScheduledJob(ctx context.Context) (*ScheduledJobClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindScheduledJob)
	if err != nil {
		return nil, err
	}
	return v.(*ScheduledJobClient), nil
}

// DispenseNotification is DispenseStorageDriver's counterpart for Kind=NotificationChannel.
func (sc *SharedClient) DispenseNotification(ctx context.Context) (*NotificationClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindNotification)
	if err != nil {
		return nil, err
	}
	return v.(*NotificationClient), nil
}

// DispenseOfflineDownloadTool is DispenseStorageDriver's counterpart for Kind=OfflineDownloadTool.
func (sc *SharedClient) DispenseOfflineDownloadTool(ctx context.Context) (*OfflineDownloadToolClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindOfflineDownloadTool)
	if err != nil {
		return nil, err
	}
	return v.(*OfflineDownloadToolClient), nil
}

// DispenseSearchIndexer is DispenseStorageDriver's counterpart for Kind=SearchIndexer.
func (sc *SharedClient) DispenseSearchIndexer(ctx context.Context) (*SearchIndexerClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindSearchIndexer)
	if err != nil {
		return nil, err
	}
	return v.(*SearchIndexerClient), nil
}

// DispenseArchiveTool is DispenseStorageDriver's counterpart for Kind=ArchiveTool.
func (sc *SharedClient) DispenseArchiveTool(ctx context.Context) (*ArchiveToolClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindArchiveTool)
	if err != nil {
		return nil, err
	}
	return v.(*ArchiveToolClient), nil
}

// DispenseFileDecorator is DispenseStorageDriver's counterpart for Kind=FileDecorator.
func (sc *SharedClient) DispenseFileDecorator(ctx context.Context) (*FileDecoratorClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindFileDecorator)
	if err != nil {
		return nil, err
	}
	return v.(*FileDecoratorClient), nil
}

// DispenseAccessProtocol is DispenseStorageDriver's counterpart for Kind=AccessProtocol.
func (sc *SharedClient) DispenseAccessProtocol(ctx context.Context) (*AccessProtocolClient, error) {
	v, err := sc.dispenseKind(ctx, ext.KindAccessProtocol)
	if err != nil {
		return nil, err
	}
	return v.(*AccessProtocolClient), nil
}

// DispenseAssist dispenses (or returns the already-cached) client for the
// assist auxiliary leg. Unlike every other DispenseXxx, this is NOT gated by
// a manifest Kinds check (Assist is never declared there — see assist.go);
// it keeps the existing reactive shape: dispensing always locally succeeds
// (grpcplugin.AllPluginMap always carries the "assist" key), and a plugin
// that never implements ext.Assist is instead reported at the first real
// call (AssistClient.Assist already maps the peer's codes.Unimplemented to
// ext.ErrCodeNotImplement).
func (sc *SharedClient) DispenseAssist(ctx context.Context) (*AssistClient, error) {
	v, err := sc.dispenseOnce(pb.AssistPluginName, func() (any, error) { return sc.buildAssist() })
	if err != nil {
		return nil, err
	}
	return v.(*AssistClient), nil
}

// Exited reports whether the plugin subprocess has already died. See
// Client.Exited (client.go) for the rationale — same go-plugin reaper.
func (sc *SharedClient) Exited() bool { return sc.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only. Mirrors Client.Pid (client.go).
func (sc *SharedClient) Pid() int {
	rc := sc.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// Shutdown kills the plugin subprocess — every Kind currently dispensed off
// this SharedClient goes down with it, since they all share the same
// underlying *plugin.Client. Callers must only invoke this once the LAST
// reference across every Kind dispensed against this pluginID has been
// released (pluginproc's shared pool owns that bookkeeping).
func (sc *SharedClient) Shutdown(ctx context.Context) error {
	sc.pc.Kill()
	return nil
}
