package handles

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// buildDevPluginStaging compiles the gRPC parity test's urlplugin fixture
// (mirroring bridge/grpcdriver_test.go's buildURLPlugin / pluginproc's own
// helper) straight into a fresh staging dir named "plugin", alongside a
// hand-written plugin.json declaring id — a faithful stand-in for what a
// plugin dev CLI would hand DevLoadPlugin: a local directory with a manifest
// and a runnable binary, no signature. Skips the test if the go toolchain
// build fails (e.g. no network for module resolution in a locked-down
// environment).
func buildDevPluginStaging(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()

	man := ext.Manifest{
		ID:      id,
		Name:    map[string]string{"en": id},
		Version: "1.0.0",
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{
			Source:  "third-party",
			License: "MIT",
			Pricing: "free",
		},
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, extension.ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "plugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urlplugin")
	if out2, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip plugin dev load: build urlplugin: " + err.Error() + "\n" + string(out2))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip plugin dev load: built binary missing: " + err.Error())
	}
	return dir
}

// writeDevPluginManifest writes just a plugin.json (no binary) declaring id
// into a fresh dir — enough to exercise DevLoadPlugin's manifest-level checks
// (like the builtin.* namespace guard) without paying for a real go build,
// since those checks run before FindBinary.
func writeDevPluginManifest(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	man := ext.Manifest{
		ID:      id,
		Name:    map[string]string{"en": id},
		Version: "1.0.0",
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{
			Source:  "third-party",
			License: "MIT",
			Pricing: "free",
		},
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, extension.ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func devPluginAdmin() *model.User {
	return &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}
}

// postDevLoad drives DevLoadPlugin directly (the same handler-under-test
// convention plugins_test.go uses), bypassing router mounting/gating so
// these tests focus on the handler's own behavior. Route-mounting gating on
// flags.Dev is exercised separately by TestDevLoadGatedOnDevFlag, which goes
// through RegisterDevPluginRoutes + a real gin router. httptest.NewRequest
// defaults RemoteAddr to the RFC 5737 documentation address 192.0.2.1, which
// is NOT loopback — since DevLoadPlugin now rejects non-loopback callers
// (I1), it is overridden to 127.0.0.1 here so these handler-behavior tests
// exercise the rest of DevLoadPlugin as before; the loopback gate itself is
// exercised separately by TestDevLoadRejectsNonLoopbackClientIP.
func postDevLoad(dir string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, devPluginAdmin())
	body, _ := json.Marshal(gin.H{"dir": dir})
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v4/admin/plugins/dev/load", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.RemoteAddr = "127.0.0.1:54321"
	DevLoadPlugin(c)
	return w
}

// TestDevLoadGatedOnDevFlag: with flags.Dev=false, RegisterDevPluginRoutes
// must not mount the route at all — a POST against a real gin router hits no
// handler (404), not an authorization/handler-level rejection. RequireAdmin
// alone is not the intended gate; the route must be entirely absent in a
// non-dev deployment because DevLoadPlugin executes an arbitrary local binary
// path with no signature check.
func TestDevLoadGatedOnDevFlag(t *testing.T) {
	orig := flags.Dev
	flags.Dev = false
	defer func() { flags.Dev = orig }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v4/admin/plugins")
	RegisterDevPluginRoutes(g)

	w := httptest.NewRecorder()
	body, _ := json.Marshal(gin.H{"dir": "/does/not/matter"})
	req := httptest.NewRequest(http.MethodPost, "/api/v4/admin/plugins/dev/load", bytes.NewReader(body))
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (route not mounted) when flags.Dev=false, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDevLoadRejectsNonLoopbackClientIP is the I1 regression: dev/load's
// whole threat model assumes the plugin dev CLI and the server are the same
// machine — RequireAdmin alone is not a sufficient gate if a --dev instance
// ever ends up reachable beyond localhost, since the endpoint executes an
// arbitrary unsigned local binary. A request whose transport-layer RemoteAddr
// is not loopback must be rejected with 403 before the handler does anything
// else (bad dir, builtin namespace, etc. all still error, but never
// mistakenly as 400/500 from a non-loopback caller — it must be 403 first).
// Goes through the real router (RegisterDevPluginRoutes), not the
// postDevLoad bypass, since the check is exercised via RemoteAddr's actual
// resolution path.
func TestDevLoadRejectsNonLoopbackClientIP(t *testing.T) {
	orig := flags.Dev
	flags.Dev = true
	defer func() { flags.Dev = orig }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v4/admin/plugins")
	g.Use(func(c *gin.Context) {
		c.Set(ctxkey.User, devPluginAdmin())
		c.Next()
	})
	RegisterDevPluginRoutes(g)

	body, _ := json.Marshal(gin.H{"dir": "/does/not/matter"})
	req := httptest.NewRequest(http.MethodPost, "/api/v4/admin/plugins/dev/load", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// TEST-NET-3 (RFC 5737): guaranteed non-loopback, never routable.
	req.RemoteAddr = "203.0.113.7:54321"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if code := respCode(t, w); code != 403 {
		t.Fatalf("expected envelope code 403 for a non-loopback client ip, got %d: %s", code, w.Body.String())
	}
}

// TestDevLoadRejectsSpoofedLoopbackForwardedFor is the fix-round-2
// regression: a red-team PoC found that a plain c.ClientIP()-based gate is
// bypassable, because this codebase never calls gin's
// Engine.SetTrustedProxies, so gin's default trusts X-Forwarded-For (and the
// other RemoteIPHeaders) from ANY peer. A public-internet caller could set
// "X-Forwarded-For: 127.0.0.1" and have c.ClientIP() report loopback despite
// the real TCP peer being remote - completely defeating the gate this test
// file exists to verify. The fix checks c.Request.RemoteAddr directly
// (remoteAddrIsLoopback), which cannot be influenced by any request header;
// this test pins that down by sending the exact spoof and asserting it is
// STILL rejected.
func TestDevLoadRejectsSpoofedLoopbackForwardedFor(t *testing.T) {
	orig := flags.Dev
	flags.Dev = true
	defer func() { flags.Dev = orig }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v4/admin/plugins")
	g.Use(func(c *gin.Context) {
		c.Set(ctxkey.User, devPluginAdmin())
		c.Next()
	})
	RegisterDevPluginRoutes(g)

	body, _ := json.Marshal(gin.H{"dir": "/does/not/matter"})
	req := httptest.NewRequest(http.MethodPost, "/api/v4/admin/plugins/dev/load", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// The spoof: a real public-internet peer (TEST-NET-3, RFC 5737) claiming
	// to be loopback via a forwarding header gin would otherwise trust by
	// default.
	req.RemoteAddr = "203.0.113.9:54321"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if code := respCode(t, w); code != 403 {
		t.Fatalf("expected envelope code 403 for a spoofed X-Forwarded-For: 127.0.0.1 from a non-loopback peer, got %d: %s", code, w.Body.String())
	}
}

// TestDevLoadAcceptsLoopbackClientIP is the positive counterpart: a genuinely
// loopback RemoteAddr (no forwarding headers involved) must clear the gate
// and reach the rest of the handler (it still 400s here since the dir
// doesn't exist — the point is that it is NOT 403, i.e. the gate did not
// reject it).
func TestDevLoadAcceptsLoopbackClientIP(t *testing.T) {
	orig := flags.Dev
	flags.Dev = true
	defer func() { flags.Dev = orig }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v4/admin/plugins")
	g.Use(func(c *gin.Context) {
		c.Set(ctxkey.User, devPluginAdmin())
		c.Next()
	})
	RegisterDevPluginRoutes(g)

	dir := filepath.Join(t.TempDir(), "does-not-exist")
	body, _ := json.Marshal(gin.H{"dir": dir})
	req := httptest.NewRequest(http.MethodPost, "/api/v4/admin/plugins/dev/load", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if code := respCode(t, w); code != 400 {
		t.Fatalf("expected envelope code 400 (bad dir, past the loopback gate) for a loopback client ip, got %d: %s", code, w.Body.String())
	}
}

// TestDevLoadRegistersAndServes is the hot-load core happy path: flags.Dev is
// true, POSTing a hand-built staging dir (urlplugin binary + plugin.json)
// registers the plugin free of the trust-tier gate and the registry can
// immediately resolve a storage driver constructor for the id. A second POST
// against the same dir (simulating a dev-loop rebuild-and-reload) must still
// succeed — Deregister-then-reregister makes repeated loads idempotent
// instead of erroring on an existing id.
func TestDevLoadRegistersAndServes(t *testing.T) {
	orig := flags.Dev
	flags.Dev = true
	defer func() { flags.Dev = orig }()

	const id = "com.test.devhotreload.serves"
	dir := buildDevPluginStaging(t, id)
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(id) })

	w := postDevLoad(dir)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			ID               string   `json:"id"`
			Version          string   `json:"version"`
			Kinds            []string `json:"kinds"`
			ReloadedStorages int      `json:"reloaded_storages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if resp.Data.ID != id {
		t.Fatalf("expected id %q, got %q", id, resp.Data.ID)
	}
	if resp.Data.Version != "1.0.0" {
		t.Fatalf("expected version 1.0.0, got %q", resp.Data.Version)
	}
	if len(resp.Data.Kinds) != 1 || resp.Data.Kinds[0] != string(ext.KindStorageDriver) {
		t.Fatalf("expected kinds [%s], got %v", ext.KindStorageDriver, resp.Data.Kinds)
	}
	if resp.Data.ReloadedStorages != 0 {
		t.Fatalf("expected 0 reloaded storages (none mounted yet), got %d", resp.Data.ReloadedStorages)
	}

	if _, err := extension.DefaultRegistry.GetStorageDriver(id); err != nil {
		t.Fatalf("expected GetStorageDriver to resolve the freshly loaded plugin: %v", err)
	}

	// Second load (simulated rebuild): must still succeed, not error on the id
	// already being registered.
	w2 := postDevLoad(dir)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 on repeated dev load (idempotent), got %d: %s", w2.Code, w2.Body.String())
	}
	if _, err := extension.DefaultRegistry.GetStorageDriver(id); err != nil {
		t.Fatalf("expected GetStorageDriver to still resolve after repeated load: %v", err)
	}
}

// TestDevLoadBadDir: a directory with no plugin.json (or that does not exist)
// must be rejected with a 400 envelope, not a 500 or a panic.
func TestDevLoadBadDir(t *testing.T) {
	orig := flags.Dev
	flags.Dev = true
	defer func() { flags.Dev = orig }()

	dir := filepath.Join(t.TempDir(), "does-not-exist")

	w := postDevLoad(dir)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("expected envelope code 400, got %d: %s", code, w.Body.String())
	}
}

// TestDevLoadReloadsMountedStorages: a storage already mounted on the
// hot-loaded plugin's driver id must be replayed through op.LoadStorage on
// the NEXT dev load (the rebuild-and-reload path a plugin dev CLI drives),
// and the response must report exactly how many storages it touched.
func TestDevLoadReloadsMountedStorages(t *testing.T) {
	orig := flags.Dev
	flags.Dev = true
	defer func() { flags.Dev = orig }()

	const id = "com.test.devhotreload.reloadsstorages"
	dir := buildDevPluginStaging(t, id)
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(id) })

	// First load registers the plugin so a storage can be mounted on it.
	if w := postDevLoad(dir); w.Code != http.StatusOK {
		t.Fatalf("initial dev load failed: %d: %s", w.Code, w.Body.String())
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: "/dev-hotreload-reload-test",
		Driver:    id,
		Addition:  "{}",
	})
	if err != nil {
		t.Fatalf("mount storage on hot-loaded plugin: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	// Second load (simulated rebuild) must replay LoadStorage for the one
	// mounted storage.
	w := postDevLoad(dir)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			ReloadedStorages int `json:"reloaded_storages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if resp.Data.ReloadedStorages != 1 {
		t.Fatalf("expected reloaded_storages == 1, got %d", resp.Data.ReloadedStorages)
	}
}

// TestDevLoadRejectsBuiltinNamespace is the fix-round-1 regression: a dev
// plugin.json claiming a "builtin." id must be rejected with 400 BEFORE the
// handler ever calls Deregister — Deregister has no builtin guard of its own
// (only RegisterPluginMode does, and only after Deregister would already have
// run), so without this check a dev/load POST naming a live built-in's id
// would tear the built-in out of the registry and then fail to put anything
// back (RegisterPluginMode's own reserved-prefix rejection), leaving the
// server missing that built-in until restart. This test registers a real
// built-in entry first and asserts it is completely untouched afterward.
func TestDevLoadRejectsBuiltinNamespace(t *testing.T) {
	orig := flags.Dev
	flags.Dev = true
	defer func() { flags.Dev = orig }()

	const id = "builtin.devhotreload.guard"
	man := ext.Manifest{
		ID:      id,
		Name:    map[string]string{"en": "guard builtin"},
		Version: "builtin",
		CoreAPI: "builtin",
		Kinds:   []ext.Kind{ext.KindFeature},
		Provenance: ext.Provenance{
			Source:  "official",
			License: "builtin",
		},
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, man); err != nil {
		t.Fatalf("register builtin fixture: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(id) })

	dir := writeDevPluginManifest(t, id)

	w := postDevLoad(dir)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("expected envelope code 400 for a builtin.* id, got %d: %s", code, w.Body.String())
	}

	entry, ok := extension.DefaultRegistry.Get(id)
	if !ok {
		t.Fatal("expected the original builtin entry to still be registered after the rejected dev load")
	}
	if !entry.Builtin() {
		t.Fatal("expected the surviving entry to still be marked builtin")
	}
	if !entry.Enabled() {
		t.Fatal("expected the surviving builtin entry to still be enabled")
	}
}
