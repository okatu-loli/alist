package bootstrap

import (
	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/alist-org/alist/v3/internal/offline_download/aria2"
	"github.com/alist-org/alist/v3/internal/offline_download/qbittorrent"
	"github.com/alist-org/alist/v3/internal/offline_download/transmission"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// InitOfflineDownloadTools registers the concrete OfflineDownloadTool backends as
// independent, manageable built-in extensions (子项目 B, wave B3+), mirroring
// InitSearchBackends exactly. Each tool is TWO registrations that share an id:
//
//   - RegisterBuiltinExtension inserts a Kind=OfflineDownloadTool registry entry
//     so the tool lists in the extension manager and toggles through
//     EnableBuiltin/DisableBuiltin with persistence — the enable/disable boundary
//     the offline_download base's ActiveTool gate reads.
//   - offline_download.RegisterTool records the toolName→extensionID mapping AND
//     registers the trusted in-process instance with the host registry, so the
//     base can resolve it by the offline_download_tool setting AND gate it on the
//     owning extension's enabled state.
//
// B3 ships builtin.offline_download.aria2; B4/B5 add qBittorrent / transmission
// ALONGSIDE it here — each a separate manageable extension the admin can toggle
// independently, with the offline_download_tool setting picking which is live.
// It must run BEFORE InitBuiltinManage so ApplyDisabledBuiltins re-applies an
// admin's persisted "off" choice to these entries too. A registration failure is
// fatal: a half-wired tool must not run.
func InitOfflineDownloadTools() {
	// Each tool contributes ONLY a config_section (design C): the base feature's
	// config page appends it after its own "general" section — the tool's own
	// settings, keyed by Parent=builtin.offline_download and ordered after the
	// base's general section (Order 0). The tool no longer declares a standalone
	// SettingsPages (C3): its config is a section on the parent feature page, not
	// a separate entry the extension manager links to; the tool detail's
	// parent-jump sends the admin to the base feature's config page instead.
	toolContributions := func(section ext.ConfigSection) ext.Contributions {
		return ext.Contributions{
			ConfigSections: []ext.ConfigSection{section},
		}
	}

	aria2Manifest := ext.Manifest{
		ID:         aria2.ExtensionID,
		Name:       map[string]string{"en": "Offline: aria2", "zh": "离线下载:aria2"},
		Kinds:      []ext.Kind{ext.KindOfflineDownloadTool},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this removes aria2 as an offline-download tool; if aria2 is the selected tool, offline downloads fall back to the built-in direct-download path (SimpleHttp) until it is re-enabled or another tool is selected.",
			"zh": "关闭后 aria2 离线下载工具将不可用;若当前选用 aria2,离线下载将回落到内置直连下载(SimpleHttp),直到重新启用或选择其他工具。",
		},
		Contributions: toolContributions(ext.ConfigSection{
			Parent:      "builtin.offline_download",
			Title:       map[string]string{"en": "aria2", "zh": "aria2"},
			SettingKeys: []string{"aria2_uri", "aria2_secret"},
			Order:       1,
		}),
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, aria2Manifest); err != nil {
		utils.Log.Fatalf("failed to register offline download tool %q: %+v", aria2Manifest.ID, err)
	}
	offline_download.RegisterTool(aria2.ToolName, aria2.ExtensionID, &aria2.Tool{})

	qbitManifest := ext.Manifest{
		ID:         qbittorrent.ExtensionID,
		Name:       map[string]string{"en": "Offline: qBittorrent", "zh": "离线下载:qBittorrent"},
		Kinds:      []ext.Kind{ext.KindOfflineDownloadTool},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this removes qBittorrent as an offline-download tool; if qBittorrent is the selected tool, offline downloads fall back to the built-in direct-download path (SimpleHttp) until it is re-enabled or another tool is selected.",
			"zh": "关闭后 qBittorrent 离线下载工具将不可用;若当前选用 qBittorrent,离线下载将回落到内置直连下载(SimpleHttp),直到重新启用或选择其他工具。",
		},
		Contributions: toolContributions(ext.ConfigSection{
			Parent:      "builtin.offline_download",
			Title:       map[string]string{"en": "qBittorrent", "zh": "qBittorrent"},
			SettingKeys: []string{"qbittorrent_url", "qbittorrent_seedtime"},
			Order:       2,
		}),
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, qbitManifest); err != nil {
		utils.Log.Fatalf("failed to register offline download tool %q: %+v", qbitManifest.ID, err)
	}
	offline_download.RegisterTool(qbittorrent.ToolName, qbittorrent.ExtensionID, &qbittorrent.Tool{})

	transmissionManifest := ext.Manifest{
		ID:         transmission.ExtensionID,
		Name:       map[string]string{"en": "Offline: Transmission", "zh": "离线下载:Transmission"},
		Kinds:      []ext.Kind{ext.KindOfflineDownloadTool},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this removes Transmission as an offline-download tool; if Transmission is the selected tool, offline downloads fall back to the built-in direct-download path (SimpleHttp) until it is re-enabled or another tool is selected.",
			"zh": "关闭后 Transmission 离线下载工具将不可用;若当前选用 Transmission,离线下载将回落到内置直连下载(SimpleHttp),直到重新启用或选择其他工具。",
		},
		Contributions: toolContributions(ext.ConfigSection{
			Parent:      "builtin.offline_download",
			Title:       map[string]string{"en": "Transmission", "zh": "Transmission"},
			SettingKeys: []string{"transmission_uri", "transmission_seedtime"},
			Order:       3,
		}),
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, transmissionManifest); err != nil {
		utils.Log.Fatalf("failed to register offline download tool %q: %+v", transmissionManifest.ID, err)
	}
	offline_download.RegisterTool(transmission.ToolName, transmission.ExtensionID, &transmission.Tool{})
}
