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

// fakeAuthNMethod is the plugin-side ext.AuthNMethod used to observe whether
// InitHost delivered a working host to Init. Mirrors fakeDriver
// (inithost_test.go) one Kind over.
type fakeAuthNMethod struct {
	initCh chan ext.Host
}

func (m *fakeAuthNMethod) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-authn"} }
func (m *fakeAuthNMethod) Shutdown(ctx context.Context) error { return nil }

func (m *fakeAuthNMethod) Init(ctx context.Context, host ext.Host) error {
	m.initCh <- host
	return nil
}

func (m *fakeAuthNMethod) MethodInfo() ext.AuthMethodInfo { return ext.AuthMethodInfo{ID: "fake"} }
func (m *fakeAuthNMethod) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	return ext.AuthNChallenge{}, nil
}
func (m *fakeAuthNMethod) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	return ext.AuthNResult{}, nil
}

var _ ext.AuthNMethod = (*fakeAuthNMethod)(nil)

// failingInitAuthNMethod is an AuthNMethod whose Init always fails, used to
// prove that a business-error ErrorReply from authNMethodServer.InitHost (as
// opposed to a gRPC transport error) still makes GRPCClient — and therefore
// Dispense — fail loudly. Mirrors failingInitDriver.
type failingInitAuthNMethod struct{}

func (m *failingInitAuthNMethod) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-authn"}
}
func (m *failingInitAuthNMethod) Shutdown(ctx context.Context) error { return nil }

func (m *failingInitAuthNMethod) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (m *failingInitAuthNMethod) MethodInfo() ext.AuthMethodInfo {
	return ext.AuthMethodInfo{ID: "failing"}
}
func (m *failingInitAuthNMethod) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	return ext.AuthNChallenge{}, nil
}
func (m *failingInitAuthNMethod) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	return ext.AuthNResult{}, nil
}

var _ ext.AuthNMethod = (*failingInitAuthNMethod)(nil)

// TestAuthNMethodInitHost_WithHostServerFactory mirrors
// TestInitHost_WithHostServerFactory (StorageDriver) one Kind over: it proves
// the full handshake end to end using go-plugin's in-process test harness.
func TestAuthNMethodInitHost_WithHostServerFactory(t *testing.T) {
	m := &fakeAuthNMethod{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.AuthNPluginName: &AuthNMethodPlugin{Impl: m, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AuthNPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AuthNMethodServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AuthNMethodServiceClient", raw)
	}

	select {
	case h := <-m.initCh:
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

// TestAuthNMethodInitHost_NilFactory_InitNotCalled mirrors
// TestInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (AuthNMethodPluginMap, as before this feature existed), GRPCClient never
// calls InitHost and the method's Init is never invoked.
func TestAuthNMethodInitHost_NilFactory_InitNotCalled(t *testing.T) {
	m := &fakeAuthNMethod{initCh: make(chan ext.Host, 1)}
	plugins := AuthNMethodPluginMap(m) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AuthNPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AuthNMethodServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AuthNMethodServiceClient", raw)
	}

	select {
	case h := <-m.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestAuthNMethodInitHost_InitFails_DispenseErrors mirrors
// TestInitHost_InitFails_DispenseErrors: a method Init failure must fail the
// Dispense, not silently hand back a live client.
func TestAuthNMethodInitHost_InitFails_DispenseErrors(t *testing.T) {
	m := &failingInitAuthNMethod{}
	plugins := map[string]plugin.Plugin{
		pb.AuthNPluginName: &AuthNMethodPlugin{Impl: m, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AuthNPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the plugin's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the method's Init error (%q)", err.Error(), "boom")
	}
}
