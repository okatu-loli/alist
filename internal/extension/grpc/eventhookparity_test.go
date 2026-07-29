package grpc

import (
	"context"
	"fmt"
	"os/exec"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchEventHookPlugin starts the eventhookplugin fixture over a real
// go-plugin subprocess. Mirrors launchActionPlugin one Kind over.
func launchEventHookPlugin(t *testing.T) *host.EventHookClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchEventHook(context.Background(), exec.Command(eventHookPluginBin), nil)
	if err != nil {
		t.Fatalf("launch eventhook plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestEventHookGRPCRoundTrip is W7's gRPC round-trip gate: drive OnEvent
// against a real go-plugin subprocess and confirm every ext.Event field
// crosses the wire intact (the eventhookplugin fixture echoes them back via
// the OnEvent error message — see its doc comment). Mirrors
// TestAPIRouteGRPCRoundTrip / TestActionGRPCRoundTrip one Kind over.
func TestEventHookGRPCRoundTrip(t *testing.T) {
	c := launchEventHookPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testeventhook" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	ev := ext.Event{
		Type:      ext.EventFSMove,
		UnixMilli: 42,
		Path:      "/src",
		DstPath:   "/dst",
		Actor:     "carol",
	}
	err := c.OnEvent(ctx, ev)
	if err == nil {
		t.Fatal("OnEvent: want the fixture's echo error, got nil")
	}
	want := fmt.Sprintf("echo:%s:%d:%s:%s:%s", ev.Type, ev.UnixMilli, ev.Path, ev.DstPath, ev.Actor)
	if err.Error() != want {
		t.Fatalf("OnEvent echo = %q, want %q (Event must cross the wire intact)", err.Error(), want)
	}
}

// TestEventHookGRPCDispatchThroughExtension drives the full host-side path
// this W7 delivers: extension.SetEventHookFactory -> DispatchEvent -> a real
// gRPC subprocess's OnEvent, exactly the wiring server/handles/plugins.go
// installs in production (with pluginproc.Default.AcquireEventHook in place
// of this test's direct host.LaunchEventHook).
func TestEventHookGRPCDispatchThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchEventHookPlugin(t)

	extension.SetEventHookFactory(func(e *extension.Entry) (ext.EventHook, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetEventHookFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testeventhook",
		Kinds: []ext.Kind{ext.KindEventHook},
		Permissions: ext.Permissions{
			HostAPIs: []string{extension.CapEvents},
		},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	// DispatchEvent isolates OnEvent's error (it is a notification sink, not a
	// veto — see ext.EventHook's doc comment), so a subprocess round trip that
	// "fails" via the fixture's echo error must not panic or propagate: the
	// call below simply must not block or crash.
	extension.DispatchEvent(context.Background(), r, ext.Event{
		Type: ext.EventFSMkdir, Path: "/dispatched", Actor: "dave",
	})
}
