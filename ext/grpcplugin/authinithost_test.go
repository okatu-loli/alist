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

// fakeAuthProvider is the plugin-side ext.AuthProvider used to observe whether
// InitHost delivered a working host to Init. Mirrors fakeDriver
// (inithost_test.go) one Kind over.
type fakeAuthProvider struct {
	initCh chan ext.Host
}

func (p *fakeAuthProvider) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-auth"} }
func (p *fakeAuthProvider) Shutdown(ctx context.Context) error { return nil }

func (p *fakeAuthProvider) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeAuthProvider) AuthURL(ctx context.Context, req ext.AuthURLRequest) (string, error) {
	return "", nil
}
func (p *fakeAuthProvider) Exchange(ctx context.Context, req ext.ExchangeRequest) (ext.Token, error) {
	return ext.Token{}, nil
}
func (p *fakeAuthProvider) Refresh(ctx context.Context, refreshToken string) (ext.Token, error) {
	return ext.Token{}, nil
}

var _ ext.AuthProvider = (*fakeAuthProvider)(nil)

// failingInitAuthProvider is an AuthProvider whose Init always fails, used to
// prove that a business-error ErrorReply from authProviderServer.InitHost (as
// opposed to a gRPC transport error) still makes GRPCClient — and therefore
// Dispense — fail loudly. Mirrors failingInitDriver.
type failingInitAuthProvider struct{}

func (p *failingInitAuthProvider) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-auth"}
}
func (p *failingInitAuthProvider) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitAuthProvider) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitAuthProvider) AuthURL(ctx context.Context, req ext.AuthURLRequest) (string, error) {
	return "", nil
}
func (p *failingInitAuthProvider) Exchange(ctx context.Context, req ext.ExchangeRequest) (ext.Token, error) {
	return ext.Token{}, nil
}
func (p *failingInitAuthProvider) Refresh(ctx context.Context, refreshToken string) (ext.Token, error) {
	return ext.Token{}, nil
}

var _ ext.AuthProvider = (*failingInitAuthProvider)(nil)

// TestAuthProviderInitHost_WithHostServerFactory mirrors
// TestInitHost_WithHostServerFactory (StorageDriver) one Kind over: it proves
// the full handshake end to end using go-plugin's in-process test harness.
func TestAuthProviderInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeAuthProvider{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.AuthPluginName: &AuthProviderPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AuthPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AuthProviderServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AuthProviderServiceClient", raw)
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

// TestAuthProviderInitHost_NilFactory_InitNotCalled mirrors
// TestInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (AuthProviderPluginMap, as before this feature existed), GRPCClient never
// calls InitHost and the provider's Init is never invoked.
func TestAuthProviderInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeAuthProvider{initCh: make(chan ext.Host, 1)}
	plugins := AuthProviderPluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AuthPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AuthProviderServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AuthProviderServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestAuthProviderInitHost_InitFails_DispenseErrors mirrors
// TestInitHost_InitFails_DispenseErrors: a provider Init failure must fail the
// Dispense, not silently hand back a live client.
func TestAuthProviderInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitAuthProvider{}
	plugins := map[string]plugin.Plugin{
		pb.AuthPluginName: &AuthProviderPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AuthPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the plugin's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the provider's Init error (%q)", err.Error(), "boom")
	}
}
