package grpcplugin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// fakeAPIRoute is the plugin-side ext.APIRoute used to observe whether InitHost
// delivered a working host to Init, and to exercise Routes/Handle over the
// wire. Mirrors fakeAuthProvider (authinithost_test.go) one Kind over.
type fakeAPIRoute struct {
	initCh chan ext.Host
	routes []ext.RouteDecl
}

func (p *fakeAPIRoute) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-apiroute"} }
func (p *fakeAPIRoute) Shutdown(ctx context.Context) error { return nil }

func (p *fakeAPIRoute) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeAPIRoute) Routes() []ext.RouteDecl { return p.routes }

func (p *fakeAPIRoute) Handle(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{
		Status: 201,
		Header: http.Header{"X-Reply": {"yes"}},
		Body:   []byte("method=" + req.Method + " url=" + req.URL + " body=" + string(req.Body) + " hdr=" + req.Header.Get("X-Req")),
	}, nil
}

var _ ext.APIRoute = (*fakeAPIRoute)(nil)

// failingInitAPIRoute is an APIRoute whose Init always fails, used to prove
// that a business-error ErrorReply from apiRouteServer.InitHost (as opposed to
// a gRPC transport error) still makes GRPCClient — and therefore Dispense —
// fail loudly. Mirrors failingInitAuthProvider.
type failingInitAPIRoute struct{}

func (p *failingInitAPIRoute) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-apiroute"}
}
func (p *failingInitAPIRoute) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitAPIRoute) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitAPIRoute) Routes() []ext.RouteDecl { return nil }
func (p *failingInitAPIRoute) Handle(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return nil, nil
}

var _ ext.APIRoute = (*failingInitAPIRoute)(nil)

// TestAPIRouteInitHost_WithHostServerFactory mirrors
// TestAuthProviderInitHost_WithHostServerFactory one Kind over: it proves the
// full handshake end to end using go-plugin's in-process test harness.
func TestAPIRouteInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeAPIRoute{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.APIRoutePluginName: &APIRoutePlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.APIRoutePluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.APIRouteServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.APIRouteServiceClient", raw)
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

// TestAPIRouteInitHost_NilFactory_InitNotCalled mirrors
// TestAuthProviderInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (APIRoutePluginMap, as before this feature existed), GRPCClient never calls
// InitHost and the route's Init is never invoked.
func TestAPIRouteInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeAPIRoute{initCh: make(chan ext.Host, 1)}
	plugins := APIRoutePluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.APIRoutePluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.APIRouteServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.APIRouteServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestAPIRouteInitHost_InitFails_DispenseErrors mirrors
// TestAuthProviderInitHost_InitFails_DispenseErrors: a route Init failure must
// fail the Dispense, not silently hand back a live client.
func TestAPIRouteInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitAPIRoute{}
	plugins := map[string]plugin.Plugin{
		pb.APIRoutePluginName: &APIRoutePlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.APIRoutePluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the plugin's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the route's Init error (%q)", err.Error(), "boom")
	}
}

// TestAPIRouteRoutesAndHandleRoundTrip proves Routes() and Handle() cross the
// wire intact: the declared RouteDecl set (prefix/public) and the Handle
// response's status/header/body survive the gRPC round trip unmangled.
func TestAPIRouteRoutesAndHandleRoundTrip(t *testing.T) {
	p := &fakeAPIRoute{
		initCh: make(chan ext.Host, 1),
		routes: []ext.RouteDecl{
			{Prefix: "/webhook", Public: false},
			{Prefix: "/s", Public: true},
		},
	}
	plugins := APIRoutePluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.APIRoutePluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.APIRouteServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.APIRouteServiceClient", raw)
	}

	ctx := context.Background()

	rr, err := svc.Routes(ctx, &pb.Empty{})
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(rr.Routes) != 2 {
		t.Fatalf("Routes count = %d, want 2", len(rr.Routes))
	}
	if rr.Routes[0].Prefix != "/webhook" || rr.Routes[0].Public {
		t.Fatalf("Routes[0] = %+v, want prefix=/webhook public=false", rr.Routes[0])
	}
	if rr.Routes[1].Prefix != "/s" || !rr.Routes[1].Public {
		t.Fatalf("Routes[1] = %+v, want prefix=/s public=true", rr.Routes[1])
	}

	hr, err := svc.Handle(ctx, &pb.HTTPDoRequest{
		Method: "POST",
		Url:    "/webhook/abc",
		Header: pb.HeaderToProto(http.Header{"X-Req": {"req-val"}}),
		Body:   []byte("payload"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if e := pb.ErrorFromProto(hr.Error); e != nil {
		t.Fatalf("Handle returned error: %v", e)
	}
	if hr.Status != 201 {
		t.Fatalf("Handle status = %d, want 201", hr.Status)
	}
	if got := string(hr.Body); got != "method=POST url=/webhook/abc body=payload hdr=req-val" {
		t.Fatalf("Handle body = %q", got)
	}
	hdr := pb.HeaderFromProto(hr.Header)
	if hdr.Get("X-Reply") != "yes" {
		t.Fatalf("Handle header X-Reply = %q, want yes", hdr.Get("X-Reply"))
	}
}
