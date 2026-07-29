package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestTransmissionSettingsSeeded pins the B5 Transmission tool config:
// transmission_uri lands VISIBLE (an admin points it at their daemon) with the
// localhost RPC default, and transmission_seedtime lands as a visible number
// defaulting to "0", both in the OFFLINE_DOWNLOAD group.
func TestTransmissionSettingsSeeded(t *testing.T) {
	uri := findSeed(t, conf.TransmissionUri)
	if uri.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want OFFLINE_DOWNLOAD (%d)", conf.TransmissionUri, uri.Group, model.OFFLINE_DOWNLOAD)
	}
	if uri.Type != conf.TypeString {
		t.Errorf("%s: Type = %q, want %q", conf.TransmissionUri, uri.Type, conf.TypeString)
	}
	if uri.Value != "http://localhost:9091/transmission/rpc" {
		t.Errorf("%s: default Value = %q, want http://localhost:9091/transmission/rpc", conf.TransmissionUri, uri.Value)
	}
	if uri.Flag == model.PRIVATE {
		t.Errorf("%s: must be VISIBLE (not PRIVATE) so an admin can configure it", conf.TransmissionUri)
	}

	seedtime := findSeed(t, conf.TransmissionSeedtime)
	if seedtime.Group != model.OFFLINE_DOWNLOAD {
		t.Errorf("%s: Group = %d, want OFFLINE_DOWNLOAD (%d)", conf.TransmissionSeedtime, seedtime.Group, model.OFFLINE_DOWNLOAD)
	}
	if seedtime.Type != conf.TypeNumber {
		t.Errorf("%s: Type = %q, want %q", conf.TransmissionSeedtime, seedtime.Type, conf.TypeNumber)
	}
	if seedtime.Value != "0" {
		t.Errorf("%s: default Value = %q, want 0", conf.TransmissionSeedtime, seedtime.Value)
	}
}
