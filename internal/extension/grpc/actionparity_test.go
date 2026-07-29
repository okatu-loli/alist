package grpc

import (
	"context"
	"os/exec"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchActionPlugin starts the actionplugin fixture over a real go-plugin
// subprocess. Mirrors launchAPIRoutePlugin one Kind over.
func launchActionPlugin(t *testing.T) *host.ActionClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchAction(context.Background(), exec.Command(actionPluginBin), nil)
	if err != nil {
		t.Fatalf("launch action plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestActionGRPCRoundTrip is W8b's gRPC round-trip gate: drive Invoke against
// a real go-plugin subprocess and confirm principal/actionID/args and the
// result cross the wire intact. Mirrors TestAPIRouteGRPCRoundTrip one Kind
// over.
func TestActionGRPCRoundTrip(t *testing.T) {
	c := launchActionPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testaction" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	res, err := c.Invoke(ctx, "principal-x", "echo", map[string]any{"path": "/x", "force": true})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("Invoke result type = %T, want map[string]any", res)
	}
	if m["principal"] != "principal-x" || m["action_id"] != "echo" {
		t.Fatalf("Invoke result = %+v", m)
	}
	argsOut, ok := m["args"].(map[string]any)
	if !ok || argsOut["path"] != "/x" || argsOut["force"] != true {
		t.Fatalf("Invoke result args = %+v", m["args"])
	}
}

// TestActionGRPCDispatchThroughExtension drives the full host-side path this
// W8b delivers: extension.SetActionFactory -> InvokeExtensionAction -> a real
// gRPC subprocess's Invoke, exactly the wiring server/handles/plugins.go
// installs in production (with pluginproc.Default.AcquireAction in place of
// this test's direct host.LaunchAction).
func TestActionGRPCDispatchThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchActionPlugin(t)

	extension.SetActionFactory(func(e *extension.Entry) (ext.Action, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetActionFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testaction",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			MenuActions: []ext.MenuAction{
				{ID: "echo", Label: map[string]string{"en": "Echo"}, Target: "global"},
			},
		},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	res, err := extension.InvokeExtensionAction(context.Background(), r, ext.Principal("principal-y"), "com.example.testaction", "echo",
		map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("InvokeExtensionAction: %v", err)
	}
	got, ok := res.(map[string]any)
	if !ok || got["action_id"] != "echo" {
		t.Fatalf("InvokeExtensionAction result = %+v", res)
	}
	argsOut, ok := got["args"].(map[string]any)
	if !ok || argsOut["k"] != "v" {
		t.Fatalf("InvokeExtensionAction result args = %+v", got["args"])
	}
	// InvokeExtensionAction threads the caller's principal through to the
	// gRPC leg's Invoke call — pin the full round trip end to end.
	if got["principal"] != "principal-y" {
		t.Fatalf("principal = %v, want principal-y threaded through to the real subprocess", got["principal"])
	}
}
