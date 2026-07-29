package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestOfflineDownloadToolSettingSeeded pins the B1 tool-selection setting:
// offline_download_tool lands VISIBLE in the OFFLINE_DOWNLOAD group as a Select
// over SimpleHttp + the three daemon tools, defaulting to SimpleHttp.
func TestOfflineDownloadToolSettingSeeded(t *testing.T) {
	tool := findSeed(t, conf.OfflineDownloadTool)
	if tool.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want model.OFFLINE_DOWNLOAD (%d)", conf.OfflineDownloadTool, tool.Group, model.OFFLINE_DOWNLOAD)
	}
	if tool.Type != conf.TypeSelect {
		t.Errorf("%s: Type = %q, want %q", conf.OfflineDownloadTool, tool.Type, conf.TypeSelect)
	}
	if tool.Value != "SimpleHttp" {
		t.Errorf("%s: default Value = %q, want %q", conf.OfflineDownloadTool, tool.Value, "SimpleHttp")
	}
	if tool.Options != "SimpleHttp,aria2,qBittorrent,Transmission" {
		t.Errorf("%s: Options = %q", conf.OfflineDownloadTool, tool.Options)
	}
	if tool.Flag == model.PRIVATE {
		t.Errorf("%s: must be VISIBLE (not PRIVATE)", conf.OfflineDownloadTool)
	}
}
