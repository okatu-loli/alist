package hostcap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestMain stands up the same in-memory DB + migration + default conf the gRPC
// parity tests use, so op.Authorize / fs.* have a real core to run against.
// tokenstore.Init + common.SecretKey mirror what internal/bootstrap wires in
// production (server/router.go, internal/bootstrap/db.go) — VerifyToken tests
// need real signed/tracked tokens, the same way server/middlewares.Auth does.
func TestMain(m *testing.M) {
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic("failed to migrate test database: " + err.Error())
	}
	tokenstore.Init(tokenstore.NewSQLiteStore())
	common.SecretKey = []byte("hostcap-test-secret")
	os.Exit(m.Run())
}

// mountLocal mounts a local-driver storage at mountPath backed by a fresh temp
// dir seeded with one file and one subdir, and returns the temp dir. It is the
// real filesystem host.FS borrows.
func mountLocal(t *testing.T, mountPath string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, root),
	})
	if err != nil {
		t.Fatalf("mount local storage: %v", err)
	}
	return root
}

// Invariant 1: FS runs op.Authorize on every op; an allowed subtree lists, a
// path outside the grant is denied, and a missing fs capability is denied.
func TestFSAuthorizeInvariant(t *testing.T) {
	mnt := "/inv1"
	mountLocal(t, mnt)
	ctx := context.Background()

	granted := New(Grant{
		PluginID: "p.fs",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: mnt, Write: true}},
	}, Options{})

	// Allowed: list the granted subtree.
	objs, err := granted.FS().List(ctx, "", ext.ObjRef{Anchor: mnt}, ext.ListArgs{})
	if err != nil {
		t.Fatalf("List granted subtree: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 entries (seed.txt, sub), got %d: %+v", len(objs), objs)
	}

	// Denied: a path outside the grant fails at Authorize.
	if _, err := granted.FS().List(ctx, "", ext.ObjRef{Anchor: "/somewhere-else"}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("List outside grant: want ErrPermissionDenied, got %v", err)
	}

	// Denied: no fs capability at all.
	nofs := New(Grant{PluginID: "p.nofs", FS: []FSScope{{Path: mnt}}}, Options{})
	if _, err := nofs.FS().List(ctx, "", ext.ObjRef{Anchor: mnt}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("List without CapFS: want ErrPermissionDenied, got %v", err)
	}
}

// Invariant 1 (write): Mkdir under a read-only grant is denied; under a
// read-write grant it succeeds.
func TestFSWriteAuthorization(t *testing.T) {
	mnt := "/inv1w"
	mountLocal(t, mnt)
	ctx := context.Background()

	ro := New(Grant{PluginID: "p.ro", HostAPIs: []GrantItem{{Name: CapFS}}, FS: []FSScope{{Path: mnt}}}, Options{})
	if _, err := ro.FS().Mkdir(ctx, "", ext.ObjRef{Anchor: mnt}, "newdir"); err != ext.ErrPermissionDenied {
		t.Fatalf("Mkdir read-only: want ErrPermissionDenied, got %v", err)
	}

	rw := New(Grant{PluginID: "p.rw", HostAPIs: []GrantItem{{Name: CapFS}}, FS: []FSScope{{Path: mnt, Write: true}}}, Options{})
	if _, err := rw.FS().Mkdir(ctx, "", ext.ObjRef{Anchor: mnt}, "newdir"); err != nil {
		t.Fatalf("Mkdir read-write: %v", err)
	}
	objs, _ := rw.FS().List(ctx, "", ext.ObjRef{Anchor: mnt}, ext.ListArgs{})
	if len(objs) != 3 {
		t.Fatalf("after Mkdir want 3 entries, got %d", len(objs))
	}
}

// Invariant 3: Store is namespaced by plugin id — plugin B cannot read plugin
// A's data even sharing one backend.
func TestStoreIsolationInvariant(t *testing.T) {
	ctx := context.Background()
	backend := NewMemKVBackend()
	grant := func(id string) Grant {
		return Grant{PluginID: id, HostAPIs: []GrantItem{{Name: CapStore}}}
	}
	a := New(grant("plugin.A"), Options{KVBackend: backend})
	b := New(grant("plugin.B"), Options{KVBackend: backend})

	if err := a.Store().KV().Set(ctx, "secret", []byte("A-only")); err != nil {
		t.Fatal(err)
	}
	// B reads its own (empty) namespace, not A's.
	if _, ok, _ := b.Store().KV().Get(ctx, "secret"); ok {
		t.Fatal("plugin B read plugin A's key — namespace isolation broken")
	}
	v, ok, _ := a.Store().KV().Get(ctx, "secret")
	if !ok || string(v) != "A-only" {
		t.Fatalf("plugin A cannot read its own key: ok=%v v=%q", ok, v)
	}

	// Invariant 5: no store capability → denied.
	nostore := New(Grant{PluginID: "plugin.C"}, Options{KVBackend: backend})
	if err := nostore.Store().KV().Set(ctx, "k", []byte("v")); err != ext.ErrPermissionDenied {
		t.Fatalf("Set without CapStore: want ErrPermissionDenied, got %v", err)
	}
}

// Invariant 2 + 5: HTTP dials only hosts on the effective allowlist (global
// policy ∩ Grant.Network) and requires the network capability.
func TestHTTPAllowlistInvariant(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	host := mustHost(t, srv.URL)

	// Granted network + host on allowlist + open global policy → allowed.
	ok := New(Grant{
		PluginID: "p.net",
		HostAPIs: []GrantItem{{Name: CapNetwork}},
		Network:  []string{host},
	}, Options{OutboundPolicy: AllowAllOutbound})
	resp, err := ok.HTTP().Do(ctx, &ext.HTTPRequest{Method: http.MethodGet, URL: srv.URL})
	if err != nil {
		t.Fatalf("allowed request: %v", err)
	}
	if resp.Status != http.StatusOK || string(resp.Body) != "ok" {
		t.Fatalf("unexpected response: %d %q", resp.Status, resp.Body)
	}

	// Host NOT on the plugin's allowlist → denied (invariant 2).
	noHost := New(Grant{
		PluginID: "p.net2",
		HostAPIs: []GrantItem{{Name: CapNetwork}},
		Network:  []string{"other.example.com"},
	}, Options{OutboundPolicy: AllowAllOutbound})
	if _, err := noHost.HTTP().Do(ctx, &ext.HTTPRequest{URL: srv.URL}); err != ext.ErrPermissionDenied {
		t.Fatalf("host off allowlist: want ErrPermissionDenied, got %v", err)
	}

	// Global policy denies even though the grant allows → denied (defense in depth).
	policyDeny := New(Grant{
		PluginID: "p.net3",
		HostAPIs: []GrantItem{{Name: CapNetwork}},
		Network:  []string{host},
	}, Options{OutboundPolicy: OutboundPolicyFunc(func(string) bool { return false })})
	if _, err := policyDeny.HTTP().Do(ctx, &ext.HTTPRequest{URL: srv.URL}); err != ext.ErrPermissionDenied {
		t.Fatalf("global policy deny: want ErrPermissionDenied, got %v", err)
	}

	// No network capability → denied (invariant 5).
	nonet := New(Grant{PluginID: "p.net4", Network: []string{host}}, Options{OutboundPolicy: AllowAllOutbound})
	if _, err := nonet.HTTP().Do(ctx, &ext.HTTPRequest{URL: srv.URL}); err != ext.ErrPermissionDenied {
		t.Fatalf("no CapNetwork: want ErrPermissionDenied, got %v", err)
	}
}

// Invariant 4: Sign refuses to sign a ref the plugin's principal cannot read,
// and refuses without the sign capability.
func TestSignAuthorizationInvariant(t *testing.T) {
	mnt := "/inv4"
	mountLocal(t, mnt)
	ctx := context.Background()

	signer := New(Grant{
		PluginID: "p.sign",
		HostAPIs: []GrantItem{{Name: CapSign}},
		FS:       []FSScope{{Path: mnt}},
	}, Options{})

	// A ref outside the grant cannot be signed.
	if _, err := signer.Sign().SignLink(ctx, "", ext.ObjRef{Anchor: "/not-granted"}, ext.SignScopeDownload, time.Minute); err != ext.ErrPermissionDenied {
		t.Fatalf("sign un-granted ref: want ErrPermissionDenied, got %v", err)
	}

	// No sign capability → denied.
	nosign := New(Grant{PluginID: "p.nosign", FS: []FSScope{{Path: mnt}}}, Options{})
	if _, err := nosign.Sign().SignLink(ctx, "", ext.ObjRef{Anchor: mnt}, ext.SignScopeDownload, time.Minute); err != ext.ErrPermissionDenied {
		t.Fatalf("sign without CapSign: want ErrPermissionDenied, got %v", err)
	}

	// A granted ref signs and round-trips through VerifyLink when a download
	// signer is available. In a bare test DB one may not be initialized; skip
	// the positive leg rather than assert on an absent key set.
	tok, err := signer.Sign().SignLink(ctx, "", ext.ObjRef{StorageID: "1", Anchor: mnt}, ext.SignScopeDownload, time.Minute)
	if err != nil {
		if e, ok := err.(*ext.Error); ok && e.Code == ext.ErrCodeNotSupport {
			t.Skip("no download signer initialized in test db; positive sign path covered by integration")
		}
		t.Fatalf("sign granted ref: %v", err)
	}
	ref, scope, err := signer.Sign().VerifyLink(ctx, tok)
	if err != nil {
		t.Fatalf("verify link: %v", err)
	}
	if ref.Anchor != mnt || scope != ext.SignScopeDownload {
		t.Fatalf("verify mismatch: ref=%+v scope=%q", ref, scope)
	}
}

// Cache is per-plugin and gated: granted set/get round-trips; ungranted misses.
func TestCacheGrant(t *testing.T) {
	granted := New(Grant{PluginID: "p.cache", HostAPIs: []GrantItem{{Name: CapCache}}}, Options{})
	granted.Cache().Set("k", []byte("v"), time.Minute)
	if v, ok := granted.Cache().Get("k"); !ok || string(v) != "v" {
		t.Fatalf("granted cache get: ok=%v v=%q", ok, v)
	}
	ungranted := New(Grant{PluginID: "p.nocache"}, Options{})
	ungranted.Cache().Set("k", []byte("v"), time.Minute)
	if _, ok := ungranted.Cache().Get("k"); ok {
		t.Fatal("ungranted cache should miss")
	}
}

func TestNetworkAllowsGlob(t *testing.T) {
	g := Grant{Network: []string{"*.example.com", "exact.host"}}
	cases := map[string]bool{
		"api.example.com": true,
		"example.com":     true,
		"exact.host":      true,
		"evil.com":        false,
		"notexample.com":  false,
	}
	for host, want := range cases {
		if got := g.NetworkAllows(host); got != want {
			t.Errorf("NetworkAllows(%q) = %v, want %v", host, got, want)
		}
	}
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Hostname()
}
