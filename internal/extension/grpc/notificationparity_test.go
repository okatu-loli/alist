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

// launchNotificationPlugin starts the notificationplugin fixture over a real
// go-plugin subprocess. Mirrors launchEventHookPlugin one Kind over.
func launchNotificationPlugin(t *testing.T) *host.NotificationClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchNotification(context.Background(), exec.Command(notificationPluginBin), nil)
	if err != nil {
		t.Fatalf("launch notification plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestNotificationGRPCRoundTrip is W7b's gRPC round-trip gate: drive Notify
// against a real go-plugin subprocess and confirm every ext.Notice field
// crosses the wire intact (the notificationplugin fixture echoes them back
// via the Notify error message — see its doc comment). Mirrors
// TestEventHookGRPCRoundTrip one Kind over.
func TestNotificationGRPCRoundTrip(t *testing.T) {
	c := launchNotificationPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testnotification" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	n := ext.Notice{
		Level:     ext.NoticeWarn,
		Title:     "storage offline",
		Body:      "the mount stopped responding",
		Source:    "storage",
		UnixMilli: 42,
	}
	err := c.Notify(ctx, n)
	if err == nil {
		t.Fatal("Notify: want the fixture's echo error, got nil")
	}
	want := fmt.Sprintf("echo:%s:%s:%s:%s:%d", n.Level, n.Title, n.Body, n.Source, n.UnixMilli)
	if err.Error() != want {
		t.Fatalf("Notify echo = %q, want %q (Notice must cross the wire intact)", err.Error(), want)
	}
}

// TestNotificationGRPCDispatchThroughExtension drives the full host-side path
// this W7b delivers: extension.SetNotificationFactory -> DispatchNotification
// -> a real gRPC subprocess's Notify, exactly the wiring
// server/handles/plugins.go installs in production (with
// pluginproc.Default.AcquireNotification in place of this test's direct
// host.LaunchNotification).
func TestNotificationGRPCDispatchThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchNotificationPlugin(t)

	extension.SetNotificationFactory(func(e *extension.Entry) (ext.Notification, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetNotificationFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testnotification",
		Kinds: []ext.Kind{ext.KindNotification},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	// DispatchNotification isolates Notify's error (it is a notification sink,
	// not a veto — see ext.Notification's doc comment), so a subprocess round
	// trip that "fails" via the fixture's echo error must not panic or
	// propagate: the call below simply must not block or crash.
	extension.DispatchNotification(context.Background(), r, ext.Notice{
		Level: ext.NoticeInfo, Title: "dispatched",
	})
}
