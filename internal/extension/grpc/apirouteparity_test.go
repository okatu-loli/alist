package grpc

import (
	"context"
	"os/exec"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchAPIRoutePlugin starts the apirouteplugin fixture over a real
// go-plugin subprocess. Mirrors launchBgPlugin/launchAuthPlugin one Kind over.
func launchAPIRoutePlugin(t *testing.T) *host.APIRouteClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchAPIRoute(context.Background(), exec.Command(apiRoutePluginBin), nil)
	if err != nil {
		t.Fatalf("launch apiroute plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestAPIRouteGRPCRoundTrip is W8a's gRPC round-trip gate: drive Routes/Handle
// against a real go-plugin subprocess and confirm both cross the wire intact.
// Mirrors TestAuthProviderGRPCRoundTrip / TestBackgroundServiceGRPCRoundTrip
// one Kind over.
func TestAPIRouteGRPCRoundTrip(t *testing.T) {
	c := launchAPIRoutePlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testapiroute" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	decls := c.Routes()
	if len(decls) != 2 || decls[0].Prefix != "/webhook" || decls[0].Public ||
		decls[1].Prefix != "/s" || !decls[1].Public {
		t.Fatalf("Routes = %+v", decls)
	}

	resp, err := c.Handle(ctx, &ext.HTTPRequest{Method: "POST", URL: "/webhook/x"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != "handled:POST:/webhook/x" {
		t.Fatalf("Handle resp = %+v", resp)
	}
}

// TestAPIRouteGRPCDispatchThroughExtension drives the full host-side path this
// W8a delivers: extension.SetAPIRouteFactory -> DispatchAPIRoute -> a real
// gRPC subprocess's Handle, exactly the wiring server/handles/plugins.go
// installs in production (with pluginproc.Default.AcquireAPIRoute in place of
// this test's direct host.LaunchAPIRoute).
func TestAPIRouteGRPCDispatchThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchAPIRoutePlugin(t)

	extension.SetAPIRouteFactory(func(e *extension.Entry) (ext.APIRoute, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetAPIRouteFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testapiroute",
		Kinds: []ext.Kind{ext.KindAPIRoute},
		Permissions: ext.Permissions{
			HostAPIs: []string{extension.CapPublicRoutes},
		},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	resp, err := extension.DispatchAPIRoute(context.Background(), r, "com.example.testapiroute", "/webhook/hook", false,
		&ext.HTTPRequest{Method: "POST", URL: "/webhook/hook", Body: []byte("payload")})
	if err != nil {
		t.Fatalf("DispatchAPIRoute (authenticated): %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != "handled:POST:/webhook/hook" {
		t.Fatalf("authenticated dispatch resp = %+v", resp)
	}

	resp, err = extension.DispatchAPIRoute(context.Background(), r, "com.example.testapiroute", "/s/abc", true,
		&ext.HTTPRequest{Method: "GET", URL: "/s/abc"})
	if err != nil {
		t.Fatalf("DispatchAPIRoute (public): %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != "handled:GET:/s/abc" {
		t.Fatalf("public dispatch resp = %+v", resp)
	}
}
