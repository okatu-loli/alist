package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// findSeed returns the seeded SettingItem for key from InitialSettings(), or
// fails the test if it is absent. It stubs conf.Conf the same way the
// third-party-domain test does, since InitialSettings() reads
// conf.Conf.Tasks.*.Workers and this package has no bootstrap TestMain.
func findSeed(t *testing.T, key string) model.SettingItem {
	t.Helper()
	oldConf := conf.Conf
	conf.Conf = &conf.Config{}
	t.Cleanup(func() { conf.Conf = oldConf })

	for _, item := range InitialSettings() {
		if item.Key == key {
			return item
		}
	}
	t.Fatalf("setting %q must be seeded by InitialSettings()", key)
	return model.SettingItem{}
}

// TestSearchSettingsSeeded pins the W6 search config: search_max_depth and
// search_result_limit are seeded VISIBLE (not PRIVATE) TypeNumber settings in
// the INDEX display group with defaults 20 / 1000, so the generic settings form
// renders them and the search handler can read real, admin-tunable values.
func TestSearchSettingsSeeded(t *testing.T) {
	cases := []struct {
		key     string
		wantDef string
	}{
		{conf.SearchMaxDepth, "20"},
		{conf.SearchResultLimit, "1000"},
	}
	for _, tc := range cases {
		it := findSeed(t, tc.key)
		if it.Group != model.INDEX {
			t.Errorf("%s: Group = %d, want model.INDEX (%d)", tc.key, it.Group, model.INDEX)
		}
		if it.Type != conf.TypeNumber {
			t.Errorf("%s: Type = %q, want %q", tc.key, it.Type, conf.TypeNumber)
		}
		if it.Flag == model.PRIVATE {
			t.Errorf("%s: must be VISIBLE (not PRIVATE) so it shows in the admin form", tc.key)
		}
		if it.Value != tc.wantDef {
			t.Errorf("%s: default Value = %q, want %q", tc.key, it.Value, tc.wantDef)
		}
	}
}

// TestOfflineDownloadSettingsSeeded pins the W6 offline_download config:
// offline_download_task_threads_num moves into the OFFLINE_DOWNLOAD group and
// loses its PRIVATE flag (it is read by KEY, so the display move is safe), and
// offline_download_max_size_gb is seeded VISIBLE TypeNumber in OFFLINE_DOWNLOAD
// with default 100.
func TestOfflineDownloadSettingsSeeded(t *testing.T) {
	threads := findSeed(t, conf.TaskOfflineDownloadThreadsNum)
	if threads.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want model.OFFLINE_DOWNLOAD (%d)", conf.TaskOfflineDownloadThreadsNum, threads.Group, model.OFFLINE_DOWNLOAD)
	}
	if threads.Flag == model.PRIVATE {
		t.Errorf("%s: must no longer be PRIVATE so it shows in the admin form", conf.TaskOfflineDownloadThreadsNum)
	}
	if threads.Type != conf.TypeNumber {
		t.Errorf("%s: Type = %q, want %q", conf.TaskOfflineDownloadThreadsNum, threads.Type, conf.TypeNumber)
	}

	size := findSeed(t, conf.OfflineDownloadMaxSizeGB)
	if size.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want model.OFFLINE_DOWNLOAD (%d)", conf.OfflineDownloadMaxSizeGB, size.Group, model.OFFLINE_DOWNLOAD)
	}
	if size.Type != conf.TypeNumber {
		t.Errorf("%s: Type = %q, want %q", conf.OfflineDownloadMaxSizeGB, size.Type, conf.TypeNumber)
	}
	if size.Flag == model.PRIVATE {
		t.Errorf("%s: must be VISIBLE (not PRIVATE)", conf.OfflineDownloadMaxSizeGB)
	}
	if size.Value != "100" {
		t.Errorf("%s: default Value = %q, want %q", conf.OfflineDownloadMaxSizeGB, size.Value, "100")
	}
}

// TestOfflineDownloadTransferThreadsSeeded pins the B2 transfer-engine config:
// offline_download_transfer_threads_num is seeded VISIBLE TypeNumber in the
// OFFLINE_DOWNLOAD group with default 5, so the admin form can tune the transfer
// manager's worker count independently of the download worker count.
func TestOfflineDownloadTransferThreadsSeeded(t *testing.T) {
	it := findSeed(t, conf.OfflineDownloadTransferThreadsNum)
	if it.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want model.OFFLINE_DOWNLOAD (%d)", conf.OfflineDownloadTransferThreadsNum, it.Group, model.OFFLINE_DOWNLOAD)
	}
	if it.Type != conf.TypeNumber {
		t.Errorf("%s: Type = %q, want %q", conf.OfflineDownloadTransferThreadsNum, it.Type, conf.TypeNumber)
	}
	if it.Flag == model.PRIVATE {
		t.Errorf("%s: must be VISIBLE (not PRIVATE) so it shows in the admin form", conf.OfflineDownloadTransferThreadsNum)
	}
	if it.Value != "5" {
		t.Errorf("%s: default Value = %q, want %q", conf.OfflineDownloadTransferThreadsNum, it.Value, "5")
	}
}
