package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestQbittorrentSettingsSeeded pins the B4 qBittorrent tool config:
// qbittorrent_url lands PRIVATE (its default embeds admin credentials in the
// userinfo) with the localhost WebUI default, and qbittorrent_seedtime lands
// VISIBLE as a number defaulting to "0", both in the OFFLINE_DOWNLOAD group.
func TestQbittorrentSettingsSeeded(t *testing.T) {
	u := findSeed(t, conf.QbittorrentUrl)
	if u.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want OFFLINE_DOWNLOAD (%d)", conf.QbittorrentUrl, u.Group, model.OFFLINE_DOWNLOAD)
	}
	if u.Type != conf.TypeString {
		t.Errorf("%s: Type = %q, want %q", conf.QbittorrentUrl, u.Type, conf.TypeString)
	}
	if u.Value != "http://admin:adminadmin@localhost:8080/" {
		t.Errorf("%s: default Value = %q, want http://admin:adminadmin@localhost:8080/", conf.QbittorrentUrl, u.Value)
	}
	if u.Flag != model.PRIVATE {
		t.Errorf("%s: must be PRIVATE (default embeds credentials)", conf.QbittorrentUrl)
	}

	st := findSeed(t, conf.QbittorrentSeedtime)
	if st.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want OFFLINE_DOWNLOAD (%d)", conf.QbittorrentSeedtime, st.Group, model.OFFLINE_DOWNLOAD)
	}
	if st.Type != conf.TypeNumber {
		t.Errorf("%s: Type = %q, want %q", conf.QbittorrentSeedtime, st.Type, conf.TypeNumber)
	}
	if st.Value != "0" {
		t.Errorf("%s: default Value = %q, want 0", conf.QbittorrentSeedtime, st.Value)
	}
	if st.Flag == model.PRIVATE {
		t.Errorf("%s: must be VISIBLE (not PRIVATE) so an admin can configure it", conf.QbittorrentSeedtime)
	}
}
