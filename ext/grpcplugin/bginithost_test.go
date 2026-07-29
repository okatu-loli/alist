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

// fakeBackgroundService is the plugin-side ext.BackgroundService used to
// observe whether InitHost delivered a working host to Init. Mirrors
// fakeDriver (inithost_test.go) one Kind over.
type fakeBackgroundService struct {
	initCh chan ext.Host
}

func (s *fakeBackgroundService) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-bg"} }
func (s *fakeBackgroundService) Shutdown(ctx context.Context) error { return nil }

func (s *fakeBackgroundService) Init(ctx context.Context, host ext.Host) error {
	s.initCh <- host
	return nil
}

func (s *fakeBackgroundService) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *fakeBackgroundService) Status(ctx context.Context) (ext.ServiceStatus, error) {
	return ext.ServiceStatus{State: ext.ServiceStateStopped}, nil
}

var _ ext.BackgroundService = (*fakeBackgroundService)(nil)

// failingInitBackgroundService is a BackgroundService whose Init always
// fails, used to prove that a business-error ErrorReply from
// backgroundServiceServer.InitHost (as opposed to a gRPC transport error)
// still makes GRPCClient — and therefore Dispense — fail loudly. Mirrors
// failingInitDriver.
type failingInitBackgroundService struct{}

func (s *failingInitBackgroundService) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-bg"}
}
func (s *failingInitBackgroundService) Shutdown(ctx context.Context) error { return nil }

func (s *failingInitBackgroundService) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (s *failingInitBackgroundService) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *failingInitBackgroundService) Status(ctx context.Context) (ext.ServiceStatus, error) {
	return ext.ServiceStatus{State: ext.ServiceStateStopped}, nil
}

var _ ext.BackgroundService = (*failingInitBackgroundService)(nil)

// TestBackgroundServiceInitHost_WithHostServerFactory mirrors
// TestInitHost_WithHostServerFactory (StorageDriver) one Kind over: it proves
// the full handshake end to end using go-plugin's in-process test harness.
func TestBackgroundServiceInitHost_WithHostServerFactory(t *testing.T) {
	s := &fakeBackgroundService{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.BgServicePluginName: &BackgroundServicePlugin{Impl: s, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.BgServicePluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.BackgroundServiceServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.BackgroundServiceServiceClient", raw)
	}

	select {
	case h := <-s.initCh:
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

// TestBackgroundServiceInitHost_NilFactory_InitNotCalled mirrors
// TestInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (BackgroundServicePluginMap, as before this feature existed), GRPCClient
// never calls InitHost and the service's Init is never invoked.
func TestBackgroundServiceInitHost_NilFactory_InitNotCalled(t *testing.T) {
	s := &fakeBackgroundService{initCh: make(chan ext.Host, 1)}
	plugins := BackgroundServicePluginMap(s) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.BgServicePluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.BackgroundServiceServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.BackgroundServiceServiceClient", raw)
	}

	select {
	case h := <-s.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestBackgroundServiceInitHost_InitFails_DispenseErrors mirrors
// TestInitHost_InitFails_DispenseErrors: a service Init failure must fail the
// Dispense, not silently hand back a live client.
func TestBackgroundServiceInitHost_InitFails_DispenseErrors(t *testing.T) {
	s := &failingInitBackgroundService{}
	plugins := map[string]plugin.Plugin{
		pb.BgServicePluginName: &BackgroundServicePlugin{Impl: s, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.BgServicePluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the plugin's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the service's Init error (%q)", err.Error(), "boom")
	}
}
