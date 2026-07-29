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

// launchOfflineDownloadToolPlugin starts the offlinedownloadtoolplugin
// fixture over a real go-plugin subprocess. Mirrors launchNotificationPlugin
// one Kind over.
func launchOfflineDownloadToolPlugin(t *testing.T) *host.OfflineDownloadToolClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchOfflineDownloadTool(context.Background(), exec.Command(offlineDownloadToolPluginBin), nil)
	if err != nil {
		t.Fatalf("launch offline download tool plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestOfflineDownloadToolGRPCRoundTrip is Phase E ring 1's gRPC round-trip
// gate: drive Config/AddURL/Remove/Status against a real go-plugin subprocess
// and confirm every field crosses the wire intact. AddURL/Status prove
// fidelity by deriving their positive return values from the request (the
// fixture's doc comment explains why, since — unlike Notify/OnEvent — they
// have a real return channel besides error); Remove, which has no such
// channel, uses the same echo-via-error trick as those other Kinds. Mirrors
// TestNotificationGRPCRoundTrip one Kind over.
func TestOfflineDownloadToolGRPCRoundTrip(t *testing.T) {
	c := launchOfflineDownloadToolPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testofflinedownloadtool" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	if cfg := c.Config(); cfg.Name != "echotool" {
		t.Fatalf("Config().Name = %q, want echotool", cfg.Name)
	}

	args := ext.AddArgs{URL: "http://example.com/f.bin", TempDir: "/tmp/dl", UID: "user-7"}
	gid, err := c.AddURL(ctx, args)
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	wantGID := fmt.Sprintf("gid:%s:%s:%s", args.URL, args.TempDir, args.UID)
	if gid != wantGID {
		t.Fatalf("AddURL gid = %q, want %q (AddArgs must cross the wire intact)", gid, wantGID)
	}

	err = c.Remove(ctx, gid)
	if err == nil {
		t.Fatal("Remove: want the fixture's echo error, got nil")
	}
	wantRemoveErr := fmt.Sprintf("removed:%s", gid)
	if err.Error() != wantRemoveErr {
		t.Fatalf("Remove echo = %q, want %q", err.Error(), wantRemoveErr)
	}

	st, err := c.Status(ctx, gid)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.TotalBytes != 12345 || st.Progress != 42.5 || st.NewGID != "new-"+gid ||
		!st.Completed || st.Status != "status-for-"+gid || st.Err != "err-for-"+gid {
		t.Fatalf("Status = %+v, fields must cross the wire intact for gid %q", st, gid)
	}

	// The "errgid" sentinel proves Status's error leg crosses intact too.
	if _, err := c.Status(ctx, "errgid"); err == nil {
		t.Fatal("Status(errgid): want an error, got nil")
	} else if want := "status error for errgid"; err.Error() != want {
		t.Fatalf("Status(errgid) error = %q, want %q", err.Error(), want)
	}
}

// TestOfflineDownloadToolGRPCFindByNameThroughExtension drives the full
// host-side path this Phase E ring 1a delivers: extension.
// SetOfflineDownloadToolFactory -> extension.FindOfflineDownloadToolByName ->
// a real gRPC subprocess's Config/AddURL, exactly the wiring
// server/handles/plugins.go installs in production (with
// pluginproc.Default.AcquireOfflineDownloadTool in place of this test's
// direct host.LaunchOfflineDownloadTool), and internal/offline_download.
// ToolByName's own miss-fallback in place of calling
// FindOfflineDownloadToolByName directly.
func TestOfflineDownloadToolGRPCFindByNameThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchOfflineDownloadToolPlugin(t)

	extension.SetOfflineDownloadToolFactory(func(e *extension.Entry) (ext.OfflineDownloadTool, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetOfflineDownloadToolFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testofflinedownloadtool",
		Kinds: []ext.Kind{ext.KindOfflineDownloadTool},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	tool, ok := extension.FindOfflineDownloadToolByName(r, "echotool")
	if !ok || tool == nil {
		t.Fatalf("FindOfflineDownloadToolByName(echotool) = %v, %v; want a resolved tool", tool, ok)
	}

	gid, err := tool.AddURL(context.Background(), ext.AddArgs{URL: "u", TempDir: "t", UID: "id"})
	if err != nil {
		t.Fatalf("AddURL through the gRPC-resolved tool: %v", err)
	}
	if want := "gid:u:t:id"; gid != want {
		t.Fatalf("AddURL gid = %q, want %q", gid, want)
	}
}
