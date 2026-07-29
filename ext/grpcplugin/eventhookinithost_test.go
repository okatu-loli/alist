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

// fakeEventHook is the plugin-side ext.EventHook used to observe whether
// InitHost delivered a working host to Init, and to exercise OnEvent over the
// wire. Mirrors fakeAction (actioninithost_test.go) one Kind over.
type fakeEventHook struct {
	initCh chan ext.Host

	gotEvent ext.Event
	err      error
}

func (p *fakeEventHook) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-eventhook"} }
func (p *fakeEventHook) Shutdown(ctx context.Context) error { return nil }

func (p *fakeEventHook) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeEventHook) OnEvent(ctx context.Context, e ext.Event) error {
	p.gotEvent = e
	return p.err
}

var _ ext.EventHook = (*fakeEventHook)(nil)

// failingInitEventHook is an EventHook whose Init always fails, used to prove
// that a business-error ErrorReply from eventHookServer.InitHost (as opposed
// to a gRPC transport error) still makes GRPCClient — and therefore Dispense —
// fail loudly. Mirrors failingInitAction.
type failingInitEventHook struct{}

func (p *failingInitEventHook) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-eventhook"}
}
func (p *failingInitEventHook) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitEventHook) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitEventHook) OnEvent(ctx context.Context, e ext.Event) error { return nil }

var _ ext.EventHook = (*failingInitEventHook)(nil)

// TestEventHookInitHost_WithHostServerFactory mirrors
// TestActionInitHost_WithHostServerFactory one Kind over: it proves the full
// handshake end to end using go-plugin's in-process test harness.
func TestEventHookInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeEventHook{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.EventHookPluginName: &EventHookPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.EventHookPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.EventHookServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.EventHookServiceClient", raw)
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

// TestEventHookInitHost_NilFactory_InitNotCalled mirrors
// TestActionInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (EventHookPluginMap, as before this feature existed), GRPCClient never calls
// InitHost and the hook's Init is never invoked.
func TestEventHookInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeEventHook{initCh: make(chan ext.Host, 1)}
	plugins := EventHookPluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.EventHookPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.EventHookServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.EventHookServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestEventHookInitHost_InitFails_DispenseErrors mirrors
// TestActionInitHost_InitFails_DispenseErrors: a hook Init failure must fail
// the Dispense, not silently hand back a live client.
func TestEventHookInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitEventHook{}
	plugins := map[string]plugin.Plugin{
		pb.EventHookPluginName: &EventHookPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.EventHookPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the hook's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the hook's Init error (%q)", err.Error(), "boom")
	}
}

// TestEventHookOnEventRoundTrip proves OnEvent crosses the wire intact: every
// ext.Event field (type/unix_milli/path/dst_path/actor) the host sent
// survives the gRPC round trip unmangled.
func TestEventHookOnEventRoundTrip(t *testing.T) {
	p := &fakeEventHook{initCh: make(chan ext.Host, 1)}
	plugins := EventHookPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.EventHookPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.EventHookServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.EventHookServiceClient", raw)
	}

	ctx := context.Background()
	reply, err := svc.OnEvent(ctx, &pb.EventProto{
		Type:      string(ext.EventFSMove),
		UnixMilli: 1234567,
		Path:      "/a/src.txt",
		DstPath:   "/a/dst",
		Actor:     "alice",
	})
	if err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		t.Fatalf("OnEvent returned error: %v", e)
	}

	want := ext.Event{
		Type:      ext.EventFSMove,
		UnixMilli: 1234567,
		Path:      "/a/src.txt",
		DstPath:   "/a/dst",
		Actor:     "alice",
	}
	if p.gotEvent != want {
		t.Fatalf("plugin saw event = %+v, want %+v", p.gotEvent, want)
	}
}

// TestEventHookOnEventErrorRoundTrip proves an OnEvent error surfaces as an
// ErrorReply rather than a transport failure.
func TestEventHookOnEventErrorRoundTrip(t *testing.T) {
	p := &fakeEventHook{
		initCh: make(chan ext.Host, 1),
		err:    ext.NewError(ext.ErrCodeObjectNotFound, "no such target"),
	}
	plugins := EventHookPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.EventHookPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc := raw.(pb.EventHookServiceClient)

	reply, err := svc.OnEvent(context.Background(), &pb.EventProto{Type: string(ext.EventFSRemove)})
	if err != nil {
		t.Fatalf("OnEvent transport error: %v", err)
	}
	got := pb.ErrorFromProto(reply.Error)
	if got == nil {
		t.Fatal("want a business error, got nil")
	}
	code, ok := ext.CodeOf(got)
	if !ok || code != ext.ErrCodeObjectNotFound {
		t.Fatalf("error code = %v (ok=%v), want ErrCodeObjectNotFound", code, ok)
	}
}
