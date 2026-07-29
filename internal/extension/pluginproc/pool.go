package pluginproc

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// procClient is the shared shape every host.*Client type in this package
// implements: a way to notice its subprocess already died (Exited) and a way
// to tear it down (Shutdown). All 15 pools in Manager are built on it via
// pool[C] below, so the launch/reuse/refcount/relaunch/linger machinery is
// written exactly once instead of once per Kind.
//
// comparable is required so a pool can compare a caller-observed staleClient
// against its own cached client by identity (restart's race-collapse check)
// — every concrete C here is a pointer type (*host.Client, *host.AssistClient,
// ...), which is always comparable.
type procClient interface {
	comparable
	Exited() bool
	Shutdown(ctx context.Context) error
}

// poolEntry is one pluginID's cached subprocess plus its refcount. generation
// and lingerTimer are only ever populated for pools constructed with a
// non-nil linger func (assist, archiveTool as of this writing) — see
// pool.release/scheduleLinger; every other pool leaves them at their zero
// value and never reads them, so unifying the entry type across every Kind
// costs nothing for the pools that don't need the linger behavior.
type poolEntry[C procClient] struct {
	client C
	refs   int
	// generation counts relaunches of this entry's process. A lingerTimer
	// captures the generation it was armed for; if the process is relaunched
	// (self-heal-on-cache-hit in acquire, or an explicit restart/evict) before
	// the timer fires, the timer's callback finds a generation mismatch and
	// knows the process it was scheduled to kill is not the one now running,
	// so it does nothing.
	generation uint64
	// lingerTimer is the pending idle-shutdown timer while refs==0, or nil
	// once cancelled (re-Acquire reuse) or fired (already shut down). Only
	// ever armed by pools with a non-nil linger func.
	lingerTimer *time.Timer
}

// launchFunc starts a fresh subprocess for pluginID (binPath is the plugin
// binary) and returns its client. Each Manager pool captures its own Launch*
// call and hostBuilder wiring in one of these at construction (see New()).
type launchFunc[C procClient] func(ctx context.Context, pluginID, binPath string) (C, error)

// pool is the generic launch/reuse/refcount/relaunch/linger engine one Kind's
// map[pluginID]*entry + Acquire/relaunch/release/Count quintet used to be
// written out by hand for. kind names the Kind in relaunch error messages
// (e.g. "action", "assist") — purely cosmetic. linger, when non-nil, is
// called fresh every time a release drops refs to zero to decide how long to
// keep an idle, unreferenced subprocess warm before shutting it down; a nil
// linger func (every pool but assist/archiveTool) makes release shut down
// synchronously the instant refs hits zero, matching every Kind's original,
// pre-generic behavior.
type pool[C procClient] struct {
	mu     sync.Mutex
	procs  map[string]*poolEntry[C]
	launch launchFunc[C]
	kind   string
	linger func() time.Duration
}

func newPool[C procClient](kind string, launch launchFunc[C], linger func() time.Duration) *pool[C] {
	return &pool[C]{
		procs:  map[string]*poolEntry[C]{},
		launch: launch,
		kind:   kind,
		linger: linger,
	}
}

// acquire returns a shared client for pluginID (launching binPath on first
// use) plus a release func; the subprocess is killed (or, for a lingering
// pool, linger-scheduled) when the last release runs.
//
// A cache hit first disarms any pending idle-linger timer on the entry — a
// process about to be shut down for being unreferenced is, by definition, no
// longer unreferenced once this call hands out a new reference — then, same
// as every pool, relaunches transparently if the cached subprocess has
// already died since it was last touched, so a fresh or returning caller
// never starts against a corpse.
func (p *pool[C]) acquire(ctx context.Context, pluginID, binPath string) (C, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.procs[pluginID]; ok {
		if e.lingerTimer != nil {
			e.lingerTimer.Stop()
			e.lingerTimer = nil
		}
		if e.client.Exited() {
			c, err := p.relaunchLocked(ctx, pluginID, binPath, e.client)
			if err != nil {
				var zero C
				return zero, nil, err
			}
			e.client = c
			e.generation++
		}
		e.refs++
		return e.client, p.release(pluginID), nil
	}
	c, err := p.launch(ctx, pluginID, binPath)
	if err != nil {
		var zero C
		return zero, nil, err
	}
	p.procs[pluginID] = &poolEntry[C]{client: c, refs: 1}
	return c, p.release(pluginID), nil
}

// restart relaunches pluginID's subprocess after a caller has detected the
// stale client is dead. If another caller already won the race and replaced
// it, restart returns that replacement without relaunching a second time,
// collapsing concurrent restarts into a single subprocess launch.
func (p *pool[C]) restart(ctx context.Context, pluginID, binPath string, stale C) (C, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.procs[pluginID]
	if !ok {
		var zero C
		return zero, fmt.Errorf("pluginproc: restart %s %q: no active process (all references already released)", p.kind, pluginID)
	}
	if e.client != stale {
		return e.client, nil
	}
	if e.lingerTimer != nil {
		e.lingerTimer.Stop()
		e.lingerTimer = nil
	}
	c, err := p.relaunchLocked(ctx, pluginID, binPath, e.client)
	if err != nil {
		var zero C
		return zero, err
	}
	e.client = c
	e.generation++
	return c, nil
}

// evict force-relaunches pluginID's cached subprocess so it picks up
// out-of-band authority changes only read at launch (e.g. a Grant edit). A
// no-op, returning nil, when nothing is cached for pluginID. Unlike restart,
// there is no staleClient to compare against: whatever is currently cached
// gets relaunched unconditionally.
//
// A pending idle-linger timer on the entry is always disarmed first. If the
// entry has zero references (it was idle-lingering, or this pool never
// lingers and nobody happens to hold a reference right now), there is no
// live caller whose next call would run against stale authority, so evict
// just tears it down immediately — same end state as if the linger timer
// had fired right now — rather than relaunching a process nobody is using:
// the next Acquire is then a clean cache miss that launches fresh under
// current authority anyway, so this never leaves an unreferenced,
// never-reclaimed replacement process behind. When the entry IS referenced,
// evict relaunches it in place; existing reference holders keep their ref
// count against the fresh entry, exactly like a self-heal relaunch in
// acquire.
//
// INVARIANT (same one scheduleLinger's doc comment establishes): the
// refs<=0 tear-down below removes the map entry inside the critical section
// but runs the actual Shutdown call AFTER p.mu is released, never while
// holding it. This is a background-style reclaim — nothing is waiting on
// this call the way a caller waits on its own release — so blocking every
// other pluginID's Acquire/release on this pool for go-plugin's Kill()
// grace period would be paying a cost that has nothing to do with them.
func (p *pool[C]) evict(ctx context.Context, pluginID, binPath string) error {
	p.mu.Lock()
	e, ok := p.procs[pluginID]
	if !ok {
		p.mu.Unlock()
		return nil
	}
	if e.lingerTimer != nil {
		e.lingerTimer.Stop()
		e.lingerTimer = nil
	}
	if e.refs <= 0 {
		delete(p.procs, pluginID)
		client := e.client
		p.mu.Unlock()
		_ = client.Shutdown(context.Background())
		return nil
	}
	c, err := p.relaunchLocked(ctx, pluginID, binPath, e.client)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	e.client = c
	e.generation++
	p.mu.Unlock()
	return nil
}

// relaunchLocked tears down old (best-effort: it's presumed already dead, but
// Shutdown/Kill is safe to call again and frees lingering resources) and
// starts a replacement subprocess for pluginID. Caller must hold p.mu.
func (p *pool[C]) relaunchLocked(ctx context.Context, pluginID, binPath string, old C) (C, error) {
	_ = old.Shutdown(context.Background())
	c, err := p.launch(ctx, pluginID, binPath)
	if err != nil {
		var zero C
		return zero, fmt.Errorf("pluginproc: relaunch %s %q: %w", p.kind, pluginID, err)
	}
	return c, nil
}

// release drops one reference to pluginID's entry. When refs reaches zero
// and the pool has no linger func, the subprocess is shut down synchronously
// (the caller-initiated release pays its own cost) and the entry removed —
// every pool's original behavior except assist/archiveTool. When the pool
// does have a linger func, an idle-shutdown timer is armed instead (see
// scheduleLinger), so a caller that re-acquires within the linger window
// reuses the still-warm process.
func (p *pool[C]) release(pluginID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			e, ok := p.procs[pluginID]
			if !ok {
				p.mu.Unlock()
				return
			}
			e.refs--
			if e.refs > 0 {
				p.mu.Unlock()
				return
			}
			if p.linger == nil {
				// Every non-lingering pool's original, pre-generic behavior:
				// THIS caller (the one whose release just dropped refs to
				// zero) pays the Shutdown cost itself, synchronously, inside
				// the lock — not a background reclaim blocking unrelated
				// callers, so left exactly as it always was.
				_ = e.client.Shutdown(context.Background())
				delete(p.procs, pluginID)
				p.mu.Unlock()
				return
			}
			if linger := p.linger(); linger > 0 {
				p.scheduleLinger(pluginID, e, linger)
				p.mu.Unlock()
				return
			}
			// linger() <= 0: not reachable today (assistIdleLinger and
			// archiveToolIdleLinger are always positive, in production and
			// in every test that shrinks them), but if it ever were, this is
			// a background-style reclaim exactly like scheduleLinger's timer
			// callback — no caller is left waiting on this particular
			// Shutdown the way the branch above has one — so it gets the
			// same lock-released-before-Shutdown treatment as evict's
			// refs<=0 branch and scheduleLinger, not the synchronous
			// "caller pays their own cost" treatment two branches up.
			delete(p.procs, pluginID)
			client := e.client
			p.mu.Unlock()
			_ = client.Shutdown(context.Background())
		})
	}
}

// scheduleLinger arms the idle-shutdown timer for an entry that just dropped
// to zero references. Caller must hold p.mu.
//
// The timer's callback re-acquires p.mu and re-validates, before touching
// anything, that: the pluginID is still mapped to this exact entry (not
// deleted and replaced), refs are still zero (nothing re-acquired it in the
// meantime — that path also stops this timer directly, but the refs check is
// the correctness backstop if the callback is already running when Stop() is
// called), and the entry's generation is still the one this timer was armed
// for (no self-heal relaunch, restart, or evict swapped in a different
// subprocess this timer was never meant to kill). Only when all three hold
// does it remove the entry and shut the client down.
//
// INVARIANT: Shutdown runs AFTER p.mu is released, never while holding it.
// go-plugin's Kill() (Shutdown's implementation) waits up to a fixed grace
// period for the subprocess to exit cleanly before SIGKILLing it, so calling
// it while holding p.mu would stall every other Acquire/release on this pool
// for that same window, for a purely background reclaim that has nothing to
// do with them. The map delete still happens inside the critical section (so
// a concurrent acquire can never observe a half-removed entry); only the
// actual process-kill is moved past the unlock.
func (p *pool[C]) scheduleLinger(pluginID string, e *poolEntry[C], linger time.Duration) {
	gen := e.generation
	e.lingerTimer = time.AfterFunc(linger, func() {
		p.mu.Lock()
		cur, ok := p.procs[pluginID]
		if !ok || cur != e || cur.refs > 0 || cur.generation != gen {
			p.mu.Unlock()
			return
		}
		delete(p.procs, pluginID)
		client := cur.client
		p.mu.Unlock()
		_ = client.Shutdown(context.Background())
	})
}

// count reports the number of currently live subprocesses in this pool.
func (p *pool[C]) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.procs)
}
