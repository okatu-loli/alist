package offline_download

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// setSizeCapSetting seeds offline_download_max_size_gb=val and removes it on
// cleanup so the effective cap reflects the admin-tunable setting.
func setSizeCapSetting(t *testing.T, val string) {
	t.Helper()
	if err := op.SaveSettingItem(&model.SettingItem{
		Key:   conf.OfflineDownloadMaxSizeGB,
		Value: val,
		Type:  conf.TypeNumber,
		Group: model.OFFLINE_DOWNLOAD,
	}); err != nil {
		t.Fatalf("seed %s=%q: %v", conf.OfflineDownloadMaxSizeGB, val, err)
	}
	t.Cleanup(func() { _ = op.DeleteSettingItemByKey(conf.OfflineDownloadMaxSizeGB) })
}

// TestMaxOfflineDownloadBytesReadsSetting pins the W6 wiring: the effective
// SimpleHttp size backstop is read from offline_download_max_size_gb (in GiB) at
// run time, with a non-positive/blank value falling back to the built-in 100 GiB
// default. This is the value newBoundedReader is fed for every SimpleHttp task.
func TestMaxOfflineDownloadBytesReadsSetting(t *testing.T) {
	// Unset → default 100 GiB backstop.
	if got := maxOfflineDownloadBytes(); got != defaultMaxOfflineDownloadBytes {
		t.Fatalf("unset: cap = %d, want default %d", got, defaultMaxOfflineDownloadBytes)
	}

	// A positive value tightens/loosens the cap: value<<30 GiB.
	setSizeCapSetting(t, "5")
	if got, want := maxOfflineDownloadBytes(), int64(5)<<30; got != want {
		t.Fatalf("cap for 5 GiB = %d, want %d", got, want)
	}

	// Zero falls back to the default (never an unbounded/zero cap).
	setSizeCapSetting(t, "0")
	if got := maxOfflineDownloadBytes(); got != defaultMaxOfflineDownloadBytes {
		t.Fatalf("zero: cap = %d, want default %d", got, defaultMaxOfflineDownloadBytes)
	}

	// Negative also falls back to the default.
	setSizeCapSetting(t, "-1")
	if got := maxOfflineDownloadBytes(); got != defaultMaxOfflineDownloadBytes {
		t.Fatalf("negative: cap = %d, want default %d", got, defaultMaxOfflineDownloadBytes)
	}
}
