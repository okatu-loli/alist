package grpc

import (
	"context"
	"os/exec"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchFileDecoratorPlugin starts the filedecoratorplugin fixture over a
// real go-plugin subprocess. Mirrors launchEventHookPlugin one Kind over.
func launchFileDecoratorPlugin(t *testing.T) *host.FileDecoratorClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchFileDecorator(context.Background(), exec.Command(fileDecoratorPluginBin), nil)
	if err != nil {
		t.Fatalf("launch filedecorator plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestFileDecoratorGRPCRoundTrip is W9a's gRPC round-trip gate: drive
// Groups/Decorate against a real go-plugin subprocess and confirm the
// declared group and the returned values map cross the wire intact. Mirrors
// TestEventHookGRPCRoundTrip / TestAPIRouteGRPCRoundTrip one Kind over.
func TestFileDecoratorGRPCRoundTrip(t *testing.T) {
	c := launchFileDecoratorPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testfiledecorator" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	// Groups is derived client-side from the manifest's Contributions
	// (no separate RPC) — proves that derivation round-trips too.
	groups := c.Groups()
	if len(groups) != 1 || groups[0] != "_labels" {
		t.Fatalf("Groups = %v, want [_labels]", groups)
	}

	refs := []ext.ObjRef{{StorageID: "1", Anchor: "/dir/a.txt"}, {StorageID: "1", Anchor: "/dir/b.txt"}}
	values, err := c.Decorate(ctx, "_labels", refs)
	if err != nil {
		t.Fatalf("Decorate: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("Decorate values = %+v, want 2 entries", values)
	}
	for _, ref := range refs {
		if got := values[ext.RefKey(ref)]; got != "decorated:_labels" {
			t.Fatalf("values[%s] = %v, want decorated:_labels", ext.RefKey(ref), got)
		}
	}
}

// TestFileDecoratorGRPCDispatchThroughExtension drives the full host-side
// path this W9a delivers: extension.SetFileDecoratorFactory ->
// DecorateFields -> a real gRPC subprocess's Decorate, exactly the wiring
// server/handles/plugins.go installs in production (with
// pluginproc.Default.AcquireFileDecorator in place of this test's direct
// host.LaunchFileDecorator).
func TestFileDecoratorGRPCDispatchThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchFileDecoratorPlugin(t)

	extension.SetFileDecoratorFactory(func(e *extension.Entry) (ext.FileDecorator, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetFileDecoratorFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testfiledecorator",
		Kinds: []ext.Kind{ext.KindFileDecorator},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	refs := []ext.ObjRef{{StorageID: "1", Anchor: "/x"}}
	res := extension.DecorateFields(context.Background(), r, []string{"_labels"}, refs, 500*time.Millisecond)
	if len(res.Pending) != 0 {
		t.Fatalf("Pending = %v, want none", res.Pending)
	}
	if got := res.Fields["_labels"][ext.RefKey(refs[0])]; got != "decorated:_labels" {
		t.Fatalf("Fields[_labels][%s] = %v, want decorated:_labels", ext.RefKey(refs[0]), got)
	}
}
