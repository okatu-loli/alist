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

// fakeAction is the plugin-side ext.Action used to observe whether InitHost
// delivered a working host to Init, and to exercise Invoke over the wire.
// Mirrors fakeAPIRoute (apirouteinithost_test.go) one Kind over.
type fakeAction struct {
	initCh chan ext.Host

	gotPrincipal ext.Principal
	gotActionID  string
	gotArgs      map[string]any
	result       any
	err          error
}

func (p *fakeAction) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-action"} }
func (p *fakeAction) Shutdown(ctx context.Context) error { return nil }

func (p *fakeAction) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeAction) Invoke(ctx context.Context, principal ext.Principal, actionID string, args map[string]any) (any, error) {
	p.gotPrincipal = principal
	p.gotActionID = actionID
	p.gotArgs = args
	return p.result, p.err
}

var _ ext.Action = (*fakeAction)(nil)

// failingInitAction is an Action whose Init always fails, used to prove that a
// business-error ErrorReply from actionServer.InitHost (as opposed to a gRPC
// transport error) still makes GRPCClient — and therefore Dispense — fail
// loudly. Mirrors failingInitAPIRoute.
type failingInitAction struct{}

func (p *failingInitAction) Manifest() ext.Manifest             { return ext.Manifest{ID: "failing-init-action"} }
func (p *failingInitAction) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitAction) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitAction) Invoke(ctx context.Context, principal ext.Principal, actionID string, args map[string]any) (any, error) {
	return nil, nil
}

var _ ext.Action = (*failingInitAction)(nil)

// TestActionInitHost_WithHostServerFactory mirrors
// TestAPIRouteInitHost_WithHostServerFactory one Kind over: it proves the full
// handshake end to end using go-plugin's in-process test harness.
func TestActionInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeAction{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.ActionPluginName: &ActionPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ActionPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.ActionServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.ActionServiceClient", raw)
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

// TestActionInitHost_NilFactory_InitNotCalled mirrors
// TestAPIRouteInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (ActionPluginMap, as before this feature existed), GRPCClient never calls
// InitHost and the action's Init is never invoked.
func TestActionInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeAction{initCh: make(chan ext.Host, 1)}
	plugins := ActionPluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ActionPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.ActionServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.ActionServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestActionInitHost_InitFails_DispenseErrors mirrors
// TestAPIRouteInitHost_InitFails_DispenseErrors: an action Init failure must
// fail the Dispense, not silently hand back a live client.
func TestActionInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitAction{}
	plugins := map[string]plugin.Plugin{
		pb.ActionPluginName: &ActionPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ActionPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the plugin's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the action's Init error (%q)", err.Error(), "boom")
	}
}

// TestActionInvokeRoundTrip proves Invoke() crosses the wire intact: the
// principal/actionID/args the host sent and the result the plugin returned
// both survive the gRPC round trip (JSON-encoded args_json/result_json)
// unmangled.
func TestActionInvokeRoundTrip(t *testing.T) {
	p := &fakeAction{
		initCh: make(chan ext.Host, 1),
		result: map[string]any{"ok": true, "count": float64(3)},
	}
	plugins := ActionPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ActionPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.ActionServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.ActionServiceClient", raw)
	}

	ctx := context.Background()
	argsJSON := []byte(`{"path":"/x","force":true}`)
	ir, err := svc.Invoke(ctx, &pb.InvokeRequest{
		Principal: "user-123",
		ActionId:  "share.create",
		ArgsJson:  argsJSON,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if e := pb.ErrorFromProto(ir.Error); e != nil {
		t.Fatalf("Invoke returned error: %v", e)
	}

	if p.gotPrincipal != "user-123" {
		t.Fatalf("plugin saw principal = %q, want user-123", p.gotPrincipal)
	}
	if p.gotActionID != "share.create" {
		t.Fatalf("plugin saw actionID = %q, want share.create", p.gotActionID)
	}
	if p.gotArgs["path"] != "/x" || p.gotArgs["force"] != true {
		t.Fatalf("plugin saw args = %+v, want path=/x force=true", p.gotArgs)
	}

	if string(ir.ResultJson) != `{"count":3,"ok":true}` {
		t.Fatalf("result_json = %s, want the marshaled result map", ir.ResultJson)
	}
}

// TestActionInvokeErrorRoundTrip proves an Invoke error surfaces as an
// ErrorReply rather than a transport failure or a lost result.
func TestActionInvokeErrorRoundTrip(t *testing.T) {
	p := &fakeAction{
		initCh: make(chan ext.Host, 1),
		err:    ext.NewError(ext.ErrCodeObjectNotFound, "no such share"),
	}
	plugins := ActionPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.ActionPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc := raw.(pb.ActionServiceClient)

	ir, err := svc.Invoke(context.Background(), &pb.InvokeRequest{ActionId: "share.get"})
	if err != nil {
		t.Fatalf("Invoke transport error: %v", err)
	}
	got := pb.ErrorFromProto(ir.Error)
	if got == nil {
		t.Fatal("want a business error, got nil")
	}
	code, ok := ext.CodeOf(got)
	if !ok || code != ext.ErrCodeObjectNotFound {
		t.Fatalf("error code = %v (ok=%v), want ErrCodeObjectNotFound", code, ok)
	}
}
