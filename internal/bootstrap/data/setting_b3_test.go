package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestAria2SettingsSeeded pins the B3 aria2 tool config: aria2_uri lands VISIBLE
// (an admin points it at their daemon) with the localhost JSON-RPC default, and
// aria2_secret lands PRIVATE (it is a credential), both in the OFFLINE_DOWNLOAD
// group as strings.
func TestAria2SettingsSeeded(t *testing.T) {
	uri := findSeed(t, conf.Aria2Uri)
	if uri.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want OFFLINE_DOWNLOAD (%d)", conf.Aria2Uri, uri.Group, model.OFFLINE_DOWNLOAD)
	}
	if uri.Type != conf.TypeString {
		t.Errorf("%s: Type = %q, want %q", conf.Aria2Uri, uri.Type, conf.TypeString)
	}
	if uri.Value != "http://localhost:6800/jsonrpc" {
		t.Errorf("%s: default Value = %q, want http://localhost:6800/jsonrpc", conf.Aria2Uri, uri.Value)
	}
	if uri.Flag == model.PRIVATE {
		t.Errorf("%s: must be VISIBLE (not PRIVATE) so an admin can configure it", conf.Aria2Uri)
	}

	secret := findSeed(t, conf.Aria2Secret)
	if secret.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want OFFLINE_DOWNLOAD (%d)", conf.Aria2Secret, secret.Group, model.OFFLINE_DOWNLOAD)
	}
	if secret.Type != conf.TypeString {
		t.Errorf("%s: Type = %q, want %q", conf.Aria2Secret, secret.Type, conf.TypeString)
	}
	if secret.Value != "" {
		t.Errorf("%s: default Value = %q, want empty", conf.Aria2Secret, secret.Value)
	}
	if secret.Flag != model.PRIVATE {
		t.Errorf("%s: must be PRIVATE (it is a credential)", conf.Aria2Secret)
	}
}
