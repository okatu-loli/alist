package handles

import (
	"context"
	"fmt"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// B6 handler tool-selection tests: AddOfflineDownload must honor a per-request /
// default tool end to end — build a SimpleHttp (Toolname-empty) task or a daemon
// (Toolname-set, TempDir under conf.TempDir/<tool>) task — while a task runs on
// its own Toolname (proven in the offline_download package tests). The manager is
// the package-shared PAUSED one, so no task ever dispatches here.

// b6FakeTool is a trivial ext.OfflineDownloadTool; the handler only needs
// ToolByName to RESOLVE it (registered + owning extension enabled) — its daemon
// methods are never called because the test manager is paused.
type b6FakeTool struct{ name string }

func (f *b6FakeTool) Manifest() ext.Manifest                     { return ext.Manifest{ID: f.name} }
func (f *b6FakeTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (f *b6FakeTool) Shutdown(ctx context.Context) error         { return nil }
func (f *b6FakeTool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: f.name}
}
func (f *b6FakeTool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	return "gid", nil
}
func (f *b6FakeTool) Remove(ctx context.Context, gid string) error { return nil }
func (f *b6FakeTool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	return ext.ToolStatus{Completed: true}, nil
}

// registerB6Tool wires a fake daemon tool (enabled owning extension + RegisterTool)
// so offline_download.ToolByName(name) resolves it. Returns the owning extension
// id so a caller can disable it to model an unavailable tool.
func registerB6Tool(t *testing.T, name string) string {
	t.Helper()
	extID := "builtin.offline_download." + name
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    extID,
		Name:  map[string]string{"en": name},
		Kinds: []ext.Kind{ext.KindOfflineDownloadTool},
	}); err != nil {
		t.Fatalf("register owning extension %q: %v", extID, err)
	}
	offline_download.RegisterTool(name, extID, &b6FakeTool{name: name})
	t.Cleanup(func() {
		offline_download.RegisterTool(name, extID, nil)
		extension.DefaultRegistry.Deregister(extID)
	})
	return extID
}

// setOfflineDownloadTool sets the offline_download_tool setting and restores it
// to SimpleHttp afterward so cases never leak the default into each other.
func setOfflineDownloadTool(t *testing.T, value string) {
	t.Helper()
	if err := op.SaveSettingItem(&model.SettingItem{
		Key: conf.OfflineDownloadTool, Value: value, Type: conf.TypeSelect, Group: model.OFFLINE_DOWNLOAD,
	}); err != nil {
		t.Fatalf("set %s=%q: %v", conf.OfflineDownloadTool, value, err)
	}
	t.Cleanup(func() {
		_ = op.SaveSettingItem(&model.SettingItem{
			Key: conf.OfflineDownloadTool, Value: "SimpleHttp", Type: conf.TypeSelect, Group: model.OFFLINE_DOWNLOAD,
		})
	})
}

// queuedTask submits a batch and returns the single task the handler queued.
func queuedTask(t *testing.T, u *model.User, path, body string) *offline_download.DownloadTask {
	t.Helper()
	w := driveJSON(u, "POST", body, AddOfflineDownload)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("expected 200, got %d: %s", got, w.Body.String())
	}
	infos := parseTaskInfos(t, w)
	if len(infos) != 1 {
		t.Fatalf("expected exactly one queued task, got %d: %s", len(infos), w.Body.String())
	}
	task, ok := offline_download.OfflineDownloadTaskManager.GetByID(infos[0].ID)
	if !ok {
		t.Fatalf("queued task %q not found in manager", infos[0].ID)
	}
	return task
}

// TestAddOfflineDownloadSimpleHttpDefault: req.Tool empty + setting=SimpleHttp
// builds the built-in direct path — a Toolname-empty task with no TempDir.
func TestAddOfflineDownloadSimpleHttpDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	setOfflineDownloadTool(t, "SimpleHttp")

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload)
	body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"]}`, mount+"/dst")
	task := queuedTask(t, u, mount+"/dst", body)

	if task.Toolname != "" {
		t.Fatalf("SimpleHttp default must leave Toolname empty, got %q", task.Toolname)
	}
	if task.TempDir != "" {
		t.Fatalf("SimpleHttp task must have no TempDir, got %q", task.TempDir)
	}
}

// TestAddOfflineDownloadExplicitTool: req.Tool="faketool" (registered+enabled)
// builds a daemon task with Toolname="faketool" and a TempDir under
// conf.TempDir/faketool.
func TestAddOfflineDownloadExplicitTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	registerB6Tool(t, "faketool")
	setOfflineDownloadTool(t, "SimpleHttp") // default is SimpleHttp; req.Tool must override

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload)
	body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"],"tool":"faketool"}`, mount+"/dst")
	task := queuedTask(t, u, mount+"/dst", body)

	if task.Toolname != "faketool" {
		t.Fatalf("explicit tool must set Toolname=faketool, got %q", task.Toolname)
	}
	wantPrefix := conf.Conf.TempDir + "/faketool/"
	if !strings.HasPrefix(task.TempDir, wantPrefix) {
		t.Fatalf("daemon TempDir = %q, want prefix %q", task.TempDir, wantPrefix)
	}
}

// TestAddOfflineDownloadSettingDefaultTool: req.Tool empty + setting=faketool
// builds a daemon task with Toolname="faketool".
func TestAddOfflineDownloadSettingDefaultTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	registerB6Tool(t, "faketool")
	setOfflineDownloadTool(t, "faketool")

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload)
	body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"]}`, mount+"/dst")
	task := queuedTask(t, u, mount+"/dst", body)

	if task.Toolname != "faketool" {
		t.Fatalf("setting default must set Toolname=faketool, got %q", task.Toolname)
	}
}

// TestAddOfflineDownloadExplicitUnavailableTool: an explicitly named tool that is
// not available (unregistered, or a disabled owning extension) is a 400 and
// queues nothing.
func TestAddOfflineDownloadExplicitUnavailableTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	setOfflineDownloadTool(t, "SimpleHttp")

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload)

	t.Run("unregistered", func(t *testing.T) {
		before := len(offline_download.OfflineDownloadTaskManager.GetAll())
		body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"],"tool":"nope"}`, mount+"/dst")
		w := driveJSON(u, "POST", body, AddOfflineDownload)
		if got := respCode(t, w); got != 400 {
			t.Fatalf("explicit unavailable tool must be 400, got %d: %s", got, w.Body.String())
		}
		if after := len(offline_download.OfflineDownloadTaskManager.GetAll()); after != before {
			t.Fatalf("a rejected add must queue nothing: %d -> %d", before, after)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		extID := registerB6Tool(t, "disabledtool")
		if err := extension.DefaultRegistry.Disable(extID); err != nil {
			t.Fatalf("disable: %v", err)
		}
		body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"],"tool":"disabledtool"}`, mount+"/dst")
		w := driveJSON(u, "POST", body, AddOfflineDownload)
		if got := respCode(t, w); got != 400 {
			t.Fatalf("explicit disabled tool must be 400, got %d: %s", got, w.Body.String())
		}
	})
}

// TestAddOfflineDownloadStaleDefaultFallsBack: req.Tool empty + a SETTING default
// naming an unavailable (disabled) tool must NOT 400 — it silently falls back to
// the SimpleHttp direct path (Toolname empty).
func TestAddOfflineDownloadStaleDefaultFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	extID := registerB6Tool(t, "staletool")
	if err := extension.DefaultRegistry.Disable(extID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	setOfflineDownloadTool(t, "staletool")

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload)
	body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"]}`, mount+"/dst")
	task := queuedTask(t, u, mount+"/dst", body)

	if task.Toolname != "" {
		t.Fatalf("stale default must fall back to SimpleHttp (Toolname empty), got %q", task.Toolname)
	}
}

// TestAddOfflineDownloadAuthzWithTool: the double gate is unchanged regardless of
// tool — a caller missing ActOfflineDownload is 403 even when naming a valid tool,
// before any task is queued.
func TestAddOfflineDownloadAuthzWithTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	registerB6Tool(t, "authztool")

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite) // write but no offline-download capability
	before := len(offline_download.OfflineDownloadTaskManager.GetAll())
	body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"],"tool":"authztool"}`, mount+"/dst")
	w := driveJSON(u, "POST", body, AddOfflineDownload)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("missing ActOfflineDownload must be 403 regardless of tool, got %d: %s", got, w.Body.String())
	}
	if after := len(offline_download.OfflineDownloadTaskManager.GetAll()); after != before {
		t.Fatalf("a denied add must queue nothing: %d -> %d", before, after)
	}
}
