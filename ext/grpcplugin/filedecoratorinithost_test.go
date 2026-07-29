package grpcplugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// fakeFileDecorator is the plugin-side ext.FileDecorator used to observe
// whether InitHost delivered a working host to Init, and to exercise Decorate
// over the wire. Mirrors fakeEventHook (eventhookinithost_test.go) one Kind
// over.
type fakeFileDecorator struct {
	initCh chan ext.Host
	groups []string
	values map[string]any
	err    error

	gotGroup string
	gotRefs  []ext.ObjRef
}

func (p *fakeFileDecorator) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-filedecorator"} }
func (p *fakeFileDecorator) Shutdown(ctx context.Context) error { return nil }

func (p *fakeFileDecorator) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeFileDecorator) Groups() []string { return p.groups }

func (p *fakeFileDecorator) Decorate(ctx context.Context, group string, refs []ext.ObjRef) (map[string]any, error) {
	p.gotGroup = group
	p.gotRefs = refs
	return p.values, p.err
}

var _ ext.FileDecorator = (*fakeFileDecorator)(nil)

// failingInitFileDecorator is a FileDecorator whose Init always fails, used to
// prove that a business-error ErrorReply from fileDecoratorServer.InitHost (as
// opposed to a gRPC transport error) still makes GRPCClient — and therefore
// Dispense — fail loudly. Mirrors failingInitEventHook.
type failingInitFileDecorator struct{}

func (p *failingInitFileDecorator) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-filedecorator"}
}
func (p *failingInitFileDecorator) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitFileDecorator) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitFileDecorator) Groups() []string { return nil }
func (p *failingInitFileDecorator) Decorate(ctx context.Context, group string, refs []ext.ObjRef) (map[string]any, error) {
	return nil, nil
}

var _ ext.FileDecorator = (*failingInitFileDecorator)(nil)

// TestFileDecoratorInitHost_WithHostServerFactory mirrors
// TestEventHookInitHost_WithHostServerFactory one Kind over: it proves the
// full handshake end to end using go-plugin's in-process test harness.
func TestFileDecoratorInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeFileDecorator{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.FileDecoratorPluginName: &FileDecoratorPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.FileDecoratorPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.FileDecoratorServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.FileDecoratorServiceClient", raw)
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

// TestFileDecoratorInitHost_NilFactory_InitNotCalled mirrors
// TestEventHookInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (FileDecoratorPluginMap, as before this feature existed), GRPCClient never
// calls InitHost and the decorator's Init is never invoked.
func TestFileDecoratorInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeFileDecorator{initCh: make(chan ext.Host, 1)}
	plugins := FileDecoratorPluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.FileDecoratorPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.FileDecoratorServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.FileDecoratorServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestFileDecoratorInitHost_InitFails_DispenseErrors mirrors
// TestEventHookInitHost_InitFails_DispenseErrors: a decorator Init failure
// must fail the Dispense, not silently hand back a live client.
func TestFileDecoratorInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitFileDecorator{}
	plugins := map[string]plugin.Plugin{
		pb.FileDecoratorPluginName: &FileDecoratorPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.FileDecoratorPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the decorator's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the decorator's Init error (%q)", err.Error(), "boom")
	}
}

// TestFileDecoratorDecorateRoundTrip proves Decorate crosses the wire intact:
// the group name, the ref batch, and the returned map[string]any (JSON-encoded
// as values_json) all survive the gRPC round trip unmangled.
func TestFileDecoratorDecorateRoundTrip(t *testing.T) {
	p := &fakeFileDecorator{
		initCh: make(chan ext.Host, 1),
		values: map[string]any{
			"/dir/a.txt": []any{"red", "blue"},
			"/dir/b.txt": true,
		},
	}
	plugins := FileDecoratorPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.FileDecoratorPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.FileDecoratorServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.FileDecoratorServiceClient", raw)
	}

	ctx := context.Background()
	reply, err := svc.Decorate(ctx, &pb.DecorateRequest{
		Group: "_labels",
		Refs: []*pb.ObjRef{
			{StorageId: "1", Anchor: "/dir/a.txt"},
			{StorageId: "1", Anchor: "/dir/b.txt"},
		},
	})
	if err != nil {
		t.Fatalf("Decorate: %v", err)
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		t.Fatalf("Decorate returned error: %v", e)
	}

	if p.gotGroup != "_labels" {
		t.Fatalf("plugin saw group = %q, want _labels", p.gotGroup)
	}
	wantRefs := []ext.ObjRef{{StorageID: "1", Anchor: "/dir/a.txt"}, {StorageID: "1", Anchor: "/dir/b.txt"}}
	if len(p.gotRefs) != len(wantRefs) || p.gotRefs[0] != wantRefs[0] || p.gotRefs[1] != wantRefs[1] {
		t.Fatalf("plugin saw refs = %+v, want %+v", p.gotRefs, wantRefs)
	}

	var got map[string]any
	if err := json.Unmarshal(reply.ValuesJson, &got); err != nil {
		t.Fatalf("decode values_json: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("values map = %+v, want 2 entries", got)
	}
	if got["/dir/b.txt"] != true {
		t.Fatalf("values[/dir/b.txt] = %v, want true", got["/dir/b.txt"])
	}
	list, ok := got["/dir/a.txt"].([]any)
	if !ok || len(list) != 2 || list[0] != "red" || list[1] != "blue" {
		t.Fatalf("values[/dir/a.txt] = %v, want [red blue]", got["/dir/a.txt"])
	}
}

// TestFileDecoratorDecorateErrorRoundTrip proves a Decorate error surfaces as
// an ErrorReply rather than a transport failure.
func TestFileDecoratorDecorateErrorRoundTrip(t *testing.T) {
	p := &fakeFileDecorator{
		initCh: make(chan ext.Host, 1),
		err:    ext.NewError(ext.ErrCodeNotImplement, "no such group"),
	}
	plugins := FileDecoratorPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.FileDecoratorPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc := raw.(pb.FileDecoratorServiceClient)

	reply, err := svc.Decorate(context.Background(), &pb.DecorateRequest{Group: "_missing"})
	if err != nil {
		t.Fatalf("Decorate transport error: %v", err)
	}
	got := pb.ErrorFromProto(reply.Error)
	if got == nil {
		t.Fatal("want a business error, got nil")
	}
	code, ok := ext.CodeOf(got)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("error code = %v (ok=%v), want ErrCodeNotImplement", code, ok)
	}
}
