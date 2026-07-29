package grpcplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// fakeScheduledJob is the plugin-side ext.ScheduledJob used to observe
// whether InitHost delivered a working host to Init, and to exercise
// Schedule/Run over the wire. Mirrors fakeEventHook one Kind over.
type fakeScheduledJob struct {
	initCh chan ext.Host

	spec   string
	runs   int
	runErr error
}

func (p *fakeScheduledJob) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-scheduledjob"} }
func (p *fakeScheduledJob) Shutdown(ctx context.Context) error { return nil }

func (p *fakeScheduledJob) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeScheduledJob) Schedule() string { return p.spec }

func (p *fakeScheduledJob) Run(ctx context.Context) error {
	p.runs++
	return p.runErr
}

var _ ext.ScheduledJob = (*fakeScheduledJob)(nil)

// failingInitScheduledJob is a ScheduledJob whose Init always fails, used to
// prove that a business-error ErrorReply from scheduledJobServer.InitHost (as
// opposed to a gRPC transport error) still makes GRPCClient — and therefore
// Dispense — fail loudly. Mirrors failingInitEventHook.
type failingInitScheduledJob struct{}

func (p *failingInitScheduledJob) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-scheduledjob"}
}
func (p *failingInitScheduledJob) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitScheduledJob) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitScheduledJob) Schedule() string              { return "@every 1h" }
func (p *failingInitScheduledJob) Run(ctx context.Context) error { return nil }

var _ ext.ScheduledJob = (*failingInitScheduledJob)(nil)

// TestScheduledJobInitHost_WithHostServerFactory mirrors
// TestEventHookInitHost_WithHostServerFactory one Kind over: it proves the
// full handshake end to end using go-plugin's in-process test harness.
func TestScheduledJobInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeScheduledJob{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.ScheduledJobPluginName: &ScheduledJobPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ScheduledJobPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.ScheduledJobServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.ScheduledJobServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		if h == nil {
			t.Fatal("Init called with nil host")
		}
		cfg, err := h.LoadConfig(context.Background())
		if err != nil {
			t.Fatalf("host.LoadConfig: %v", err)
		}
		if string(cfg) != `{"hello":"host"}` {
			t.Fatalf("host.LoadConfig = %s, want the fakeHostServer payload", cfg)
		}
	default:
		t.Fatal("Init was not called synchronously by Dispense/InitHost")
	}
}

// TestScheduledJobInitHost_NilFactory_InitNotCalled mirrors
// TestEventHookInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (ScheduledJobPluginMap, as before this feature existed), GRPCClient never
// calls InitHost and the job's Init is never invoked.
func TestScheduledJobInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeScheduledJob{initCh: make(chan ext.Host, 1)}
	plugins := ScheduledJobPluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ScheduledJobPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.ScheduledJobServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.ScheduledJobServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestScheduledJobInitHost_InitFails_DispenseErrors mirrors
// TestEventHookInitHost_InitFails_DispenseErrors: a job Init failure must
// fail the Dispense, not silently hand back a live client.
func TestScheduledJobInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitScheduledJob{}
	plugins := map[string]plugin.Plugin{
		pb.ScheduledJobPluginName: &ScheduledJobPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ScheduledJobPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the job's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the job's Init error (%q)", err.Error(), "boom")
	}
}

// TestScheduledJobScheduleAndRunRoundTrip proves Schedule() and Run() cross
// the wire intact: the spec Schedule() returns and the fact that Run() was
// invoked (and its error, when any) survive the gRPC round trip. Mirrors
// TestAPIRouteRoutesAndHandleRoundTrip's Schedule/Run pairing.
func TestScheduledJobScheduleAndRunRoundTrip(t *testing.T) {
	p := &fakeScheduledJob{initCh: make(chan ext.Host, 1), spec: "@every 30s"}
	plugins := ScheduledJobPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ScheduledJobPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.ScheduledJobServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.ScheduledJobServiceClient", raw)
	}

	ctx := context.Background()

	sr, err := svc.Schedule(ctx, &pb.Empty{})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if sr.Spec != "@every 30s" {
		t.Fatalf("Schedule spec = %q, want @every 30s", sr.Spec)
	}

	rr, err := svc.Run(ctx, &pb.Empty{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if e := pb.ErrorFromProto(rr.Error); e != nil {
		t.Fatalf("Run returned error: %v", e)
	}
	if p.runs != 1 {
		t.Fatalf("plugin Run count = %d, want 1", p.runs)
	}
}

// TestScheduledJobRunErrorRoundTrip proves a Run error surfaces as an
// ErrorReply rather than a transport failure.
func TestScheduledJobRunErrorRoundTrip(t *testing.T) {
	p := &fakeScheduledJob{
		initCh: make(chan ext.Host, 1),
		spec:   "@every 1h",
		runErr: ext.NewError(ext.ErrCodeObjectNotFound, "no such target"),
	}
	plugins := ScheduledJobPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ScheduledJobPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc := raw.(pb.ScheduledJobServiceClient)

	rr, err := svc.Run(context.Background(), &pb.Empty{})
	if err != nil {
		t.Fatalf("Run transport error: %v", err)
	}
	got := pb.ErrorFromProto(rr.Error)
	if got == nil {
		t.Fatal("want a business error, got nil")
	}
	code, ok := ext.CodeOf(got)
	if !ok || code != ext.ErrCodeObjectNotFound {
		t.Fatalf("error code = %v (ok=%v), want ErrCodeObjectNotFound", code, ok)
	}
}
