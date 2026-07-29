package extension

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// stubService is a BackgroundService whose Run blocks until ctx is cancelled and
// reports a state that flips running↔stopped around Run.
type stubService struct {
	mu      sync.Mutex
	state   string
	runs    int32
	started chan struct{}
}

func (s *stubService) Manifest() ext.Manifest               { return ext.Manifest{} }
func (s *stubService) Init(context.Context, ext.Host) error { return nil }
func (s *stubService) Shutdown(context.Context) error       { return nil }
func (s *stubService) Run(ctx context.Context) error {
	atomic.AddInt32(&s.runs, 1)
	s.set(ext.ServiceStateRunning)
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	s.set(ext.ServiceStateStopped)
	return nil
}
func (s *stubService) Status(context.Context) (ext.ServiceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ext.ServiceStatus{State: s.state, Metrics: map[string]any{"runs": atomic.LoadInt32(&s.runs)}}, nil
}
func (s *stubService) set(st string) { s.mu.Lock(); s.state = st; s.mu.Unlock() }

func bgEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindBackgroundService}},
		Kind:     ext.KindBackgroundService,
		enabled:  true,
	}
}

// TestResolveBackgroundServiceUsesFactory pins the gRPC-dispatch seam: an
// ExecGRPC entry with a binPath and no in-process instance must resolve through
// the installed backgroundServiceFactory (the plugins.go init wires this to
// pluginproc.Default.AcquireBackgroundService). Without a factory the same
// entry resolves to nil (no host-side handler), matching supervisor skip
// semantics.
func TestResolveBackgroundServiceUsesFactory(t *testing.T) {
	saved := backgroundServiceFactory
	t.Cleanup(func() { backgroundServiceFactory = saved })

	e := &Entry{
		Manifest: ext.Manifest{ID: "svc.grpc", Kinds: []ext.Kind{ext.KindBackgroundService}},
		Kind:     ext.KindBackgroundService,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-bg",
	}

	// No factory wired → no host-side handler → nil.
	backgroundServiceFactory = nil
	if svc, _ := resolveBackgroundService(e); svc != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", svc)
	}

	// Factory wired → the ExecGRPC branch is hit and its service is returned.
	stub := &stubService{state: ext.ServiceStateStopped}
	var released bool
	backgroundServiceFactory = func(got *Entry) (ext.BackgroundService, func(), error) {
		if got.ID() != "svc.grpc" || got.BinPath() != "/bin/example-bg" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	svc, release := resolveBackgroundService(e)
	if svc != stub {
		t.Fatalf("resolveBackgroundService must return the factory's service, got %T", svc)
	}
	release()
	if !released {
		t.Fatal("resolveBackgroundService must return the factory's release func")
	}
}

func TestServiceSupervisorStartStopAndStatus(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bgEntry("svc.frp")); err != nil {
		t.Fatal(err)
	}
	svc := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance("svc.frp", svc)
	t.Cleanup(func() { RegisterBackgroundServiceInstance("svc.frp", nil) })

	sup := NewServiceSupervisor(r)
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)

	// Run has begun → status reports running.
	select {
	case <-svc.started:
	case <-time.After(2 * time.Second):
		t.Fatal("service Run did not start")
	}
	st, err := ServiceStatusOf(context.Background(), r, "svc.frp")
	if err != nil {
		t.Fatalf("ServiceStatusOf: %v", err)
	}
	if st.State != ext.ServiceStateRunning {
		t.Fatalf("want running, got %q", st.State)
	}

	// Cancel → Run returns → supervisor drains.
	cancel()
	sup.Wait()
	st, _ = ServiceStatusOf(context.Background(), r, "svc.frp")
	if st.State != ext.ServiceStateStopped {
		t.Fatalf("after stop want stopped, got %q", st.State)
	}
}

// TestServiceSupervisorMultiKindManifestUsesManifestTruthSource mirrors
// TestInvokeActionMultiKindManifestUsesManifestTruthSource: an entry
// registered under a different primary Kind (Kind=StorageDriver) whose
// Manifest additionally declares BackgroundService (Kinds: [StorageDriver,
// BackgroundService]) never gets an extraKinds(BackgroundService) entry —
// BackgroundService has no FeatureBundle expansion path — so e.HasKind alone
// would make StartService/ServiceStatusOf silently skip it despite the
// plugin having declared the service. DeclaresKind must let both through.
func TestServiceSupervisorMultiKindManifestUsesManifestTruthSource(t *testing.T) {
	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{ID: "svc.multikind", Kinds: []ext.Kind{ext.KindStorageDriver, ext.KindBackgroundService}},
		Kind:     ext.KindStorageDriver, // registered/indexed under its primary Kind
		enabled:  true,
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	svc := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance("svc.multikind", svc)
	t.Cleanup(func() { RegisterBackgroundServiceInstance("svc.multikind", nil) })

	sup := NewServiceSupervisor(r)
	if err := sup.StartService("svc.multikind"); err != nil {
		t.Fatalf("StartService on multi-Kind manifest: %v", err)
	}
	select {
	case <-svc.started:
	case <-time.After(2 * time.Second):
		t.Fatal("StartService must have started the multi-Kind manifest's Run")
	}

	st, err := ServiceStatusOf(context.Background(), r, "svc.multikind")
	if err != nil {
		t.Fatalf("ServiceStatusOf on multi-Kind manifest: %v", err)
	}
	if st.State != ext.ServiceStateRunning {
		t.Fatalf("want running, got %q", st.State)
	}

	sup.StopService("svc.multikind")
	sup.Wait()
}

func TestServiceStatusOfUnknownOrDisabled(t *testing.T) {
	r := NewRegistry()
	if _, err := ServiceStatusOf(context.Background(), r, "nope"); err == nil {
		t.Fatal("unknown service: want error, got nil")
	}
	e := bgEntry("svc.off")
	e.enabled = false
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterBackgroundServiceInstance("svc.off", &stubService{started: make(chan struct{}, 1)})
	t.Cleanup(func() { RegisterBackgroundServiceInstance("svc.off", nil) })
	if _, err := ServiceStatusOf(context.Background(), r, "svc.off"); err == nil {
		t.Fatal("disabled service: want error, got nil")
	}
}

// A service that panics on its first Run is restarted (panic isolation +
// restart), and a co-scheduled healthy service keeps running throughout.
func TestServiceSupervisorPanicIsolationAndRestart(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bgEntry("svc.panic")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(bgEntry("svc.healthy")); err != nil {
		t.Fatal(err)
	}
	var panicOnce sync.Once
	restarted := make(chan struct{}, 1)
	panicker := &restartService{panicOnce: &panicOnce, restarted: restarted}
	healthy := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance("svc.panic", panicker)
	RegisterBackgroundServiceInstance("svc.healthy", healthy)
	t.Cleanup(func() {
		RegisterBackgroundServiceInstance("svc.panic", nil)
		RegisterBackgroundServiceInstance("svc.healthy", nil)
	})

	sup := NewServiceSupervisor(r)
	sup.RestartBackoff = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)
	t.Cleanup(func() { cancel(); sup.Wait() })

	// The healthy service is up and stays up.
	select {
	case <-healthy.started:
	case <-time.After(2 * time.Second):
		t.Fatal("healthy service did not start")
	}
	// The panicking service is restarted after its first (panicking) Run.
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("panicking service was not restarted")
	}
	st, err := ServiceStatusOf(context.Background(), r, "svc.healthy")
	if err != nil || st.State != ext.ServiceStateRunning {
		t.Fatalf("healthy service must stay running through the panic: st=%+v err=%v", st, err)
	}
}

// restartService panics on its first Run, then blocks (signalling the restart).
type restartService struct {
	panicOnce *sync.Once
	restarted chan struct{}
}

func (restartService) Manifest() ext.Manifest               { return ext.Manifest{} }
func (restartService) Init(context.Context, ext.Host) error { return nil }
func (restartService) Shutdown(context.Context) error       { return nil }
func (s restartService) Run(ctx context.Context) error {
	first := false
	s.panicOnce.Do(func() { first = true })
	if first {
		panic("service boom")
	}
	select {
	case s.restarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil
}
func (restartService) Status(context.Context) (ext.ServiceStatus, error) {
	return ext.ServiceStatus{State: ext.ServiceStateRunning}, nil
}
