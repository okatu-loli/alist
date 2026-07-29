package bootstrap

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/alist-org/alist/v3/internal/offline_download/aria2"
	"github.com/alist-org/alist/v3/internal/offline_download/qbittorrent"
	"github.com/alist-org/alist/v3/internal/offline_download/transmission"
)

// cleanupOfflineDownloadTools deregisters everything InitOfflineDownloadTools
// wires up and resets the base to the SimpleHttp fallback, so each test starts
// from a clean registry.
func cleanupOfflineDownloadTools() {
	extension.DefaultRegistry.Deregister(aria2.ExtensionID)
	offline_download.RegisterTool(aria2.ToolName, aria2.ExtensionID, nil)
	extension.DefaultRegistry.Deregister(qbittorrent.ExtensionID)
	offline_download.RegisterTool(qbittorrent.ToolName, qbittorrent.ExtensionID, nil)
	extension.DefaultRegistry.Deregister(transmission.ExtensionID)
	offline_download.RegisterTool(transmission.ToolName, transmission.ExtensionID, nil)
	_ = offline_download.Init("SimpleHttp")
}

// TestInitOfflineDownloadTools pins the B3 wiring, mirroring TestInitSearchBackends:
// builtin.offline_download.aria2 registers as an enabled, manageable
// Kind=OfflineDownloadTool entry AND a trusted in-process instance; the base
// resolves it; and disabling the extension gates the active tool off (offline
// download falls back to SimpleHttp) without any manager hook.
func TestInitOfflineDownloadTools(t *testing.T) {
	t.Cleanup(cleanupOfflineDownloadTools)

	InitOfflineDownloadTools()

	// (1) manageable entry: enabled, Kind OfflineDownloadTool.
	e, ok := extension.DefaultRegistry.Get(aria2.ExtensionID)
	if !ok {
		t.Fatalf("%s must be registered", aria2.ExtensionID)
	}
	if !e.Enabled() {
		t.Fatal("aria2 tool extension must be enabled by default")
	}
	if !e.HasKind(ext.KindOfflineDownloadTool) {
		t.Fatal("aria2 tool extension must carry ext.KindOfflineDownloadTool")
	}

	// (2) trusted instance resolvable by tool name (the extension id is what the
	// host registry keys on via RegisterTool).
	if _, ok := extension.GetOfflineDownloadTool(aria2.ToolName); !ok {
		t.Fatalf("GetOfflineDownloadTool(%q) must return the instance", aria2.ToolName)
	}

	// (3) base resolves it → ActiveTool non-nil.
	if err := offline_download.Init(aria2.ToolName); err != nil {
		t.Fatalf("offline_download.Init: %v", err)
	}
	if offline_download.ActiveTool() == nil {
		t.Fatal("ActiveTool must be non-nil while the aria2 extension is enabled")
	}

	// (4) enabled-gate: disabling the OWNING extension makes ActiveTool nil
	// (offline download falls back to SimpleHttp), with no manager callback.
	if err := extension.DefaultRegistry.Disable(aria2.ExtensionID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if offline_download.ActiveTool() != nil {
		t.Fatal("ActiveTool must be nil when the aria2 extension is disabled (gated)")
	}

	// (5) re-enabling restores it.
	if err := extension.DefaultRegistry.Enable(aria2.ExtensionID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if offline_download.ActiveTool() == nil {
		t.Fatal("ActiveTool must be restored when the aria2 extension is re-enabled")
	}
}

// TestInitOfflineDownloadToolsQbittorrent pins the B4 wiring ALONGSIDE aria2:
// builtin.offline_download.qbittorrent registers as an enabled, manageable
// Kind=OfflineDownloadTool entry AND a trusted in-process instance; the base
// resolves it by tool name; and disabling the extension gates the active tool off
// (offline download falls back to SimpleHttp) without any manager hook.
func TestInitOfflineDownloadToolsQbittorrent(t *testing.T) {
	t.Cleanup(cleanupOfflineDownloadTools)

	InitOfflineDownloadTools()

	// (1) manageable entry: enabled, Kind OfflineDownloadTool.
	e, ok := extension.DefaultRegistry.Get(qbittorrent.ExtensionID)
	if !ok {
		t.Fatalf("%s must be registered", qbittorrent.ExtensionID)
	}
	if !e.Enabled() {
		t.Fatal("qbittorrent tool extension must be enabled by default")
	}
	if !e.HasKind(ext.KindOfflineDownloadTool) {
		t.Fatal("qbittorrent tool extension must carry ext.KindOfflineDownloadTool")
	}

	// aria2 must still be registered alongside it (B4 does not touch the aria2 block).
	if _, ok := extension.DefaultRegistry.Get(aria2.ExtensionID); !ok {
		t.Fatalf("%s must remain registered alongside qbittorrent", aria2.ExtensionID)
	}

	// (2) trusted instance resolvable by tool name.
	if _, ok := extension.GetOfflineDownloadTool(qbittorrent.ToolName); !ok {
		t.Fatalf("GetOfflineDownloadTool(%q) must return the instance", qbittorrent.ToolName)
	}

	// (3) base resolves it → ActiveTool non-nil.
	if err := offline_download.Init(qbittorrent.ToolName); err != nil {
		t.Fatalf("offline_download.Init: %v", err)
	}
	if offline_download.ActiveTool() == nil {
		t.Fatal("ActiveTool must be non-nil while the qbittorrent extension is enabled")
	}

	// (4) enabled-gate: disabling the OWNING extension makes ActiveTool nil.
	if err := extension.DefaultRegistry.Disable(qbittorrent.ExtensionID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if offline_download.ActiveTool() != nil {
		t.Fatal("ActiveTool must be nil when the qbittorrent extension is disabled (gated)")
	}

	// (5) re-enabling restores it.
	if err := extension.DefaultRegistry.Enable(qbittorrent.ExtensionID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if offline_download.ActiveTool() == nil {
		t.Fatal("ActiveTool must be restored when the qbittorrent extension is re-enabled")
	}
}

// TestInitOfflineDownloadToolsTransmission pins the B5 wiring ALONGSIDE aria2 +
// qBittorrent: builtin.offline_download.transmission registers as an enabled,
// manageable Kind=OfflineDownloadTool entry AND a trusted in-process instance; the
// base resolves it by tool name; and disabling the extension gates the active tool
// off (offline download falls back to SimpleHttp) without any manager hook.
func TestInitOfflineDownloadToolsTransmission(t *testing.T) {
	t.Cleanup(cleanupOfflineDownloadTools)

	InitOfflineDownloadTools()

	// (1) manageable entry: enabled, Kind OfflineDownloadTool.
	e, ok := extension.DefaultRegistry.Get(transmission.ExtensionID)
	if !ok {
		t.Fatalf("%s must be registered", transmission.ExtensionID)
	}
	if !e.Enabled() {
		t.Fatal("transmission tool extension must be enabled by default")
	}
	if !e.HasKind(ext.KindOfflineDownloadTool) {
		t.Fatal("transmission tool extension must carry ext.KindOfflineDownloadTool")
	}

	// aria2 + qBittorrent must still be registered alongside it (B5 does not touch
	// their blocks).
	if _, ok := extension.DefaultRegistry.Get(aria2.ExtensionID); !ok {
		t.Fatalf("%s must remain registered alongside transmission", aria2.ExtensionID)
	}
	if _, ok := extension.DefaultRegistry.Get(qbittorrent.ExtensionID); !ok {
		t.Fatalf("%s must remain registered alongside transmission", qbittorrent.ExtensionID)
	}

	// (2) trusted instance resolvable by tool name.
	if _, ok := extension.GetOfflineDownloadTool(transmission.ToolName); !ok {
		t.Fatalf("GetOfflineDownloadTool(%q) must return the instance", transmission.ToolName)
	}

	// (3) base resolves it → ActiveTool non-nil.
	if err := offline_download.Init(transmission.ToolName); err != nil {
		t.Fatalf("offline_download.Init: %v", err)
	}
	if offline_download.ActiveTool() == nil {
		t.Fatal("ActiveTool must be non-nil while the transmission extension is enabled")
	}

	// (4) enabled-gate: disabling the OWNING extension makes ActiveTool nil.
	if err := extension.DefaultRegistry.Disable(transmission.ExtensionID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if offline_download.ActiveTool() != nil {
		t.Fatal("ActiveTool must be nil when the transmission extension is disabled (gated)")
	}

	// (5) re-enabling restores it.
	if err := extension.DefaultRegistry.Enable(transmission.ExtensionID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if offline_download.ActiveTool() == nil {
		t.Fatal("ActiveTool must be restored when the transmission extension is re-enabled")
	}
}
