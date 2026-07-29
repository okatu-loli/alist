package offline_download

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// baseFakeTool is an ext.OfflineDownloadTool double for the base resolver tests.
// Only Config().Name is load-bearing; the daemon methods are no-ops.
type baseFakeTool struct {
	name string
}

func (f *baseFakeTool) Manifest() ext.Manifest                     { return ext.Manifest{ID: f.name} }
func (f *baseFakeTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (f *baseFakeTool) Shutdown(ctx context.Context) error         { return nil }
func (f *baseFakeTool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: f.name}
}
func (f *baseFakeTool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	return "gid-" + f.name, nil
}
func (f *baseFakeTool) Remove(ctx context.Context, gid string) error { return nil }
func (f *baseFakeTool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	return ext.ToolStatus{Completed: true}, nil
}

// resetBase clears any active tool so each test starts from the SimpleHttp
// fallback.
func resetBase(t *testing.T) {
	t.Helper()
	if err := Init("SimpleHttp"); err != nil {
		t.Fatalf("Init(SimpleHttp): %v", err)
	}
}

// regTool registers a fake tool the way a real one is wired (B3+): an enabled,
// owning builtin extension entry in DefaultRegistry PLUS the toolName→extensionID
// mapping and trusted instance via RegisterTool, so ActiveTool's enabled-gate
// resolves it as active. Cleanup deregisters both.
func regTool(t *testing.T, name string) *baseFakeTool {
	t.Helper()
	fake := &baseFakeTool{name: name}
	extID := "builtin.offline_download." + name
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    extID,
		Name:  map[string]string{"en": name},
		Kinds: []ext.Kind{ext.KindOfflineDownloadTool},
	}); err != nil {
		t.Fatalf("register owning extension %q: %v", extID, err)
	}
	RegisterTool(name, extID, fake)
	t.Cleanup(func() {
		RegisterTool(name, extID, nil)
		extension.DefaultRegistry.Deregister(extID)
	})
	return fake
}

func TestBaseInitSimpleHttpNoneAndRegistryResolve(t *testing.T) {
	resetBase(t)
	if ActiveTool() != nil {
		t.Fatal("Init(SimpleHttp) must leave no active tool")
	}
	if err := Init(""); err != nil {
		t.Fatalf(`Init(""): %v`, err)
	}
	if ActiveTool() != nil {
		t.Fatal(`Init("") must leave no active tool`)
	}
	if err := Init("none"); err != nil {
		t.Fatalf("Init(none): %v", err)
	}
	if ActiveTool() != nil {
		t.Fatal("Init(none) must leave no active tool")
	}

	// A selected-but-unregistered tool is not an error: base stays on the
	// SimpleHttp fallback (nil active).
	if err := Init("ghost"); err != nil {
		t.Fatalf("Init(unregistered) returned error: %v", err)
	}
	if ActiveTool() != nil {
		t.Fatal("unregistered tool must not become active")
	}

	fake := regTool(t, "faketool")
	t.Cleanup(func() { resetBase(t) })

	if err := Init("faketool"); err != nil {
		t.Fatalf("Init(faketool): %v", err)
	}
	if ActiveTool() != ext.OfflineDownloadTool(fake) {
		t.Fatal("Init(faketool) must adopt the registered instance")
	}
	if ActiveToolName() != "faketool" {
		t.Fatalf("ActiveToolName() = %q, want faketool", ActiveToolName())
	}
}

func TestBaseInitSwitchReleasesOld(t *testing.T) {
	resetBase(t)
	_ = regTool(t, "aaa")
	b := regTool(t, "bbb")
	t.Cleanup(func() { resetBase(t) })

	if err := Init("aaa"); err != nil {
		t.Fatalf("Init(aaa): %v", err)
	}
	if ActiveTool().Config().Name != "aaa" {
		t.Fatalf("active = %q, want aaa", ActiveTool().Config().Name)
	}
	if err := Init("bbb"); err != nil {
		t.Fatalf("Init(bbb): %v", err)
	}
	if ActiveTool() != ext.OfflineDownloadTool(b) {
		t.Fatal("switching tools must release the old and adopt the new")
	}

	// Switching to SimpleHttp releases entirely.
	if err := Init("SimpleHttp"); err != nil {
		t.Fatalf("Init(SimpleHttp): %v", err)
	}
	if ActiveTool() != nil {
		t.Fatal("Init(SimpleHttp) must release the active tool")
	}
}

// TestActiveToolEnabledGate proves the B1 enabled-gate: a resolved tool is
// active only while its OWNING extension is enabled. Disabling that extension
// makes ActiveTool fall back to nil at resolution time, with no manager callback;
// re-enabling restores it.
func TestActiveToolEnabledGate(t *testing.T) {
	resetBase(t)
	fake := regTool(t, "gated")
	t.Cleanup(func() { resetBase(t) })

	if err := Init("gated"); err != nil {
		t.Fatalf("Init(gated): %v", err)
	}
	if ActiveTool() != ext.OfflineDownloadTool(fake) {
		t.Fatal("tool must be active while its extension is enabled")
	}

	// Disable the owning extension: gate closes.
	if err := extension.DefaultRegistry.Disable("builtin.offline_download.gated"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if ActiveTool() != nil {
		t.Fatal("disabled owning extension must gate ActiveTool to nil")
	}
	if ActiveToolName() != "" {
		t.Fatalf("gated ActiveToolName() = %q, want empty", ActiveToolName())
	}

	// Re-enable: gate opens.
	if err := extension.DefaultRegistry.Enable("builtin.offline_download.gated"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if ActiveTool() != ext.OfflineDownloadTool(fake) {
		t.Fatal("re-enabling the owning extension must restore the active tool")
	}
}

// TestSettingHookReinits proves the offline_download_tool setting hook is wired:
// firing the registered hook with a value re-inits the base to resolve that tool.
func TestSettingHookReinits(t *testing.T) {
	resetBase(t)
	fake := regTool(t, "hooked")
	t.Cleanup(func() { resetBase(t) })

	hasHook, err := op.HandleSettingItemHook(&model.SettingItem{Key: conf.OfflineDownloadTool, Value: "hooked"})
	if err != nil {
		t.Fatalf("HandleSettingItemHook: %v", err)
	}
	if !hasHook {
		t.Fatalf("no hook registered for %q", conf.OfflineDownloadTool)
	}
	if ActiveTool() != ext.OfflineDownloadTool(fake) {
		t.Fatal("firing the offline_download_tool hook must re-init to the selected tool")
	}

	// And flipping back to SimpleHttp releases it through the same hook.
	if _, err := op.HandleSettingItemHook(&model.SettingItem{Key: conf.OfflineDownloadTool, Value: "SimpleHttp"}); err != nil {
		t.Fatalf("HandleSettingItemHook(SimpleHttp): %v", err)
	}
	if ActiveTool() != nil {
		t.Fatal("offline_download_tool=SimpleHttp via hook must release the tool")
	}
}
