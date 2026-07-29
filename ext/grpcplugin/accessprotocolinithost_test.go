package grpcplugin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// fakeAccessProtocol is the plugin-side ext.AccessProtocol (TransportHTTP)
// used to observe whether InitHost delivered a working host to Init, and to
// exercise ProtocolInfo/ServeHTTP over the wire. Mirrors fakeAPIRoute
// (apirouteinithost_test.go) one Kind over. HandleConn is never exercised
// over gRPC (design §5) — it just satisfies the ext.AccessProtocol interface.
type fakeAccessProtocol struct {
	initCh chan ext.Host
	info   ext.ProtocolInfo
}

func (p *fakeAccessProtocol) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-accessprotocol"} }
func (p *fakeAccessProtocol) Shutdown(ctx context.Context) error { return nil }

func (p *fakeAccessProtocol) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeAccessProtocol) ProtocolInfo() ext.ProtocolInfo { return p.info }

func (p *fakeAccessProtocol) ServeHTTP(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{
		Status: 207,
		Header: http.Header{"X-Reply": {"yes"}},
		Body:   []byte("method=" + req.Method + " url=" + req.URL + " body=" + string(req.Body) + " hdr=" + req.Header.Get("X-Req")),
	}, nil
}

func (p *fakeAccessProtocol) HandleConn(ctx context.Context, conn net.Conn) error {
	return ext.NewError(ext.ErrCodeNotImplement, "http transport")
}

var _ ext.AccessProtocol = (*fakeAccessProtocol)(nil)

// failingInitAccessProtocol is an AccessProtocol whose Init always fails,
// used to prove that a business-error ErrorReply from
// accessProtocolServer.InitHost (as opposed to a gRPC transport error) still
// makes GRPCClient — and therefore Dispense — fail loudly. Mirrors
// failingInitAPIRoute.
type failingInitAccessProtocol struct{}

func (p *failingInitAccessProtocol) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-accessprotocol"}
}
func (p *failingInitAccessProtocol) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitAccessProtocol) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitAccessProtocol) ProtocolInfo() ext.ProtocolInfo { return ext.ProtocolInfo{} }
func (p *failingInitAccessProtocol) ServeHTTP(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return nil, nil
}
func (p *failingInitAccessProtocol) HandleConn(ctx context.Context, conn net.Conn) error {
	return nil
}

var _ ext.AccessProtocol = (*failingInitAccessProtocol)(nil)

// TestAccessProtocolInitHost_WithHostServerFactory mirrors
// TestAPIRouteInitHost_WithHostServerFactory one Kind over: it proves the
// full handshake end to end using go-plugin's in-process test harness.
func TestAccessProtocolInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeAccessProtocol{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.AccessProtocolPluginName: &AccessProtocolPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AccessProtocolPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AccessProtocolServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AccessProtocolServiceClient", raw)
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

// TestAccessProtocolInitHost_NilFactory_InitNotCalled mirrors
// TestAPIRouteInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (AccessProtocolPluginMap, as before this feature existed), GRPCClient never
// calls InitHost and the protocol's Init is never invoked.
func TestAccessProtocolInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeAccessProtocol{initCh: make(chan ext.Host, 1)}
	plugins := AccessProtocolPluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AccessProtocolPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AccessProtocolServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AccessProtocolServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestAccessProtocolInitHost_InitFails_DispenseErrors mirrors
// TestAPIRouteInitHost_InitFails_DispenseErrors: a protocol Init failure must
// fail the Dispense, not silently hand back a live client.
func TestAccessProtocolInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitAccessProtocol{}
	plugins := map[string]plugin.Plugin{
		pb.AccessProtocolPluginName: &AccessProtocolPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AccessProtocolPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the protocol's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the protocol's Init error (%q)", err.Error(), "boom")
	}
}

// TestAccessProtocolInfoAndServeHTTPRoundTrip proves ProtocolInfo() and
// ServeHTTP() cross the wire intact: the declared scheme/transport/mount
// pattern and the ServeHTTP response's status/header/body survive the gRPC
// round trip unmangled.
func TestAccessProtocolInfoAndServeHTTPRoundTrip(t *testing.T) {
	p := &fakeAccessProtocol{
		initCh: make(chan ext.Host, 1),
		info: ext.ProtocolInfo{
			Scheme:       "webdav",
			Transport:    ext.TransportHTTP,
			MountPattern: "/dav",
		},
	}
	plugins := AccessProtocolPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AccessProtocolPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.AccessProtocolServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.AccessProtocolServiceClient", raw)
	}

	ctx := context.Background()

	ir, err := svc.ProtocolInfo(ctx, &pb.Empty{})
	if err != nil {
		t.Fatalf("ProtocolInfo: %v", err)
	}
	if ir.Scheme != "webdav" || ir.Transport != int32(ext.TransportHTTP) || ir.MountPattern != "/dav" {
		t.Fatalf("ProtocolInfo = %+v, want scheme=webdav transport=0 mount=/dav", ir)
	}

	hr, err := svc.ServeHTTP(ctx, &pb.HTTPDoRequest{
		Method: "PROPFIND",
		Url:    "/dav/dir",
		Header: pb.HeaderToProto(http.Header{"X-Req": {"req-val"}}),
		Body:   []byte("payload"),
	})
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if e := pb.ErrorFromProto(hr.Error); e != nil {
		t.Fatalf("ServeHTTP returned error: %v", e)
	}
	if hr.Status != 207 {
		t.Fatalf("ServeHTTP status = %d, want 207", hr.Status)
	}
	if got := string(hr.Body); got != "method=PROPFIND url=/dav/dir body=payload hdr=req-val" {
		t.Fatalf("ServeHTTP body = %q", got)
	}
	hdr := pb.HeaderFromProto(hr.Header)
	if hdr.Get("X-Reply") != "yes" {
		t.Fatalf("ServeHTTP header X-Reply = %q, want yes", hdr.Get("X-Reply"))
	}
}
