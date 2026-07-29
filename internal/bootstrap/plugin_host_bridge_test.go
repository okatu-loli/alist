package bootstrap

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
)

// testManifest is a minimal, valid StorageDriver manifest for building a signed
// test package (mirrors internal/extension's own validManifest, restated here
// since that helper is unexported test scaffolding in another package).
func testManifest(id, version string, p ext.Permissions) ext.Manifest {
	return ext.Manifest{
		ID:      id,
		Name:    map[string]string{"en": "Demo"},
		Version: version,
		CoreAPI: "*",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{
			Source:    "official",
			License:   "MIT",
			SourceURL: "https://github.com/example/demo",
			Pricing:   "free",
		},
		Permissions: p,
	}
}

// buildSignedTestPackage writes a manifest + dummy binary into a fresh dir,
// signs it, and packs it into a .tar.gz the extension.Manager can Install.
func buildSignedTestPackage(t *testing.T, m ext.Manifest, priv ed25519.PrivateKey) string {
	t.Helper()
	dir := t.TempDir()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, extension.ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("#!/bin/false\ndummy-binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extension.SignDir(dir, priv); err != nil {
		t.Fatalf("SignDir: %v", err)
	}

	out := filepath.Join(t.TempDir(), "pkg.tar.gz")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestToHostcapGrant(t *testing.T) {
	// nil grant is safe: PluginID set, everything else empty.
	hg := toHostcapGrant("p1", nil)
	if hg.PluginID != "p1" || len(hg.HostAPIs) != 0 || len(hg.Network) != 0 || len(hg.FS) != 0 {
		t.Fatalf("toHostcapGrant(nil) = %+v, want empty", hg)
	}

	g := &extension.Grant{
		HostAPIs: []extension.GrantItem{{Name: "network"}, {Name: "host.fs", Scope: "read"}},
		Network:  []string{"api.example.com"},
	}
	hg = toHostcapGrant("p2", g)
	if hg.PluginID != "p2" {
		t.Fatalf("PluginID = %q", hg.PluginID)
	}
	if len(hg.HostAPIs) != 2 || hg.HostAPIs[0].Name != "network" || hg.HostAPIs[1].Name != "host.fs" || hg.HostAPIs[1].Scope != "read" {
		t.Fatalf("HostAPIs = %+v", hg.HostAPIs)
	}
	if len(hg.Network) != 1 || hg.Network[0] != "api.example.com" {
		t.Fatalf("Network = %v", hg.Network)
	}
	if len(hg.FS) != 0 {
		t.Fatalf("FS = %v, want empty (a StorageDriver borrows no fs subtree by default)", hg.FS)
	}
}

func TestHostBuilderFor(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := extension.NewManager(filepath.Join(t.TempDir(), "plugins"), extension.NewRegistry(), extension.NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}

	m := testManifest("com.example.hostbridge", "1.0.0", ext.Permissions{
		Network:  []string{"api.example.com"},
		HostAPIs: []string{"network"},
	})
	pkg := buildSignedTestPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.SetConfig(m.ID, json.RawMessage(`{"greeting":"hi"}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := mgr.SetGrant(m.ID, &extension.Grant{
		HostAPIs: []extension.GrantItem{{Name: "network"}},
		Network:  []string{"api.example.com"},
	}, false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}

	builder := hostBuilderFor(mgr)
	h := builder(m.ID)
	if h == nil {
		t.Fatal("hostBuilderFor(...)(id) = nil, want a host")
	}

	cfg, err := h.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if string(cfg) != `{"greeting":"hi"}` {
		t.Fatalf("LoadConfig = %s, want the saved config", cfg)
	}

	// SaveConfig mirrors LoadConfig in the write direction: it round-trips
	// through the SAME manager (m.SetConfig/m.GetConfig), not a side channel.
	if err := h.SaveConfig(context.Background(), []byte(`{"greeting":"bye"}`)); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	cfg, err = h.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig after SaveConfig: %v", err)
	}
	if string(cfg) != `{"greeting":"bye"}` {
		t.Fatalf("LoadConfig after SaveConfig = %s, want the newly saved config", cfg)
	}
	persisted, err := mgr.GetConfig(m.ID)
	if err != nil {
		t.Fatalf("mgr.GetConfig: %v", err)
	}
	if string(persisted) != `{"greeting":"bye"}` {
		t.Fatalf("mgr.GetConfig after SaveConfig = %s, want the value SaveConfig wrote", persisted)
	}

	// Capabilities reflects the granted host_apis (just "network" here).
	caps, err := h.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps) != 1 || caps[0] != "network" {
		t.Fatalf("Capabilities = %v, want [network]", caps)
	}

	// The granted network host must not be refused before the request even
	// leaves the process — pins that hostBuilderFor installs a permissive
	// OutboundPolicy (a deny-all default would block grant.Network hosts too).
	if _, err := h.HTTP().Do(context.Background(), &ext.HTTPRequest{Method: "GET", URL: "http://api.example.com/"}); err == ext.ErrPermissionDenied {
		t.Fatalf("HTTP().Do to granted host was permission-denied: %v", err)
	}

	// A host not on the granted allowlist is refused — grant.Network is the real
	// gate.
	if _, err := h.HTTP().Do(context.Background(), &ext.HTTPRequest{Method: "GET", URL: "http://evil.example.com/"}); err != ext.ErrPermissionDenied {
		t.Fatalf("HTTP().Do to ungranted host = %v, want ErrPermissionDenied", err)
	}

	// An uninstalled/unknown plugin id still gets a (default-deny) host —
	// GrantForID returns nil, and toHostcapGrant(nil) yields no host_apis / no
	// network.
	h2 := builder("no.such.plugin")
	if h2 == nil {
		t.Fatal(`hostBuilderFor(...)("no.such.plugin") = nil, want a (default-deny) host`)
	}
	if _, err := h2.HTTP().Do(context.Background(), &ext.HTTPRequest{Method: "GET", URL: "http://api.example.com/"}); err != ext.ErrPermissionDenied {
		t.Fatalf("unknown-plugin host HTTP().Do = %v, want ErrPermissionDenied", err)
	}
}

// TestHostBuilder_KVSharedAndPersistent pins that hostBuilderFor wires a single,
// disk-backed KVBackend (not a fresh in-memory one per launch): two hosts built
// from the same hostBuilderFor(mgr) share KV data, and — because the backend is
// rooted at the Manager's own dir — a brand new hostBuilderFor(mgr) still reads
// it back, i.e. it survives what a process restart would look like.
func TestHostBuilder_KVSharedAndPersistent(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := extension.NewManager(filepath.Join(t.TempDir(), "plugins"), extension.NewRegistry(), extension.NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}

	m := testManifest("com.example.kvbridge", "1.0.0", ext.Permissions{
		HostAPIs: []string{hostcap.CapStore},
	})
	pkg := buildSignedTestPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.SetGrant(m.ID, &extension.Grant{
		HostAPIs: []extension.GrantItem{{Name: hostcap.CapStore}},
	}, false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}

	build := hostBuilderFor(mgr)
	hA := build(m.ID)
	if err := hA.Store().KV().Set(context.Background(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}

	hB := build(m.ID)
	v, ok, err := hB.Store().KV().Get(context.Background(), "k")
	if err != nil || !ok || string(v) != "v" {
		t.Fatalf("shared KV read = %q ok=%v err=%v", v, ok, err)
	}

	// fresh builder over same dir → still there (persisted to <dir>/<id>/kv.json)
	if v, ok, _ := hostBuilderFor(mgr)(m.ID).Store().KV().Get(context.Background(), "k"); !ok || string(v) != "v" {
		t.Fatalf("persisted KV read = %q ok=%v", v, ok)
	}
}
