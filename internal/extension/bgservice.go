package extension

import (
	"context"
	"fmt"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// This file is the host consumption of Kind=BackgroundService (design §2.4): a
// long-running side task (frp tunnel, sync engine) started after boot and
// supervised for the process lifetime. It reuses the Scheduler's supervision
// shape — one goroutine per enabled service, panic isolation so one crash never
// touches the others — but for a CONTINUOUS Run rather than a ticked one, adding
// restart-with-backoff (a background service that exits/crashes is restarted;
// design §2.4 "the host supervises restart/backoff").
//
// Anything a service borrows inside Run stays Grant-gated + Authorize-enforced by
// the F0 host layer; this file only owns the goroutine lifecycle and the Status
// polling seam the frontend status card reads.

// backgroundServiceFactory is the gRPC-dispatch seam (mirrors eventHookFactory).
// nil until a gRPC bridge lands; in-process instances are the tested path.
var backgroundServiceFactory func(e *Entry) (ext.BackgroundService, func(), error)

// SetBackgroundServiceFactory installs the gRPC BackgroundService provider.
func SetBackgroundServiceFactory(fn func(e *Entry) (ext.BackgroundService, func(), error)) {
	backgroundServiceFactory = fn
}

var (
	backgroundServiceMu        sync.RWMutex
	backgroundServiceInstances = map[string]ext.BackgroundService{}
)

// RegisterBackgroundServiceInstance registers an in-process ext.BackgroundService
// under id (a built-in/native service or a test double). A nil value deregisters.
func RegisterBackgroundServiceInstance(id string, s ext.BackgroundService) {
	backgroundServiceMu.Lock()
	defer backgroundServiceMu.Unlock()
	if s == nil {
		delete(backgroundServiceInstances, id)
		return
	}
	backgroundServiceInstances[id] = s
}

func resolveBackgroundService(e *Entry) (ext.BackgroundService, func()) {
	backgroundServiceMu.RLock()
	if s, ok := backgroundServiceInstances[e.Manifest.ID]; ok {
		backgroundServiceMu.RUnlock()
		return s, func() {}
	}
	backgroundServiceMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && backgroundServiceFactory != nil {
		if s, release, err := backgroundServiceFactory(e); err == nil {
			return s, release
		}
	}
	return nil, func() {}
}

// RegisterBuiltinBackgroundService installs an in-process, trusted built-in
// BackgroundService (frp) into r: it inserts the enabled registry entry carrying
// the service's Manifest and registers the in-process instance so the supervisor
// resolves it at boot. Registration is the single call a built-in service needs;
// the entry is trusted by virtue of being host-registered, exactly like the
// built-in labels feature bundle. A nil svc or empty id is an error — a
// half-wired built-in must not register.
func RegisterBuiltinBackgroundService(r *Registry, id string, svc ext.BackgroundService) error {
	if id == "" {
		return fmt.Errorf("extension: built-in background service with empty id")
	}
	if svc == nil {
		return fmt.Errorf("extension: built-in background service %q is nil", id)
	}
	// Carry the service's own Manifest so its declared Contributions (e.g. the
	// StatusProvider card) flow into AggregateContributions; force the id/kind so
	// a half-declared built-in still registers correctly.
	man := svc.Manifest()
	man.ID = id
	if len(man.Kinds) == 0 {
		man.Kinds = []ext.Kind{ext.KindBackgroundService}
	}
	entry := &Entry{
		Manifest: man,
		Kind:     ext.KindBackgroundService,
		Mode:     InstanceModeSingleton,
		enabled:  true,
		builtin:  true,
	}
	if err := r.Register(entry); err != nil {
		return err
	}
	RegisterBackgroundServiceInstance(id, svc)
	return nil
}

const (
	defaultRestartBackoff = 500 * time.Millisecond
	defaultMaxBackoff     = 30 * time.Second
	// backoffResetAfter: a Run that stayed up at least this long is treated as a
	// healthy session, so its restart backoff resets to the base rather than
	// climbing toward the cap on an occasional restart.
	backoffResetAfter = 1 * time.Minute
)

// ServiceSupervisor runs the enabled Kind=BackgroundService plugins for the
// process lifetime. It owns one goroutine per service; each calls Run under a
// panic recover and, when Run returns or panics while the context is still live,
// restarts it after an exponential backoff (reset once a session has stayed up
// long enough). ctx cancellation stops every service (call Wait to join).
type ServiceSupervisor struct {
	r  *Registry
	wg sync.WaitGroup

	// RestartBackoff / MaxBackoff override the restart backoff base/cap (0 uses
	// the defaults). Tests set a small base to keep restart assertions fast.
	RestartBackoff time.Duration
	MaxBackoff     time.Duration
	// JoinGrace bounds Wait's post-cancel join (0 uses serviceJoinGrace). Tests
	// set a small value to keep the detach assertion fast.
	JoinGrace time.Duration

	// mu guards rootCtx and running. StartService/StopService and each
	// supervise goroutine's exit cleanup all take it, so runtime start/stop is
	// concurrency-safe (-race clean).
	mu sync.Mutex
	// rootCtx is the parent context recorded by Start; per-service contexts are
	// derived from it so ctx cancellation from Start (shutdown) or StopService
	// (runtime disable) both stop a service.
	rootCtx context.Context
	// running maps a service id to the handle whose cancel stops it. A present
	// entry means the service has a live supervise goroutine.
	running map[string]*serviceHandle
}

// serviceHandle is one running service session. Its pointer identity lets a
// supervise goroutine's exit cleanup delete itself from running ONLY if it is
// still the current session — a StopService+StartService cycle installs a new
// handle, and the old goroutine must not clobber it (the runtime-restart race
// this Wave guards).
type serviceHandle struct {
	cancel context.CancelFunc
}

// NewServiceSupervisor builds a supervisor over r.
func NewServiceSupervisor(r *Registry) *ServiceSupervisor { return &ServiceSupervisor{r: r} }

// Start records the root context and launches a supervising goroutine per
// enabled BackgroundService (via StartService). It returns immediately; ctx
// cancellation — or StopService — stops a service (call Wait to join on
// shutdown).
func (s *ServiceSupervisor) Start(ctx context.Context) {
	s.mu.Lock()
	s.rootCtx = ctx
	if s.running == nil {
		s.running = make(map[string]*serviceHandle)
	}
	s.mu.Unlock()
	for _, e := range s.r.ByKind(ext.KindBackgroundService) {
		_ = s.StartService(e.Manifest.ID)
	}
}

// StartService starts (or, if already running, leaves running — idempotent) the
// enabled BackgroundService id under a fresh context derived from the root ctx.
// It is the runtime-enable seam: EnableBuiltin calls it after flipping a
// built-in service back on. A service that is unknown, disabled, not a
// BackgroundService, or has no host-side handler wired is skipped (returns nil,
// matching Start's boot-time skip semantics).
func (s *ServiceSupervisor) StartService(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = make(map[string]*serviceHandle)
	}
	if _, ok := s.running[id]; ok {
		return nil // already running — idempotent, do not double-start
	}
	e, ok := s.r.Get(id)
	// BackgroundService has no FeatureBundle expansion path, so the Manifest's
	// declared Kinds is the sole truth source (DeclaresKind, registry.go) — a
	// plugin that declares BackgroundService alongside another primary Kind
	// must still be startable here.
	if !ok || e == nil || !e.Enabled() || !e.DeclaresKind(ext.KindBackgroundService) {
		return nil
	}
	svc, release := resolveBackgroundService(e)
	if svc == nil {
		return nil // no host-side implementation wired; skip like Start did
	}
	root := s.rootCtx
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithCancel(root)
	h := &serviceHandle{cancel: cancel}
	s.running[id] = h
	s.wg.Add(1)
	go s.supervise(ctx, id, svc, release, h)
	return nil
}

// StopService cancels the running BackgroundService id (its Run ctx is
// cancelled → a CommandContext-backed process is killed → the supervise
// goroutine exits) and drops it from running. A no-op if id is not running. It
// is the runtime-disable seam: DisableBuiltin calls it.
func (s *ServiceSupervisor) StopService(id string) {
	s.mu.Lock()
	h := s.running[id]
	if h != nil {
		delete(s.running, id)
	}
	s.mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

// serviceJoinGrace bounds how long Wait blocks on service goroutines after ctx
// is cancelled: a BackgroundService whose Run ignores ctx.Done() would otherwise
// hang shutdown forever (F2 red-team). Past the grace, unresponsive services are
// logged and detached (they die with the process).
const serviceJoinGrace = 5 * time.Second

// Wait blocks until every service goroutine has exited (after ctx is cancelled),
// or until serviceJoinGrace elapses — a misbehaving Run that ignores ctx must not
// hang shutdown. Leaked services are logged and detached.
func (s *ServiceSupervisor) Wait() {
	grace := s.JoinGrace
	if grace <= 0 {
		grace = serviceJoinGrace
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		log.Warnf("extension: BackgroundService supervisor join timed out after %s; detaching unresponsive services", grace)
	}
}

func (s *ServiceSupervisor) base() time.Duration {
	if s.RestartBackoff > 0 {
		return s.RestartBackoff
	}
	return defaultRestartBackoff
}

func (s *ServiceSupervisor) max() time.Duration {
	if s.MaxBackoff > 0 {
		return s.MaxBackoff
	}
	return defaultMaxBackoff
}

func (s *ServiceSupervisor) supervise(ctx context.Context, id string, svc ext.BackgroundService, release func(), h *serviceHandle) {
	defer s.wg.Done()
	defer release()
	// On exit, remove ourselves from running — but ONLY if we are still the
	// current session. A StopService already deleted us (h != running[id]); a
	// StopService+StartService cycle installed a new handle we must not clobber.
	// Pointer identity of h makes this race-safe under mu.
	defer func() {
		s.mu.Lock()
		if s.running[id] == h {
			delete(s.running, id)
		}
		s.mu.Unlock()
	}()
	backoff := s.base()
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		runServiceOnce(ctx, id, svc)
		if ctx.Err() != nil {
			return
		}
		// Run returned/panicked while the context is still live: restart after a
		// backoff. A healthy session (stayed up past the reset threshold) resets
		// the backoff to base; a crash loop climbs toward the cap.
		if time.Since(started) >= backoffResetAfter {
			backoff = s.base()
		}
		log.Warnf("extension: BackgroundService %q exited; restarting in %s", id, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if next := backoff * 2; next <= s.max() {
			backoff = next
		} else {
			backoff = s.max()
		}
	}
}

// runServiceOnce invokes Run with panic isolation and returns when Run returns
// (or panics). It never restarts — the supervise loop owns restart policy.
func runServiceOnce(ctx context.Context, id string, svc ext.BackgroundService) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("extension: BackgroundService %q panicked: %v", id, r)
		}
	}()
	if err := svc.Run(ctx); err != nil {
		log.Warnf("extension: BackgroundService %q run error: %v", id, err)
	}
}

// ServiceStatusOf resolves an enabled Kind=BackgroundService plugin and returns
// its reported Status for the frontend status card (design §2.4, §4
// StatusProvider). It is the seam the status-poll endpoint calls. An unknown,
// disabled, or unwired service is an error rather than a silent empty status.
func ServiceStatusOf(ctx context.Context, r *Registry, id string) (ext.ServiceStatus, error) {
	e, ok := r.Get(id)
	// Same truth source as StartService above: DeclaresKind, not HasKind.
	if !ok || !e.Enabled() || !e.DeclaresKind(ext.KindBackgroundService) {
		return ext.ServiceStatus{}, fmt.Errorf("no background service named: %s", id)
	}
	svc, release := resolveBackgroundService(e)
	if svc == nil {
		return ext.ServiceStatus{}, ext.NewError(ext.ErrCodeNotImplement,
			fmt.Sprintf("BackgroundService %q has no host-side handler wired", id))
	}
	defer release()
	return svc.Status(ctx)
}

// DefaultServiceSupervisor is the process supervisor; boot starts it over
// DefaultRegistry after LoadPlugins (bootstrap.StartBackgroundServices).
var DefaultServiceSupervisor = NewServiceSupervisor(DefaultRegistry)
