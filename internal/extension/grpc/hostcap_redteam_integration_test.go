package grpc

// Red-team integration test: proves the host.FS().Mkdir path-traversal escape
// (see internal/extension/hostcap/hostcap_redteam_test.go, VULN 1) is reachable
// by a REAL plugin subprocess through the plugin->host reverse gRPC bridge, not
// just in-process. hostserver.FSMkdir forwards req.Name verbatim to
// host.FS().Mkdir, so the untrusted name crosses the wire unchanged.
//
// It also documents the reverse-bridge trust root: a plugin can only reach the
// ext.Host served on a broker id it can Dial, and each id is bound host-side to
// exactly one plugin's Grant (serveHost wires one Host per id). The forward-
// looking invariant to preserve when production wiring lands is one Host per
// subprocess broker — see host-f0-redteam.md.

import (
	"context"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/extension/hostcap"
)

// TestRedteamReverseBridgeMkdirTraversal: an attacker plugin granted write on
// /rtw-granted uses "../" in the Mkdir name to create a directory inside the
// separate, ungranted /rtw-secret mount — over the real bridge.
func TestRedteamReverseBridgeMkdirTraversal(t *testing.T) {
	mountHostcapLocal(t, "/rtw-granted")
	mountHostcapLocal(t, "/rtw-secret")
	probe := launchProbe(t)
	ctx := context.Background()

	attacker := hostcap.New(hostcap.Grant{
		PluginID: "it.attacker",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: "/rtw-granted", Write: true}},
	}, hostcap.Options{})
	aid := serveHost(probe, attacker)

	// Precondition: the attacker cannot mkdir directly in the ungranted mount.
	if _, err := probe.Exercise(ctx, aid, "fs.mkdir", "/rtw-secret", "direct"); err == nil {
		t.Fatal("precondition: direct mkdir into ungranted mount should be denied")
	}

	// Attack: climb out of the granted mount via the name argument. FIXED — the
	// host-side validateSingleSegment refuses the traversing name even over the
	// wire, so nothing is created.
	if _, err := probe.Exercise(ctx, aid, "fs.mkdir", "/rtw-granted", "../rtw-secret/pwned"); err == nil {
		t.Fatal("mkdir traversal over the bridge should have been refused, got no error")
	}

	// Verify with a DIFFERENT host that IS granted read on the secret mount.
	verifier := hostcap.New(hostcap.Grant{
		PluginID: "it.verifier",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: "/rtw-secret"}},
	}, hostcap.Options{})
	vid := serveHost(probe, verifier)

	listing, err := probe.Exercise(ctx, vid, "fs.list", "/rtw-secret", "")
	if err != nil {
		t.Fatalf("verify list: %v", err)
	}
	if strings.Contains(listing, "pwned") {
		t.Fatalf("REGRESSION over the reverse bridge: attacker created 'pwned' inside the ungranted /rtw-secret mount; listing=%q", listing)
	}
	// Invariant holds — the ungranted mount is untouched.
	if listing != "seed.txt,sub" {
		t.Fatalf("unexpected /rtw-secret listing after attack: %q", listing)
	}
}
