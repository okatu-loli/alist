package hostcap

import "testing"

func TestGrantFromConsentPassesThroughAPIsAndNetwork(t *testing.T) {
	g := GrantFromConsent("p.id",
		[]GrantItem{{Name: CapFS}, {Name: CapNetwork}},
		[]string{"api.example.com"}, "none", "")
	if g.PluginID != "p.id" {
		t.Fatalf("plugin id = %q", g.PluginID)
	}
	if !g.HasAPI(CapFS) || !g.HasAPI(CapNetwork) {
		t.Fatalf("host_apis not passed through: %+v", g.HostAPIs)
	}
	if !g.NetworkAllows("api.example.com") {
		t.Fatal("network allowlist not passed through")
	}
	if len(g.FS) != 0 {
		t.Fatalf("fs level none should yield no scope, got %+v", g.FS)
	}
}

func TestGrantFromConsentFSLevels(t *testing.T) {
	temp := GrantFromConsent("p", nil, nil, "temp", "/scratch/p")
	if len(temp.FS) != 1 || temp.FS[0].Path != "/scratch/p" || !temp.FS[0].Write {
		t.Fatalf("temp level should confine to the scratch subtree, got %+v", temp.FS)
	}
	// temp with no scratch anchor fails closed (no stray "/").
	if g := GrantFromConsent("p", nil, nil, "temp", ""); len(g.FS) != 0 {
		t.Fatalf("temp with empty tempDir must fail closed, got %+v", g.FS)
	}
	full := GrantFromConsent("p", nil, nil, "full", "")
	if len(full.FS) != 1 || full.FS[0].Path != "/" || !full.FS[0].Write {
		t.Fatalf("full level should grant the whole tree writable, got %+v", full.FS)
	}
}
