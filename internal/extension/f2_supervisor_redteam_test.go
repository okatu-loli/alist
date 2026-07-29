package extension

import (
	"context"
	"net"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// F2 supervisor / lifecycle red team: BackgroundService (ServiceSupervisor),
// AccessProtocol (ProtocolManager) and FeatureBundle (ExpandFeatureBundle). The
// brief's questions: can a bg service exhaust the host via panic/restart, can a
// ctx-ignoring Run block shutdown, is a disabled protocol still served, and can
// a bundle expand onto a wiring point it doesn't actually implement.

// stubbornService's Run IGNORES ctx entirely: it blocks on a channel the test
// controls, not on ctx.Done(). It models a buggy/malicious BackgroundService
// that does not honor cancellation.
type stubbornService struct {
	release chan struct{}
	started chan struct{}
}

func (s *stubbornService) Manifest() ext.Manifest               { return ext.Manifest{} }
func (s *stubbornService) Init(context.Context, ext.Host) error { return nil }
func (s *stubbornService) Shutdown(context.Context) error       { return nil }
func (s *stubbornService) Run(_ context.Context) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release // never returns until the test releases it — ctx is ignored
	return nil
}
func (s *stubbornService) Status(context.Context) (ext.ServiceStatus, error) {
	return ext.ServiceStatus{State: ext.ServiceStateRunning}, nil
}

// FINDING D (LOW) — a BackgroundService whose Run ignores ctx hangs the
// supervisor's Wait() indefinitely (shutdown DoS).
//
// ServiceSupervisor.supervise calls runServiceOnce → svc.Run(ctx) directly and
// only returns from the goroutine once Run returns. Cancellation is delivered
// solely by passing ctx into Run; there is no independent deadline on Run
// returning after ctx is done. So a service that never observes ctx.Done()
// (`<-s.release` here, or a bare `select{}`) keeps its supervise goroutine alive
// forever, and ServiceSupervisor.Wait() — the graceful-shutdown join point —
// blocks with it. One misbehaving resident service can wedge process shutdown.
//
// Mitigations that lower the severity: BackgroundService is a native/trusted
// tier point, and boot starts the supervisor fire-and-forget (Wait is not on the
// current shutdown path). But if/when graceful shutdown joins the supervisor,
// this becomes a live hang. Suggested fix: bound the post-cancel join (e.g. Wait
// with a timeout, then detach) so one service cannot hold shutdown hostage.
func TestReg_BackgroundServiceRunIgnoringCtxHangsWait_FindingD(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bgEntry("svc.stubborn")); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	svc := &stubbornService{release: release, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance("svc.stubborn", svc)
	t.Cleanup(func() { RegisterBackgroundServiceInstance("svc.stubborn", nil) })

	sup := NewServiceSupervisor(r)
	sup.JoinGrace = 100 * time.Millisecond // FIXED: bound the post-cancel join
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)

	select {
	case <-svc.started:
	case <-time.After(2 * time.Second):
		t.Fatal("service Run did not start")
	}

	// Ask everything to stop, then join. FIXED (bounded join): even though Run
	// ignores ctx, Wait must return within the grace + margin instead of hanging.
	cancel()
	done := make(chan struct{})
	go func() { sup.Wait(); close(done) }()

	select {
	case <-done:
		// Wait detached the unresponsive service and returned — shutdown proceeds.
	case <-time.After(2 * time.Second):
		t.Fatal("REGRESSION: Wait hung on a ctx-ignoring Run instead of detaching after the grace")
	}

	// Release so the abandoned goroutine exits and the test cleans up (no leak).
	close(release)
}

// VERIFIED CORRECT (regression) — ProtocolManager does NOT bind a listener for a
// DISABLED TransportConn protocol. Start iterates Registry.ByKind, which filters
// on enabled, so a disabled FTP/SFTP plugin gets no inbound socket.
func TestReg_ProtocolManagerSkipsDisabledConnProtocol(t *testing.T) {
	r := NewRegistry()
	e := accessProtoEntry("proto.offconn")
	e.enabled = false
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterAccessProtocolInstance("proto.offconn", stubConnProtocol{got: make(chan net.Conn, 1)})
	t.Cleanup(func() { RegisterAccessProtocolInstance("proto.offconn", nil) })

	m := NewProtocolManager(r)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })

	if addr := m.Addr("proto.offconn"); addr != nil {
		t.Fatalf("disabled protocol must not get a listener, but one bound at %s", addr)
	}
}

// bareBundle implements ONLY FeatureBundle — none of APIRoute / EventHook /
// ScheduledJob. It models a bundle manifest that would like to be treated as an
// APIRoute etc. without actually satisfying the interface.
type bareBundle struct{}

func (bareBundle) Manifest() ext.Manifest               { return ext.Manifest{} }
func (bareBundle) Init(context.Context, ext.Host) error { return nil }
func (bareBundle) Shutdown(context.Context) error       { return nil }
func (bareBundle) Bundle() ext.BundleSpec               { return ext.BundleSpec{} }

// VERIFIED CORRECT (regression) — a bundle cannot "claim" an extension point it
// does not implement. ExpandFeatureBundle discovers contributed points by Go
// type assertion on the bundle value, so a bundle that satisfies none of them
// expands to ZERO extra kinds and opens no dispatch门. There is no manifest field
// that could assert an interface the value does not actually implement.
func TestReg_ExpandBundleWithNoContributedInterfacesAddsNoKinds(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bundleEntry("bundle.bare")); err != nil {
		t.Fatal(err)
	}
	RegisterFeatureBundleInstance("bundle.bare", bareBundle{})
	t.Cleanup(func() { CollapseFeatureBundle("bundle.bare") })

	exp, err := ExpandFeatureBundle(r, "bundle.bare")
	if err != nil {
		t.Fatalf("ExpandFeatureBundle: %v", err)
	}
	if len(exp.Kinds) != 0 {
		t.Fatalf("bundle implementing no sub-interface must add no kinds, got %v", exp.Kinds)
	}
	// And it must not be reachable as an APIRoute (no phantom route门).
	e, _ := r.Get("bundle.bare")
	if e.HasKind(ext.KindAPIRoute) || e.HasKind(ext.KindEventHook) || e.HasKind(ext.KindScheduledJob) {
		t.Fatal("bare bundle must not gain any contributed Kind")
	}
	if _, err := DispatchAPIRoute(context.Background(), r, "bundle.bare", "/anything", false,
		&ext.HTTPRequest{Method: "GET", URL: "/anything"}); err == nil {
		t.Fatal("bare bundle must not be dispatchable as an APIRoute")
	}
}
