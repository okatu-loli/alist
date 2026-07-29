package grpc

import (
	"context"
	"os/exec"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchAccessProtocolPlugin starts the accessprotocolplugin fixture over a
// real go-plugin subprocess. Mirrors launchAPIRoutePlugin one Kind over.
func launchAccessProtocolPlugin(t *testing.T) *host.AccessProtocolClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchAccessProtocol(context.Background(), exec.Command(accessProtocolPluginBin), nil)
	if err != nil {
		t.Fatalf("launch accessprotocol plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestAccessProtocolGRPCRoundTrip is W9a's gRPC round-trip gate: drive
// ProtocolInfo/ServeHTTP against a real go-plugin subprocess and confirm both
// cross the wire intact. Mirrors TestAPIRouteGRPCRoundTrip /
// TestEventHookGRPCRoundTrip one Kind over.
func TestAccessProtocolGRPCRoundTrip(t *testing.T) {
	c := launchAccessProtocolPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testaccessprotocol" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	info := c.ProtocolInfo()
	if info.Scheme != "webdav" || info.Transport != ext.TransportHTTP || info.MountPattern != "/dav" {
		t.Fatalf("ProtocolInfo = %+v", info)
	}

	resp, err := c.ServeHTTP(ctx, &ext.HTTPRequest{Method: "PROPFIND", URL: "/dav/x"})
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if resp.Status != 207 || string(resp.Body) != "served:PROPFIND:/dav/x" {
		t.Fatalf("ServeHTTP resp = %+v", resp)
	}
}

// TestAccessProtocolHandleConnNativeOnly proves the gRPC client's HandleConn
// never attempts a wire call — it fails closed with a "native only" error, per
// design §5 (a raw net.Conn does not cross the gRPC boundary).
func TestAccessProtocolHandleConnNativeOnly(t *testing.T) {
	c := launchAccessProtocolPlugin(t)

	err := c.HandleConn(context.Background(), nil)
	if err == nil {
		t.Fatal("HandleConn on a gRPC AccessProtocol client: want an error, got nil")
	}
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("HandleConn error code = %v (ok=%v), want ErrCodeNotImplement", code, ok)
	}
}

// TestAccessProtocolGRPCDispatchThroughExtension drives the full host-side
// path this W9a delivers: extension.SetAccessProtocolFactory ->
// DispatchAccessProtocolHTTP -> a real gRPC subprocess's ServeHTTP, exactly
// the wiring server/handles/plugins.go installs in production (with
// pluginproc.Default.AcquireAccessProtocol in place of this test's direct
// host.LaunchAccessProtocol).
func TestAccessProtocolGRPCDispatchThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchAccessProtocolPlugin(t)

	extension.SetAccessProtocolFactory(func(e *extension.Entry) (ext.AccessProtocol, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetAccessProtocolFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testaccessprotocol",
		Kinds: []ext.Kind{ext.KindAccessProtocol},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	resp, err := extension.DispatchAccessProtocolHTTP(context.Background(), r, "com.example.testaccessprotocol",
		&ext.HTTPRequest{Method: "GET", URL: "/dav/y"})
	if err != nil {
		t.Fatalf("DispatchAccessProtocolHTTP: %v", err)
	}
	if resp.Status != 207 || string(resp.Body) != "served:GET:/dav/y" {
		t.Fatalf("dispatch resp = %+v", resp)
	}
}
