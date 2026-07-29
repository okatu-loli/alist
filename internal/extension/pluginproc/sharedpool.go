package pluginproc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// assistPseudoKind is how sharedEntry.dispensed tracks "assist has been
// dispensed off this entry" alongside the 14 real ext.Kind values — assist
// itself is never one (it is not declared in Manifest.Kinds; see
// ext/assist.go), so a value guaranteed never to collide with a real Kind
// string is used as its map key instead of adding a parallel bool field.
const assistPseudoKind ext.Kind = "__assist__"

// errNoSharedEntryToBootstrapFrom is acquireSharedKind's signal that no
// shared entry exists yet for pluginID and the requested Kind cannot
// bootstrap one on its own (assist only — see AcquireAssist). The caller
// falls back to the dedicated, non-shared pool for that one Kind.
var errNoSharedEntryToBootstrapFrom = errors.New("pluginproc: no shared entry to dispense from; Kind cannot bootstrap one")

// sharedEntry is one pluginID's shared, multi-Kind subprocess (G2). Every
// Kind ever Acquired against this pluginID under Manager.SharedProcess
// shares this ONE entry: one underlying host.SharedClient, one aggregate
// refcount across every Kind, one idle-linger decision. Mirrors poolEntry
// (pool.go) in shape, generalized from "one Kind's client" to "one shared
// client several Kinds dispense off".
type sharedEntry struct {
	client      *host.SharedClient
	refs        int
	generation  uint64
	lingerTimer *time.Timer
	// dispensed records which Kinds (plus assistPseudoKind) have been
	// successfully dispensed off this entry's current generation — reset to
	// empty on every relaunch (evict, restart, or self-heal-on-acquire).
	// Backs both Count()'s per-Kind semantics and lingerFor's "did any
	// lingering Kind touch this process" decision.
	dispensed map[ext.Kind]bool
	// wantsLinger is sticky once set: once ANY lingering Kind (assist,
	// archiveTool) has been dispensed off this entry, the WHOLE shared
	// process gets idle-linger treatment on refs->0, even if the caller that
	// drops the last ref only ever touched a non-lingering Kind — the brief
	// ("linger 取该 pluginID 已 dispense 的所有 Kind 的 max") reads as "if
	// anything lingering ever rode this process, treat the process as
	// lingering," not "only if the LAST release happened to be a lingering
	// Kind."
	wantsLinger bool
}

// launchSharedFunc starts a fresh shared subprocess for pluginID, bootstrapping
// its manifest off primaryKind (see host.LaunchShared).
type launchSharedFunc func(ctx context.Context, pluginID, binPath string, primaryKind ext.Kind) (*host.SharedClient, error)

// sharedPool is Manager's G2 shared-process engine: active only when
// Manager.SharedProcess is true. Keyed by pluginID (not pluginID×Kind, unlike
// every pool[C] in pool.go) — see sharedEntry.
type sharedPool struct {
	mu      sync.Mutex
	entries map[string]*sharedEntry
	launch  launchSharedFunc

	// assistIdleLinger/archiveToolIdleLinger read Manager's fields live (same
	// dynamic-lookup shape pool[C]'s linger func uses), so a test shrinking
	// either window after New() still takes effect.
	assistIdleLinger      func() time.Duration
	archiveToolIdleLinger func() time.Duration
}

func newSharedPool(launch launchSharedFunc, assistIdleLinger, archiveToolIdleLinger func() time.Duration) *sharedPool {
	return &sharedPool{
		entries:               map[string]*sharedEntry{},
		launch:                launch,
		assistIdleLinger:      assistIdleLinger,
		archiveToolIdleLinger: archiveToolIdleLinger,
	}
}

// hasEntry reports whether a shared entry already exists for pluginID —
// used only by AcquireAssist to decide whether to even attempt the shared
// path (see errNoSharedEntryToBootstrapFrom's doc comment); acquireSharedKind
// re-checks under its own lock regardless, so this is an optimization, not a
// correctness dependency.
func (sp *sharedPool) hasEntry(pluginID string) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	_, ok := sp.entries[pluginID]
	return ok
}

// acquireSharedKind is every shared-mode AcquireXxx's common body: get or
// create pluginID's shared entry, dispense kind's client off it, bump the
// entry's aggregate refcount, and hand back a release func that drops it
// again. dispense is the Kind-specific SharedClient.DispenseXxx call; kind
// is used both for bookkeeping (dispensed/Count) and, when a fresh entry
// must be launched, as the primaryKind host.LaunchShared bootstraps its
// manifest from — which is why assist (canBootstrap=false) must never reach
// the "launch fresh" branch: it has no manifest RPC of its own to bootstrap
// with (see AcquireAssist for the fallback this implies).
func acquireSharedKind[C any](
	sp *sharedPool,
	ctx context.Context, pluginID, binPath string,
	kind ext.Kind,
	dispense func(*host.SharedClient) (C, error),
	wantsLinger bool,
	canBootstrap bool,
) (C, func(), error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var zero C

	e, ok := sp.entries[pluginID]
	if !ok {
		if !canBootstrap {
			return zero, nil, errNoSharedEntryToBootstrapFrom
		}
		client, err := sp.launch(ctx, pluginID, binPath, kind)
		if err != nil {
			return zero, nil, err
		}
		c, err := dispense(client)
		if err != nil {
			_ = client.Shutdown(context.Background())
			return zero, nil, err
		}
		e = &sharedEntry{client: client, refs: 1, dispensed: map[ext.Kind]bool{kind: true}, wantsLinger: wantsLinger}
		sp.entries[pluginID] = e
		return c, sp.release(pluginID), nil
	}

	if e.lingerTimer != nil {
		e.lingerTimer.Stop()
		e.lingerTimer = nil
	}
	if e.client.Exited() {
		if !canBootstrap {
			// The shared process died and assist alone can't resurrect it;
			// give up cleanly rather than guess a primaryKind. A caller
			// still holding some other Kind's reference against the (now
			// dead) old entry will self-heal it on ITS next Acquire.
			return zero, nil, errNoSharedEntryToBootstrapFrom
		}
		old := e.client
		fresh, err := sp.launch(ctx, pluginID, binPath, kind)
		if err != nil {
			return zero, nil, err
		}
		_ = old.Shutdown(context.Background())
		e.client = fresh
		e.generation++
		e.dispensed = map[ext.Kind]bool{}
		e.wantsLinger = false
	}

	c, err := dispense(e.client)
	if err != nil {
		return zero, nil, err
	}
	e.refs++
	e.dispensed[kind] = true
	if wantsLinger {
		e.wantsLinger = true
	}
	return c, sp.release(pluginID), nil
}

// release drops one reference off pluginID's shared entry (whichever Kind's
// Acquire produced it — refs are aggregated across every Kind, so this is
// identical regardless of which dispense func created the ref). Mirrors
// pool.release's shape and its lock-released-before-Shutdown invariant.
func (sp *sharedPool) release(pluginID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			sp.mu.Lock()
			e, ok := sp.entries[pluginID]
			if !ok {
				sp.mu.Unlock()
				return
			}
			e.refs--
			if e.refs > 0 {
				sp.mu.Unlock()
				return
			}
			if e.wantsLinger {
				if linger := sp.lingerFor(e); linger > 0 {
					sp.scheduleLinger(pluginID, e, linger)
					sp.mu.Unlock()
					return
				}
			}
			delete(sp.entries, pluginID)
			client := e.client
			sp.mu.Unlock()
			_ = client.Shutdown(context.Background())
		})
	}
}

// lingerFor is the "max over every lingering Kind this entry has dispensed"
// computation the brief asks for: assist and archiveTool are the only two
// lingering Kinds today: 30s each by default (assistIdleLingerDefault),
// independently configurable/shrinkable via Manager's two linger fields.
func (sp *sharedPool) lingerFor(e *sharedEntry) time.Duration {
	var d time.Duration
	if e.dispensed[assistPseudoKind] {
		if v := sp.assistIdleLinger(); v > d {
			d = v
		}
	}
	if e.dispensed[ext.KindArchiveTool] {
		if v := sp.archiveToolIdleLinger(); v > d {
			d = v
		}
	}
	return d
}

// scheduleLinger arms the idle-shutdown timer for an entry that just dropped
// to zero references. Caller must hold sp.mu. Identical race-safety shape to
// pool.scheduleLinger (pool.go) — see its doc comment for the full
// generation-guard rationale and the lock-released-before-Shutdown invariant.
func (sp *sharedPool) scheduleLinger(pluginID string, e *sharedEntry, linger time.Duration) {
	gen := e.generation
	e.lingerTimer = time.AfterFunc(linger, func() {
		sp.mu.Lock()
		cur, ok := sp.entries[pluginID]
		if !ok || cur != e || cur.refs > 0 || cur.generation != gen {
			sp.mu.Unlock()
			return
		}
		delete(sp.entries, pluginID)
		client := cur.client
		sp.mu.Unlock()
		_ = client.Shutdown(context.Background())
	})
}

// restart is Manager.Restart's shared-mode body (StorageDriver only, called
// from bridge.pluginDriver after an operation fails against a client it
// observed as dead). Because every Kind shares one process here, "restart
// the StorageDriver client" means relaunching the WHOLE shared entry — there
// is no narrower unit to restart.
func (sp *sharedPool) restart(ctx context.Context, pluginID, binPath string, stale *host.Client) (*host.Client, error) {
	sp.mu.Lock()
	e, ok := sp.entries[pluginID]
	if !ok {
		sp.mu.Unlock()
		return nil, fmt.Errorf("pluginproc: restart %q: no active shared process (all references already released)", pluginID)
	}
	// DispenseStorageDriver on an already-dispensed Kind is a pure cache hit
	// inside host.SharedClient (see its dispenseOnce) — it never re-dispenses,
	// so this never blocks or fails just because the process died; it simply
	// returns whatever *host.Client is already cached, which is exactly what
	// the race-collapse check below needs to compare against stale.
	if cur, err := e.client.DispenseStorageDriver(ctx); err == nil && cur != stale {
		sp.mu.Unlock()
		return cur, nil
	}
	if e.lingerTimer != nil {
		e.lingerTimer.Stop()
		e.lingerTimer = nil
	}
	old := e.client
	fresh, err := sp.launch(ctx, pluginID, binPath, ext.KindStorageDriver)
	if err != nil {
		sp.mu.Unlock()
		return nil, fmt.Errorf("pluginproc: restart shared %q: %w", pluginID, err)
	}
	e.client = fresh
	e.generation++
	e.dispensed = map[ext.Kind]bool{}
	newC, dispenseErr := fresh.DispenseStorageDriver(ctx)
	if dispenseErr == nil {
		e.dispensed[ext.KindStorageDriver] = true
	}
	sp.mu.Unlock()
	_ = old.Shutdown(context.Background())
	if dispenseErr != nil {
		return nil, dispenseErr
	}
	return newC, nil
}

// evict is Manager.Evict's shared-mode body: relaunch pluginID's ONE shared
// entry so every Kind currently dispensed off it (not just StorageDriver)
// picks up fresh authority. A no-op when nothing is cached. Mirrors
// pool.evict's refs<=0 short-circuit (tear down rather than relaunch an
// entry nobody references) and its disarm-before-relaunch handling of a
// pending linger timer.
func (sp *sharedPool) evict(ctx context.Context, pluginID, binPath string) error {
	sp.mu.Lock()
	e, ok := sp.entries[pluginID]
	if !ok {
		sp.mu.Unlock()
		return nil
	}
	if e.lingerTimer != nil {
		e.lingerTimer.Stop()
		e.lingerTimer = nil
	}
	if e.refs <= 0 {
		delete(sp.entries, pluginID)
		client := e.client
		sp.mu.Unlock()
		_ = client.Shutdown(context.Background())
		return nil
	}
	primary, ok := firstRealKind(e.dispensed)
	if !ok {
		sp.mu.Unlock()
		return fmt.Errorf("pluginproc: evict %q: shared entry has no dispensed primary Kind to relaunch from", pluginID)
	}
	old := e.client
	fresh, err := sp.launch(ctx, pluginID, binPath, primary)
	if err != nil {
		sp.mu.Unlock()
		return err
	}
	e.client = fresh
	e.generation++
	// Write primary back into dispensed instead of leaving it empty: fresh
	// (host.LaunchShared) already dispensed and cached primary internally as
	// part of its own manifest-bootstrap step, so this isn't re-dispensing,
	// just reflecting that in the entry's own bookkeeping. Review round 1,
	// item 1 (CONFIRMED bug): leaving this map empty meant a SECOND Evict
	// with no Acquire in between (the real trigger: an admin saving a
	// plugin's Grant twice in a row) found firstRealKind(e.dispensed) empty
	// and failed outright, even though the entry was alive and perfectly
	// evictable.
	e.dispensed = map[ext.Kind]bool{primary: true}
	sp.mu.Unlock()
	_ = old.Shutdown(context.Background())
	return nil
}

// firstRealKind returns any real ext.Kind (never assistPseudoKind) recorded
// in dispensed, so evict has something to bootstrap a relaunch's manifest
// from. Map iteration order is unspecified but irrelevant here: every
// dispensed real Kind's GetManifest RPC returns the identical manifest.
func firstRealKind(dispensed map[ext.Kind]bool) (ext.Kind, bool) {
	for k := range dispensed {
		if k != assistPseudoKind {
			return k, true
		}
	}
	return "", false
}

// count reports the number of pluginIDs whose shared entry has dispensed
// kind at least once — sharedPool's counterpart to pool.count(), adapted to
// the fact that one entry now serves several Kinds (see Manager.Count's doc
// comment on the resulting "dispense refcount, not process count" semantics
// change).
func (sp *sharedPool) count(kind ext.Kind) int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	n := 0
	for _, e := range sp.entries {
		if e.dispensed[kind] {
			n++
		}
	}
	return n
}
