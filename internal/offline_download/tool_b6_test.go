package offline_download

import (
	"context"
	"errors"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
)

// recordingTool is an ext.OfflineDownloadTool double whose AddURL records which
// tool got invoked (writing its own name into *added) and then errors, so
// runDaemon returns right after resolving+calling the tool — no polling or
// transfer needed. The only thing under test is WHICH tool got AddURL'd.
type recordingTool struct {
	name  string
	added *string
}

func (f *recordingTool) Manifest() ext.Manifest                     { return ext.Manifest{ID: f.name} }
func (f *recordingTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (f *recordingTool) Shutdown(ctx context.Context) error         { return nil }
func (f *recordingTool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: f.name}
}
func (f *recordingTool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	*f.added = f.name
	return "", errors.New("stop after AddURL")
}
func (f *recordingTool) Remove(ctx context.Context, gid string) error { return nil }
func (f *recordingTool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	return ext.ToolStatus{}, nil
}

// regRecordingTool wires a recordingTool the way a real tool is wired: an
// enabled owning builtin extension entry plus RegisterTool.
func regRecordingTool(t *testing.T, name string, added *string) {
	t.Helper()
	extID := "builtin.offline_download." + name
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    extID,
		Name:  map[string]string{"en": name},
		Kinds: []ext.Kind{ext.KindOfflineDownloadTool},
	}); err != nil {
		t.Fatalf("register owning extension %q: %v", extID, err)
	}
	RegisterTool(name, extID, &recordingTool{name: name, added: added})
	t.Cleanup(func() {
		RegisterTool(name, extID, nil)
		extension.DefaultRegistry.Deregister(extID)
	})
}

// TestToolByNameResolvesAndGates pins ToolByName: SimpleHttp/none/empty resolve
// to nil (the built-in direct path), an unregistered name is nil, an enabled
// registered tool resolves to its instance, and disabling the owning extension
// gates it back to nil (re-enabling restores it) — the same enabled-gate as
// ActiveTool, applied by NAME.
func TestToolByNameResolvesAndGates(t *testing.T) {
	resetBase(t)

	if ToolByName("") != nil || ToolByName("SimpleHttp") != nil || ToolByName("none") != nil {
		t.Fatal("empty/SimpleHttp/none must resolve to nil (built-in direct path)")
	}
	if ToolByName("ghost") != nil {
		t.Fatal("an unregistered tool name must resolve to nil")
	}

	fake := regTool(t, "byname")
	if ToolByName("byname") != ext.OfflineDownloadTool(fake) {
		t.Fatal("an enabled registered tool must resolve to its instance")
	}

	if err := extension.DefaultRegistry.Disable("builtin.offline_download.byname"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if ToolByName("byname") != nil {
		t.Fatal("a disabled owning extension must gate ToolByName to nil")
	}
	if err := extension.DefaultRegistry.Enable("builtin.offline_download.byname"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if ToolByName("byname") != ext.OfflineDownloadTool(fake) {
		t.Fatal("re-enabling the owning extension must restore ToolByName resolution")
	}
}

// TestRunDaemonUsesTaskToolname proves a task runs on ITS OWN Toolname's tool,
// not the live global offline_download_tool setting: with the setting/active tool
// pointing at "othertool", a DownloadTask built for "owntool" must AddURL against
// owntool and never touch othertool.
func TestRunDaemonUsesTaskToolname(t *testing.T) {
	resetBase(t)

	var ownAdded, otherAdded string
	regRecordingTool(t, "owntool", &ownAdded)
	regRecordingTool(t, "othertool", &otherAdded)
	t.Cleanup(func() { resetBase(t) })

	// Point the GLOBAL active tool at a DIFFERENT tool than the task's Toolname.
	if err := Init("othertool"); err != nil {
		t.Fatalf("Init(othertool): %v", err)
	}
	if ActiveToolName() != "othertool" {
		t.Fatalf("precondition: active tool = %q, want othertool", ActiveToolName())
	}

	task := &DownloadTask{
		URL:      "http://example.com/x.bin",
		Toolname: "owntool",
		TempDir:  t.TempDir(),
	}
	task.SetCreator(&model.User{ID: 1, Username: "downloader"})
	task.SetCtx(context.Background())
	if err := task.Run(); err == nil {
		t.Fatal("expected the recording tool's AddURL error to surface")
	}
	if ownAdded != "owntool" {
		t.Fatalf("task must AddURL against its own Toolname; ownAdded=%q", ownAdded)
	}
	if otherAdded != "" {
		t.Fatalf("the global active tool must NOT be used; otherAdded=%q", otherAdded)
	}
}
