package grpc

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/extension/hostcap"
)

// This is the W6a reverse-bridge closer: a real plugin subprocess drives
// Host.SaveConfig and Host.Capabilities over the wire — the two draft Host
// methods host-api.md flagged as not yet on the interface — mirroring
// TestReverseBridgeAuthLogin et al. in hostcap_authsign_integration_test.go,
// whose harness (launchProbe/serveHost) this file reuses.

// TestReverseBridgeSaveConfig: a plugin's SaveConfig call over the bridge
// reaches a fake ConfigSaver wired into the Host, carrying exactly the bytes
// the plugin sent; a Host with no ConfigSaver wired refuses the write instead
// of silently dropping it.
func TestReverseBridgeSaveConfig(t *testing.T) {
	probe := launchProbe(t)
	ctx := context.Background()

	var saved []byte
	h := hostcap.New(hostcap.Grant{PluginID: "it.saveconfig"}, hostcap.Options{
		ConfigSaver: func(cfg []byte) error {
			saved = append([]byte(nil), cfg...)
			return nil
		},
	})
	id := serveHost(probe, h)

	got, err := probe.Exercise(ctx, id, "config.save", `{"k":"v"}`, "")
	if err != nil {
		t.Fatalf("config.save over the bridge: %v", err)
	}
	if got != "ok" {
		t.Fatalf("config.save reply = %q, want %q", got, "ok")
	}
	if string(saved) != `{"k":"v"}` {
		t.Fatalf("ConfigSaver received %s, want %s", saved, `{"k":"v"}`)
	}

	// No ConfigSaver wired: the write is refused, not silently dropped.
	nosaver := hostcap.New(hostcap.Grant{PluginID: "it.saveconfig.nosaver"}, hostcap.Options{})
	nid := serveHost(probe, nosaver)
	if _, err := probe.Exercise(ctx, nid, "config.save", `{"k":"v"}`, ""); err == nil {
		t.Fatal("config.save without a ConfigSaver: expected an error, got nil")
	}
}

// TestReverseBridgeCapabilities: a plugin reads its own granted Host-API names
// over the bridge; an empty grant yields an empty list.
func TestReverseBridgeCapabilities(t *testing.T) {
	probe := launchProbe(t)
	ctx := context.Background()

	h := hostcap.New(hostcap.Grant{
		PluginID: "it.capabilities",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}, {Name: hostcap.CapNetwork}},
	}, hostcap.Options{})
	id := serveHost(probe, h)

	got, err := probe.Exercise(ctx, id, "capabilities", "", "")
	if err != nil {
		t.Fatalf("capabilities over the bridge: %v", err)
	}
	want := "network,store"
	if got != want {
		t.Fatalf("capabilities = %q, want %q", got, want)
	}

	// Empty grant → empty capability list.
	empty := hostcap.New(hostcap.Grant{PluginID: "it.capabilities.empty"}, hostcap.Options{})
	eid := serveHost(probe, empty)
	got, err = probe.Exercise(ctx, eid, "capabilities", "", "")
	if err != nil {
		t.Fatalf("capabilities (empty grant) over the bridge: %v", err)
	}
	if got != "" {
		t.Fatalf("capabilities (empty grant) = %q, want empty", got)
	}
}
