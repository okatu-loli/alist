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
	"google.golang.org/grpc"
)

// fakeDriver is the plugin-side ext.StorageDriver used to observe whether
// InitHost delivered a working host to Init.
type fakeDriver struct {
	initCh chan ext.Host
}

func (d *fakeDriver) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake"} }
func (d *fakeDriver) DriverConfig() ext.DriverConfig     { return ext.DriverConfig{} }
func (d *fakeDriver) Shutdown(ctx context.Context) error { return nil }

func (d *fakeDriver) Init(ctx context.Context, host ext.Host) error {
	d.initCh <- host
	return nil
}

func (d *fakeDriver) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	return nil
}
func (d *fakeDriver) DropInstance(ctx context.Context, storageID string) error { return nil }
func (d *fakeDriver) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	return nil, nil
}
func (d *fakeDriver) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	return nil, nil
}

var _ ext.StorageDriver = (*fakeDriver)(nil)

// failingInitDriver is a driver whose Init always fails, used to prove that a
// business-error ErrorReply from driverServer.InitHost (as opposed to a gRPC
// transport error) still makes GRPCClient — and therefore Dispense — fail
// loudly, instead of silently handing back a client for a never-initialized
// driver.
type failingInitDriver struct{}

func (d *failingInitDriver) Manifest() ext.Manifest             { return ext.Manifest{ID: "failing-init"} }
func (d *failingInitDriver) DriverConfig() ext.DriverConfig     { return ext.DriverConfig{} }
func (d *failingInitDriver) Shutdown(ctx context.Context) error { return nil }

func (d *failingInitDriver) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (d *failingInitDriver) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	return nil
}
func (d *failingInitDriver) DropInstance(ctx context.Context, storageID string) error { return nil }
func (d *failingInitDriver) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	return nil, nil
}
func (d *failingInitDriver) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	return nil, nil
}

var _ ext.StorageDriver = (*failingInitDriver)(nil)

// fakeHostServer is a minimal HostService that answers LoadConfig with a fixed
// payload, so the roundtrip test can prove the host the driver receives is a
// live connection back to a real HostService, not just a non-nil value.
type fakeHostServer struct {
	pb.UnimplementedHostServiceServer
}

func (fakeHostServer) LoadConfig(ctx context.Context, _ *pb.LoadConfigRequest) (*pb.LoadConfigReply, error) {
	return &pb.LoadConfigReply{Config: []byte(`{"hello":"host"}`)}, nil
}

func hostServerFactory(opts []grpc.ServerOption) *grpc.Server {
	s := grpc.NewServer(opts...)
	pb.RegisterHostServiceServer(s, fakeHostServer{})
	return s
}

// TestInitHost_WithHostServerFactory proves the full handshake end to end using
// go-plugin's in-process test harness (plugin.TestPluginGRPCConn), which wires a
// real GRPCServer and GRPCClient together over a real broker — the same
// machinery a real subprocess launch uses, minus the exec.
//
// TestPluginGRPCConn registers ONE Plugin instance per name for both roles: at
// server Init() it calls GRPCServer(broker, server) on that instance (the
// "plugin process" side, using Impl); at Dispense() it calls
// GRPCClient(ctx, broker, conn) on the SAME instance (the "host process" side,
// using HostServerFactory). So a single StorageDriverPlugin{Impl, HostServerFactory}
// legitimately exercises both halves of the handshake in one process.
func TestInitHost_WithHostServerFactory(t *testing.T) {
	drv := &fakeDriver{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.PluginName: &StorageDriverPlugin{Impl: drv, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	// Dispense drives GRPCClient: because HostServerFactory != nil, it does
	// id := broker.NextId(); go broker.AcceptAndServe(id, hostServerFactory);
	// then BLOCKS on InitHost(ctx, {BrokerId: id}) before returning. The
	// InitHost RPC lands on driverServer.InitHost (plugin side), which Dials
	// id — racing the AcceptAndServe goroutine, but broker.Dial blocks until
	// AcceptAndServe has registered the listener for that id, so this is safe
	// — builds a host client over it, and calls drv.Init(ctx, host) inline,
	// synchronously, before InitHost returns its ErrorReply. So by the time
	// Dispense returns without error, Init has already completed.
	raw, err := client.Dispense(pb.PluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.DriverServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.DriverServiceClient", raw)
	}

	select {
	case h := <-drv.initCh:
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

// TestInitHost_NilFactory_InitNotCalled proves the pre-bridge behavior is
// unchanged: with HostServerFactory nil (PluginMap, as before this feature
// existed), GRPCClient never calls InitHost and the driver's Init is never
// invoked.
func TestInitHost_NilFactory_InitNotCalled(t *testing.T) {
	drv := &fakeDriver{initCh: make(chan ext.Host, 1)}
	plugins := PluginMap(drv) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.PluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.DriverServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.DriverServiceClient", raw)
	}

	select {
	case h := <-drv.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestInitHost_InitFails_DispenseErrors proves the fail-open-on-init bug is
// fixed: driverServer.InitHost reports a driver Init failure as a
// pb.ErrorReply, not a gRPC transport error (reply, nil) — so a naive
// `if err != nil` check on the InitHost call sees no error and GRPCClient
// would return a live client for a driver that never initialized. GRPCClient
// must also check reply.Error (via pb.ErrorFromProto) and fail the dispense.
func TestInitHost_InitFails_DispenseErrors(t *testing.T) {
	drv := &failingInitDriver{}
	plugins := map[string]plugin.Plugin{
		pb.PluginName: &StorageDriverPlugin{Impl: drv, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.PluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the plugin's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the driver's Init error (%q)", err.Error(), "boom")
	}
}
