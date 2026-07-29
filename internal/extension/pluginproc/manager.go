// Package pluginproc manages gRPC plugin subprocesses shared across storages.
// A plugin binary is launched at most once per plugin ID; every storage
// mounted on that plugin acquires a reference to the same host.Client, and
// the subprocess is killed only when the last reference is released.
//
// A plugin subprocess can also die on its own (panic, OOM kill, segfault) —
// nothing forces a clean Drop/release in that case, so the reference count
// never drops to zero and the dead client would otherwise sit in procs
// forever, serving every subsequent call a connection error. Manager detects
// that (via host.Client.Exited, itself backed by go-plugin's own
// process-reaper goroutine) and relaunches transparently: lazily, when the
// next caller asks for the client (Acquire on a cache hit, or an explicit
// Restart from bridge.pluginDriver after a call fails against the dead
// process) — see Acquire and Restart.
//
// Every Kind's launch/reuse/refcount/relaunch/linger lifecycle is the exact
// same state machine over a different host.*Client type — see pool[C] in
// pool.go. Manager is fifteen instances of that engine (one per Kind) behind
// per-Kind Acquire*/*.Count/relaunch wrappers that keep each Kind's existing
// exported surface unchanged.
//
// G2 adds a second, OPT-IN engine alongside those fifteen: when
// Manager.SharedProcess is true, every Kind Acquired for the same pluginID
// shares ONE subprocess (host.SharedClient, dispensing several Kinds'
// services off one go-plugin connection) instead of one subprocess per Kind
// — see sharedPool in sharedpool.go. SharedProcess defaults to false, so the
// fifteen-pools behavior above is completely unchanged unless a caller
// explicitly opts in.
package pluginproc

import (
	"context"
	"errors"
	"os/exec"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// assistIdleLingerDefault is the production value of Manager.assistIdleLinger
// (see AcquireAssist and the assistProcs pool's linger func in New()): how
// long an idle, zero-ref assist subprocess is kept warm before actually
// being shut down.
const assistIdleLingerDefault = 30 * time.Second

// Manager launches and reuses plugin subprocesses, one per plugin ID, shared
// across all storages mounted on that plugin (gRPC multiplexes by storageID).
// AuthProvider and AuthNMethod plugins share the same Manager (separate
// pools, keyed the same way) so every Kind gets the identical
// launch/reuse/refcount discipline via pool[C] (pool.go).
type Manager struct {
	procs                    *pool[*host.Client]
	authProcs                *pool[*host.AuthClient]
	authNProcs               *pool[*host.AuthNClient]
	bgProcs                  *pool[*host.BgClient]
	apiRouteProcs            *pool[*host.APIRouteClient]
	actionProcs              *pool[*host.ActionClient]
	eventHookProcs           *pool[*host.EventHookClient]
	scheduledJobProcs        *pool[*host.ScheduledJobClient]
	notificationProcs        *pool[*host.NotificationClient]
	offlineDownloadToolProcs *pool[*host.OfflineDownloadToolClient]
	searchIndexerProcs       *pool[*host.SearchIndexerClient]
	archiveToolProcs         *pool[*host.ArchiveToolClient]
	fileDecoratorProcs       *pool[*host.FileDecoratorClient]
	accessProtocolProcs      *pool[*host.AccessProtocolClient]
	assistProcs              *pool[*host.AssistClient]

	// assistIdleLinger is how long an unreferenced assist subprocess is kept
	// alive before it is actually shut down (see assistProcs' linger func).
	// Defaults to assistIdleLingerDefault; tests in this package shrink it
	// directly to avoid sleeping the real 30s.
	assistIdleLinger time.Duration
	// archiveToolIdleLinger is assistIdleLinger's counterpart for
	// archiveToolProcs (review fix 3, Phase E wave 2) -- kept as its own field
	// (not shared with assistIdleLinger) so a test shrinking one Kind's linger
	// window never affects the other's. Defaults to assistIdleLingerDefault.
	archiveToolIdleLinger time.Duration

	// hostBuilder, when set, builds the per-plugin reverse ext.Host a launched
	// StorageDriver subprocess borrows (F0 reverse bridge). nil (the default)
	// means no reverse host — launches keep pre-bridge behavior (plugin's Init
	// is not called). See SetHostBuilder and launchHostBuilder.
	hostBuilder func(pluginID string) ext.Host

	// SharedProcess is the G2 single-process, multi-Kind dispense gate.
	// false (the default, zero value) keeps every AcquireXxx/Restart/Evict/
	// *Count call routed through this Kind's own dedicated pool[C] above —
	// byte-identical to pre-G2 behavior, so leaving this unset (or flipping
	// it back to false) is a zero-cost rollback. true routes every Kind's
	// Acquire for the same pluginID through the shared pool (sharedpool.go)
	// instead: one subprocess, one aggregate refcount, per pluginID. Meant
	// to be set once at boot (a deployment-wide rollout gate), not toggled
	// per call.
	SharedProcess bool

	shared *sharedPool
}

func New() *Manager {
	m := &Manager{
		assistIdleLinger:      assistIdleLingerDefault,
		archiveToolIdleLinger: assistIdleLingerDefault,
	}
	m.procs = newPool[*host.Client]("storage driver", func(ctx context.Context, pluginID, binPath string) (*host.Client, error) {
		return host.Launch(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.authProcs = newPool[*host.AuthClient]("auth provider", func(ctx context.Context, pluginID, binPath string) (*host.AuthClient, error) {
		return host.LaunchAuthProvider(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.authNProcs = newPool[*host.AuthNClient]("authn method", func(ctx context.Context, pluginID, binPath string) (*host.AuthNClient, error) {
		return host.LaunchAuthNMethod(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.bgProcs = newPool[*host.BgClient]("background service", func(ctx context.Context, pluginID, binPath string) (*host.BgClient, error) {
		return host.LaunchBackgroundService(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.apiRouteProcs = newPool[*host.APIRouteClient]("api route", func(ctx context.Context, pluginID, binPath string) (*host.APIRouteClient, error) {
		return host.LaunchAPIRoute(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.actionProcs = newPool[*host.ActionClient]("action", func(ctx context.Context, pluginID, binPath string) (*host.ActionClient, error) {
		return host.LaunchAction(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.eventHookProcs = newPool[*host.EventHookClient]("event hook", func(ctx context.Context, pluginID, binPath string) (*host.EventHookClient, error) {
		return host.LaunchEventHook(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.scheduledJobProcs = newPool[*host.ScheduledJobClient]("scheduled job", func(ctx context.Context, pluginID, binPath string) (*host.ScheduledJobClient, error) {
		return host.LaunchScheduledJob(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.notificationProcs = newPool[*host.NotificationClient]("notification", func(ctx context.Context, pluginID, binPath string) (*host.NotificationClient, error) {
		return host.LaunchNotification(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.offlineDownloadToolProcs = newPool[*host.OfflineDownloadToolClient]("offline download tool", func(ctx context.Context, pluginID, binPath string) (*host.OfflineDownloadToolClient, error) {
		return host.LaunchOfflineDownloadTool(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.searchIndexerProcs = newPool[*host.SearchIndexerClient]("search indexer", func(ctx context.Context, pluginID, binPath string) (*host.SearchIndexerClient, error) {
		return host.LaunchSearchIndexer(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.archiveToolProcs = newPool[*host.ArchiveToolClient]("archive tool", func(ctx context.Context, pluginID, binPath string) (*host.ArchiveToolClient, error) {
		return host.LaunchArchiveTool(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, func() time.Duration { return m.archiveToolIdleLinger })
	m.fileDecoratorProcs = newPool[*host.FileDecoratorClient]("file decorator", func(ctx context.Context, pluginID, binPath string) (*host.FileDecoratorClient, error) {
		return host.LaunchFileDecorator(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.accessProtocolProcs = newPool[*host.AccessProtocolClient]("access protocol", func(ctx context.Context, pluginID, binPath string) (*host.AccessProtocolClient, error) {
		return host.LaunchAccessProtocol(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, nil)
	m.assistProcs = newPool[*host.AssistClient]("assist", func(ctx context.Context, pluginID, binPath string) (*host.AssistClient, error) {
		return host.LaunchAssist(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID))
	}, func() time.Duration { return m.assistIdleLinger })
	m.shared = newSharedPool(
		func(ctx context.Context, pluginID, binPath string, primaryKind ext.Kind) (*host.SharedClient, error) {
			return host.LaunchShared(ctx, exec.Command(binPath), m.launchHostBuilder(pluginID), primaryKind)
		},
		func() time.Duration { return m.assistIdleLinger },
		func() time.Duration { return m.archiveToolIdleLinger },
	)
	return m
}

// Default is the process-wide manager the driver factory acquires from.
var Default = New()

// SetHostBuilder installs the per-plugin reverse-host factory (boot wiring).
// nil (default) means no reverse host — the plugin's Init is not called and it
// cannot borrow host capabilities (pre-bridge behavior, kept for tests).
func (m *Manager) SetHostBuilder(b func(pluginID string) ext.Host) { m.hostBuilder = b }

// launchHostBuilder adapts m.hostBuilder into the func() ext.Host shape
// host.Launch wants, closing over pluginID. Returns nil (not a func returning
// nil) when m.hostBuilder is unset, so host.Launch's own nil check keeps
// launches with no reverse host on the pre-bridge path.
func (m *Manager) launchHostBuilder(pluginID string) func() ext.Host {
	if m.hostBuilder == nil {
		return nil
	}
	return func() ext.Host { return m.hostBuilder(pluginID) }
}

// Acquire returns a shared client for pluginID (launching binPath on first use)
// plus a release func; the subprocess is killed when the last release runs.
//
// If the cached subprocess has already died since it was last touched (e.g.
// no storage mounted on this pluginID has made a call since the crash, so
// nothing has gone through the Restart path yet), Acquire relaunches it
// before handing out the client — a fresh or returning mount never starts
// against a corpse. The Exited() check this adds is a single mutex+bool read
// on the (rare) cache-hit path only; it does not run on any per-call hot
// path (List/Link/Put/...), which never call Acquire again after Init.
func (m *Manager) Acquire(ctx context.Context, pluginID, binPath string) (ext.StorageDriver, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindStorageDriver,
			func(sc *host.SharedClient) (*host.Client, error) { return sc.DispenseStorageDriver(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.procs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// A literal nil, not a naked "return c, release, err" forward: c here
		// is the concrete *host.Client zero value (nil pointer). Boxing that
		// nil pointer straight into the ext.StorageDriver return type would
		// produce a NON-nil interface (typed nil: dynamic type *host.Client,
		// dynamic value nil) — every caller's `driver != nil` check would
		// then see true and go on to call methods on a nil *host.Client. The
		// pre-generics code never had this trap because each Kind's Acquire*
		// was hand-written to `return nil, nil, err` on every error path.
		return nil, nil, err
	}
	return c, release, nil
}

// Restart relaunches pluginID's subprocess after a caller (bridge.pluginDriver,
// on an operation that failed against a crashed process) has detected the
// crash. staleClient must be the client the caller observed as dead; if
// another caller already won the race and replaced it — e.g. a sibling
// storage sharing this pluginID detected the same crash first and restarted
// it already — Restart returns that replacement without relaunching a second
// time, collapsing concurrent restarts from multiple storages into a single
// subprocess launch. Returns a plain error (never panics or retries in a
// loop) if the relaunch itself fails, e.g. the binary is gone.
func (m *Manager) Restart(ctx context.Context, pluginID, binPath string, staleClient ext.StorageDriver) (ext.StorageDriver, error) {
	// A staleClient whose dynamic type isn't *host.Client (including a nil
	// interface) asserts to the zero value here, which can never equal a
	// live cached *host.Client — matching the original concrete-vs-interface
	// `e.client != staleClient` comparison this replaces: any type mismatch
	// simply fails to match, never relaunches, and never panics.
	stale, _ := staleClient.(*host.Client)
	if m.SharedProcess {
		c, err := m.shared.restart(ctx, pluginID, binPath, stale)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	c, err := m.procs.restart(ctx, pluginID, binPath, stale)
	if err != nil {
		// Literal nil on error — see Acquire's doc comment for why this
		// can't be a naked "return c, err" forward of the pool's typed zero
		// value.
		return nil, err
	}
	return c, nil
}

// Evict force-relaunches every Kind's cached subprocess for pluginID (not
// just StorageDriver) so each one picks up out-of-band authority changes
// that are only read at launch — e.g. an admin's SetGrant call, since the
// borrowed-capability layer closes over Grant.Network when a subprocess
// starts. A plugin can run several Kinds' subprocesses concurrently (its
// StorageDriver process, a dedicated Action process, an Assist process, ...)
// and every one of them was launched under whatever Grant was active at ITS
// launch time — narrowing that Grant later must not leave any of them still
// running under the old, wider authority. binPath is used for every pool
// that has something cached for pluginID; a pool with nothing cached is
// left alone (there is nothing running against the old authority in it, so
// its next Acquire simply launches fresh with current authority anyway).
//
// Existing reference holders keep their ref count against the (now fresh)
// entry in each relaunched pool; a caller mid-Restart against a client it
// observed as stale finds the entry already replaced and reuses it, same
// race-collapse as two concurrent Restarts.
//
// The first pool relaunch that fails aborts the rest and returns that error;
// pools already relaunched before the failure stay on their fresh process
// (only the remaining ones are left on stale authority, to be picked up by
// their own next Acquire's self-heal path if their process happens to die,
// or by a subsequent Evict call).
func (m *Manager) Evict(ctx context.Context, pluginID, binPath string) error {
	if m.SharedProcess {
		if err := m.shared.evict(ctx, pluginID, binPath); err != nil {
			return err
		}
		// assistProcs is still consulted: AcquireAssist falls back to it
		// when no shared entry exists yet to dispense assist from (assist
		// cannot bootstrap one on its own — see AcquireAssist), so a
		// pluginID whose only activity so far was an assist call may have
		// its subprocess parked there instead of in the shared pool.
		return m.assistProcs.evict(ctx, pluginID, binPath)
	}
	for _, evict := range []func(context.Context, string, string) error{
		m.procs.evict,
		m.authProcs.evict,
		m.authNProcs.evict,
		m.bgProcs.evict,
		m.apiRouteProcs.evict,
		m.actionProcs.evict,
		m.eventHookProcs.evict,
		m.scheduledJobProcs.evict,
		m.notificationProcs.evict,
		m.offlineDownloadToolProcs.evict,
		m.searchIndexerProcs.evict,
		m.archiveToolProcs.evict,
		m.fileDecoratorProcs.evict,
		m.accessProtocolProcs.evict,
		m.assistProcs.evict,
	} {
		if err := evict(ctx, pluginID, binPath); err != nil {
			return err
		}
	}
	return nil
}

// Count reports the number of currently live plugin subprocesses under the
// non-shared path, or (Manager.SharedProcess) the number of pluginIDs whose
// shared entry has dispensed StorageDriver — "how many StorageDriver
// dispense references exist," not "how many OS processes," once several
// Kinds can share one process (see the package doc comment).
func (m *Manager) Count() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindStorageDriver)
	}
	return m.procs.count()
}

// AcquireAuthProvider is Acquire's counterpart for a Kind=AuthProvider plugin
// (WS-8): launches binPath on first use, reuses the shared process on
// subsequent calls for the same pluginID, and relaunches transparently if the
// cached subprocess has already died (same self-heal-on-cache-hit rationale as
// Acquire — see its doc comment).
func (m *Manager) AcquireAuthProvider(ctx context.Context, pluginID, binPath string) (ext.AuthProvider, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindAuthProvider,
			func(sc *host.SharedClient) (*host.AuthClient, error) { return sc.DispenseAuthProvider(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.authProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// AuthCount reports the number of currently live AuthProvider plugin
// subprocesses (kept separate from Count so existing StorageDriver process
// tests are unaffected by AuthProvider activity, and vice versa). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) AuthCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindAuthProvider)
	}
	return m.authProcs.count()
}

// AcquireAuthNMethod is Acquire's counterpart for a Kind=AuthNMethod plugin
// (login methods): launches binPath on first use, reuses the shared process on
// subsequent calls for the same pluginID, and relaunches transparently if the
// cached subprocess has already died (same self-heal-on-cache-hit rationale as
// Acquire — see its doc comment).
func (m *Manager) AcquireAuthNMethod(ctx context.Context, pluginID, binPath string) (ext.AuthNMethod, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindAuthNMethod,
			func(sc *host.SharedClient) (*host.AuthNClient, error) { return sc.DispenseAuthNMethod(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.authNProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// AuthNCount reports the number of currently live AuthNMethod plugin
// subprocesses (kept separate from Count/AuthCount so each Kind's process tests
// are unaffected by the others' activity). Under Manager.SharedProcess, see
// Count's doc comment for the "dispense reference" semantics change.
func (m *Manager) AuthNCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindAuthNMethod)
	}
	return m.authNProcs.count()
}

// AcquireBackgroundService is Acquire's counterpart for a Kind=BackgroundService
// plugin (FRP tunnel, sync engine): launches binPath on first use, reuses the
// shared process on subsequent calls for the same pluginID, and relaunches
// transparently if the cached subprocess has already died (same
// self-heal-on-cache-hit rationale as Acquire — see its doc comment).
func (m *Manager) AcquireBackgroundService(ctx context.Context, pluginID, binPath string) (ext.BackgroundService, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindBackgroundService,
			func(sc *host.SharedClient) (*host.BgClient, error) { return sc.DispenseBackgroundService(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.bgProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// BgCount reports the number of currently live BackgroundService plugin
// subprocesses (kept separate from Count/AuthCount/AuthNCount so each Kind's
// process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) BgCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindBackgroundService)
	}
	return m.bgProcs.count()
}

// AcquireAPIRoute is Acquire's counterpart for a Kind=APIRoute plugin (W8a):
// launches binPath on first use, reuses the shared process on subsequent calls
// for the same pluginID, and relaunches transparently if the cached subprocess
// has already died (same self-heal-on-cache-hit rationale as Acquire — see its
// doc comment).
func (m *Manager) AcquireAPIRoute(ctx context.Context, pluginID, binPath string) (ext.APIRoute, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindAPIRoute,
			func(sc *host.SharedClient) (*host.APIRouteClient, error) { return sc.DispenseAPIRoute(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.apiRouteProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// APIRouteCount reports the number of currently live APIRoute plugin
// subprocesses (kept separate from Count/AuthCount/AuthNCount/BgCount so each
// Kind's process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) APIRouteCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindAPIRoute)
	}
	return m.apiRouteProcs.count()
}

// AcquireAction is Acquire's counterpart for a Kind=Action plugin (W8b):
// launches binPath on first use, reuses the shared process on subsequent calls
// for the same pluginID, and relaunches transparently if the cached subprocess
// has already died (same self-heal-on-cache-hit rationale as Acquire — see its
// doc comment).
func (m *Manager) AcquireAction(ctx context.Context, pluginID, binPath string) (ext.Action, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindAction,
			func(sc *host.SharedClient) (*host.ActionClient, error) { return sc.DispenseAction(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.actionProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// ActionCount reports the number of currently live Action plugin subprocesses
// (kept separate from Count/AuthCount/AuthNCount/BgCount/APIRouteCount so each
// Kind's process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) ActionCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindAction)
	}
	return m.actionProcs.count()
}

// AcquireEventHook is Acquire's counterpart for a Kind=EventHook plugin (W7):
// launches binPath on first use, reuses the shared process on subsequent
// calls for the same pluginID, and relaunches transparently if the cached
// subprocess has already died (same self-heal-on-cache-hit rationale as
// Acquire — see its doc comment).
func (m *Manager) AcquireEventHook(ctx context.Context, pluginID, binPath string) (ext.EventHook, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindEventHook,
			func(sc *host.SharedClient) (*host.EventHookClient, error) { return sc.DispenseEventHook(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.eventHookProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// EventHookCount reports the number of currently live EventHook plugin
// subprocesses (kept separate from the other Kind counts so each Kind's
// process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) EventHookCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindEventHook)
	}
	return m.eventHookProcs.count()
}

// AcquireScheduledJob is Acquire's counterpart for a Kind=ScheduledJob plugin
// (W7): launches binPath on first use, reuses the shared process on
// subsequent calls for the same pluginID, and relaunches transparently if the
// cached subprocess has already died (same self-heal-on-cache-hit rationale
// as Acquire — see its doc comment).
func (m *Manager) AcquireScheduledJob(ctx context.Context, pluginID, binPath string) (ext.ScheduledJob, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindScheduledJob,
			func(sc *host.SharedClient) (*host.ScheduledJobClient, error) { return sc.DispenseScheduledJob(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.scheduledJobProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// ScheduledJobCount reports the number of currently live ScheduledJob plugin
// subprocesses (kept separate from the other Kind counts so each Kind's
// process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) ScheduledJobCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindScheduledJob)
	}
	return m.scheduledJobProcs.count()
}

// AcquireNotification is Acquire's counterpart for a Kind=NotificationChannel
// plugin (W7b): launches binPath on first use, reuses the shared process on
// subsequent calls for the same pluginID, and relaunches transparently if the
// cached subprocess has already died (same self-heal-on-cache-hit rationale
// as Acquire — see its doc comment).
func (m *Manager) AcquireNotification(ctx context.Context, pluginID, binPath string) (ext.Notification, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindNotification,
			func(sc *host.SharedClient) (*host.NotificationClient, error) { return sc.DispenseNotification(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.notificationProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// NotificationCount reports the number of currently live NotificationChannel
// plugin subprocesses (kept separate from the other Kind counts so each
// Kind's process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) NotificationCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindNotification)
	}
	return m.notificationProcs.count()
}

// AcquireOfflineDownloadTool is Acquire's counterpart for a
// Kind=OfflineDownloadTool plugin (Phase E ring 1): launches binPath on first
// use, reuses the shared process on subsequent calls for the same pluginID,
// and relaunches transparently if the cached subprocess has already died
// (same self-heal-on-cache-hit rationale as Acquire — see its doc comment).
func (m *Manager) AcquireOfflineDownloadTool(ctx context.Context, pluginID, binPath string) (ext.OfflineDownloadTool, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindOfflineDownloadTool,
			func(sc *host.SharedClient) (*host.OfflineDownloadToolClient, error) {
				return sc.DispenseOfflineDownloadTool(ctx)
			},
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.offlineDownloadToolProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// OfflineDownloadToolCount reports the number of currently live
// OfflineDownloadTool plugin subprocesses (kept separate from the other Kind
// counts so each Kind's process tests are unaffected by the others'
// activity). Under Manager.SharedProcess, see Count's doc comment for the
// "dispense reference" semantics change.
func (m *Manager) OfflineDownloadToolCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindOfflineDownloadTool)
	}
	return m.offlineDownloadToolProcs.count()
}

// AcquireSearchIndexer is Acquire's counterpart for a Kind=SearchIndexer
// plugin (Phase E ring 1b): launches binPath on first use, reuses the shared
// process on subsequent calls for the same pluginID, and relaunches
// transparently if the cached subprocess has already died (same
// self-heal-on-cache-hit rationale as Acquire — see its doc comment).
func (m *Manager) AcquireSearchIndexer(ctx context.Context, pluginID, binPath string) (ext.SearchIndexer, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindSearchIndexer,
			func(sc *host.SharedClient) (*host.SearchIndexerClient, error) { return sc.DispenseSearchIndexer(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.searchIndexerProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// SearchIndexerCount reports the number of currently live SearchIndexer
// plugin subprocesses (kept separate from the other Kind counts so each
// Kind's process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) SearchIndexerCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindSearchIndexer)
	}
	return m.searchIndexerProcs.count()
}

// AcquireArchiveTool is Acquire's counterpart for a Kind=ArchiveTool plugin
// (Phase E wave 2): launches binPath on first use, reuses the shared process
// on subsequent calls for the same pluginID, and relaunches transparently if
// the cached subprocess has already died (same self-heal-on-cache-hit
// rationale as Acquire — see its doc comment).
//
// Like AcquireAssist, a cache hit here also disarms any pending idle-linger
// timer on the reused entry: a process about to be shut down for being
// unreferenced is, by definition, no longer unreferenced once this call
// hands out a new reference. Releasing the last reference (the pool's linger
// func, wired to archiveToolIdleLinger in New()) arms that timer instead of
// shutting down synchronously — see pool.scheduleLinger's doc comment for
// the full race-safety rationale.
//
// Under Manager.SharedProcess, ArchiveTool is one of the two Kinds
// (alongside assist) whose linger preference is sticky for the WHOLE shared
// entry: once ArchiveTool has been dispensed off pluginID's shared process,
// that process gets idle-linger treatment on refs->0 even if the caller
// dropping the last ref only ever touched some other, non-lingering Kind —
// see sharedEntry.wantsLinger and sharedPool.lingerFor (sharedpool.go).
func (m *Manager) AcquireArchiveTool(ctx context.Context, pluginID, binPath string) (ext.ArchiveTool, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindArchiveTool,
			func(sc *host.SharedClient) (*host.ArchiveToolClient, error) { return sc.DispenseArchiveTool(ctx) },
			true, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.archiveToolProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// ArchiveToolCount reports the number of currently live ArchiveTool plugin
// subprocesses (kept separate from the other Kind counts so each Kind's
// process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) ArchiveToolCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindArchiveTool)
	}
	return m.archiveToolProcs.count()
}

// AcquireFileDecorator is Acquire's counterpart for a Kind=FileDecorator
// plugin (W9a): launches binPath on first use, reuses the shared process on
// subsequent calls for the same pluginID, and relaunches transparently if the
// cached subprocess has already died (same self-heal-on-cache-hit rationale
// as Acquire — see its doc comment).
func (m *Manager) AcquireFileDecorator(ctx context.Context, pluginID, binPath string) (ext.FileDecorator, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindFileDecorator,
			func(sc *host.SharedClient) (*host.FileDecoratorClient, error) { return sc.DispenseFileDecorator(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.fileDecoratorProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// FileDecoratorCount reports the number of currently live FileDecorator
// plugin subprocesses (kept separate from the other Kind counts so each
// Kind's process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) FileDecoratorCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindFileDecorator)
	}
	return m.fileDecoratorProcs.count()
}

// AcquireAccessProtocol is Acquire's counterpart for a Kind=AccessProtocol
// plugin's TransportHTTP half (W9a): launches binPath on first use, reuses
// the shared process on subsequent calls for the same pluginID, and
// relaunches transparently if the cached subprocess has already died (same
// self-heal-on-cache-hit rationale as Acquire — see its doc comment). A
// TransportConn (FTP/SFTP) protocol never reaches here — it is native/
// in-process only (design §5) and is never registered as an ExecGRPC entry.
func (m *Manager) AcquireAccessProtocol(ctx context.Context, pluginID, binPath string) (ext.AccessProtocol, func(), error) {
	if m.SharedProcess {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, ext.KindAccessProtocol,
			func(sc *host.SharedClient) (*host.AccessProtocolClient, error) { return sc.DispenseAccessProtocol(ctx) },
			false, true)
		if err != nil {
			return nil, nil, err
		}
		return c, release, nil
	}
	c, release, err := m.accessProtocolProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// AccessProtocolCount reports the number of currently live AccessProtocol
// plugin subprocesses (kept separate from the other Kind counts so each
// Kind's process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, see Count's doc comment for the "dispense
// reference" semantics change.
func (m *Manager) AccessProtocolCount() int {
	if m.SharedProcess {
		return m.shared.count(ext.KindAccessProtocol)
	}
	return m.accessProtocolProcs.count()
}

// AcquireAssist is Acquire's counterpart for the assist auxiliary service
// (assist gRPC leg, Task 2): launches binPath on first use, reuses the
// shared process on subsequent calls for the same pluginID, and relaunches
// transparently if the cached subprocess has already died (same
// self-heal-on-cache-hit rationale as Acquire — see its doc comment).
//
// This pool is kept separate from every other Kind's pool (procs,
// actionProcs, ...) even when the same pluginID also runs one of those: an
// assist dispense is always a dedicated subprocess (host.LaunchAssist only
// ever dispenses "assist" from it), so the impl inside it gets exactly one
// Init call, never a second one layered on top of a process some other Kind
// already initialized.
//
// Unlike most other Acquire*, a cache hit here also disarms any pending
// idle-linger timer on the reused entry: a process that was about to be shut
// down for being unreferenced is, by definition, no longer unreferenced once
// this call hands out a new reference. Releasing the last reference (the
// pool's linger func, wired to assistIdleLinger in New()) arms that timer
// instead of shutting down synchronously.
//
// Under Manager.SharedProcess, assist is the one Kind that CANNOT bootstrap
// a brand-new shared entry on its own: AssistService has no GetManifest RPC
// (see host.SharedClient.DispenseAssist), so there is nothing to learn a
// fresh subprocess's declared Kinds from if assist is the very first thing
// ever requested for pluginID. When a shared entry already exists (some
// other Kind got there first), this dispenses assist off it, sharing that
// process — the valuable case, since an admin's install-time "test
// connection" assist call and the plugin's primary Kind commonly both run
// soon after each other. When no shared entry exists yet, this falls back to
// the dedicated assistProcs pool below, unchanged from pre-G2 behavior — the
// same "an assist-only dispense is a dedicated subprocess" shape this pool's
// own doc comment above describes, just no longer the ONLY shape once some
// other Kind has already stood up a shared process for this pluginID.
func (m *Manager) AcquireAssist(ctx context.Context, pluginID, binPath string) (ext.Assist, func(), error) {
	if m.SharedProcess && m.shared.hasEntry(pluginID) {
		c, release, err := acquireSharedKind(m.shared, ctx, pluginID, binPath, assistPseudoKind,
			func(sc *host.SharedClient) (*host.AssistClient, error) { return sc.DispenseAssist(ctx) },
			true, false)
		if err == nil {
			return c, release, nil
		}
		if !errors.Is(err, errNoSharedEntryToBootstrapFrom) {
			return nil, nil, err
		}
		// The shared entry vanished (last ref released, or its process died
		// and self-heal needs a real Kind) between hasEntry's check and
		// here — fall through to the dedicated pool below exactly as if
		// hasEntry had reported false to begin with.
	}
	c, release, err := m.assistProcs.acquire(ctx, pluginID, binPath)
	if err != nil {
		// Literal nil — see Acquire's doc comment for why this can't be a
		// naked forward of the pool's typed zero value.
		return nil, nil, err
	}
	return c, release, nil
}

// AssistCount reports the number of currently live assist plugin
// subprocesses (kept separate from the other Kind counts so each Kind's
// process tests are unaffected by the others' activity). Under
// Manager.SharedProcess, assist can be dispensed off either home (see
// AcquireAssist), so this sums both: pluginIDs whose shared entry has
// dispensed assist, plus pluginIDs still on the dedicated assistProcs pool
// (the bootstrap-fallback case).
func (m *Manager) AssistCount() int {
	if m.SharedProcess {
		return m.shared.count(assistPseudoKind) + m.assistProcs.count()
	}
	return m.assistProcs.count()
}
