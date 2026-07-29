package handles

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// init (not a lazy, test-triggered sync.Once) points flags.DataDir — and so
// both extension.PluginsDir()/DefaultManager's "<DataDir>/plugins" AND
// internal/op's storageSecretsKey() (G4 §1, which resolves the shared
// secrets key via extension.PluginsDir() directly, bypassing DefaultManager)
// — at a scratch dir before ANY test in this package runs. This must NOT be
// gated behind a test-called setup helper (the earlier shape): a test that
// never calls such a helper — e.g. storage_secret_redteam_test.go's
// encryption tests, which only touch op.CreateStorage/UpdateStorage — would
// otherwise resolve flags.DataDir's zero value ("") into a bare relative
// "plugins" dir under this package's own source directory, leaking a
// scratch plugins/.secrets.key into the repo. Whether that leak occurred
// used to depend on go test's (unspecified) intra-package run order; an
// init() runs unconditionally at program start, before any test, so it's
// no longer a race.
func init() {
	dir, err := os.MkdirTemp("", "alist-plugin-guard-*")
	if err != nil {
		panic(err)
	}
	flags.DataDir = dir
}

// setupPluginGuardEnv relaxes plugin_policy to "any" for the duration of the
// test, so an unsigned test package installs cleanly instead of being
// rejected by the default official-only trust floor. Restores the policy
// setting on cleanup. (flags.DataDir itself is redirected package-wide by
// the init() above, not here.)
func setupPluginGuardEnv(t *testing.T) {
	t.Helper()
	prev := ""
	if item, err := op.GetSettingItemByKey(conf.PluginPolicy); err == nil && item != nil {
		prev = item.Value
	}
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.PluginPolicy, Value: extension.PolicyAnyValue}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = op.SaveSettingItem(&model.SettingItem{Key: conf.PluginPolicy, Value: prev})
	})
}

// guardFakeAddition is the empty Additional for guardFakeDriver.
type guardFakeAddition struct{}

// guardFakeDriver is a minimal driver.Driver standing in for a plugin-backed
// storage driver in tests: its Config().Name is the plugin id it is mounted
// under, which is exactly what pluginStorages (plugins.go) matches on. Real
// plugin-backed storages resolve the same way in production — GetStorageDriver
// keys the registry by plugin id — so registering one of these under a test
// plugin's id is a faithful stand-in without needing a real gRPC subprocess.
type guardFakeDriver struct {
	model.Storage
	name string
}

func (d *guardFakeDriver) Config() driver.Config          { return driver.Config{Name: d.name, NoCache: true} }
func (d *guardFakeDriver) GetAddition() driver.Additional { return &guardFakeAddition{} }
func (d *guardFakeDriver) Init(ctx context.Context) error { return nil }
func (d *guardFakeDriver) Drop(ctx context.Context) error { return nil }
func (d *guardFakeDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}
func (d *guardFakeDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return nil, errors.New("guardFakeDriver: Link not implemented")
}

// guardPluginNoForceID / guardPluginForceID are distinct fake driver names
// (and matching plugin ids, see writeGuardPluginPackage) so the two guard
// tests below don't step on each other's registry entries or mounted
// storages.
const (
	guardPluginNoForceID = "guardpluginnoforce"
	guardPluginForceID   = "guardpluginforce"
)

func init() {
	op.RegisterDriver(func() driver.Driver { return &guardFakeDriver{name: guardPluginNoForceID} })
	op.RegisterDriver(func() driver.Driver { return &guardFakeDriver{name: guardPluginForceID} })
}

// writeGuardPluginPackage builds a minimal, unsigned plugin package (manifest
// + a placeholder "plugin" binary) under id and packs it into a .tar.gz,
// returning the archive path. Install only needs the package to gate cleanly
// (see setupPluginGuardEnv's plugin_policy=any) and to have a runnable
// binary present — the placeholder is never actually executed by these tests
// (Disable/Uninstall never spawn the plugin subprocess).
func writeGuardPluginPackage(t *testing.T, id string) string {
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
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			t.Fatal(err)
		}
		hdr.Name = e.Name()
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// mountGuardStorage creates a live storage (visible via op.GetAllStorages)
// backed by the guardFakeDriver registered under driverName, mounted at
// mountPath. Returns its id.
func mountGuardStorage(t *testing.T, mountPath, driverName string) uint {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    driverName,
		Addition:  "{}",
	})
	if err != nil {
		t.Fatalf("create storage %s: %v", mountPath, err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	return id
}

func guardPluginAdmin() *model.User {
	return &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}
}

// TestDisablePluginRejectsWithMountedStoragesWithoutForce is the core
// dependency-guard regression: a plugin backing a live storage must not be
// disabled out from under it. Without ?force=true the handler must refuse
// with 409 and report the blocking storage(s) in data.storages, and the
// plugin's own state must be untouched (pluginStateChange never reached).
func TestDisablePluginRejectsWithMountedStoragesWithoutForce(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)

	sid := mountGuardStorage(t, "/guard-noforce", guardPluginNoForceID)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: guardPluginNoForceID}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+guardPluginNoForceID+"/disable", nil)

	DisablePlugin(c)

	if code := respCode(t, w); code != 409 {
		t.Fatalf("expected 409, got %d: %s", code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Storages []mountedStorageDep `json:"storages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if len(resp.Data.Storages) != 1 || resp.Data.Storages[0].MountPath != "/guard-noforce" || resp.Data.Storages[0].ID != sid {
		t.Fatalf("expected the mounted storage listed as a blocker, got %+v", resp.Data.Storages)
	}

	// The storage must still be live — no cascade happened without force.
	if _, err := op.GetStorageByMountPath("/guard-noforce"); err != nil {
		t.Fatalf("storage should still be mounted after a refused disable: %v", err)
	}
}

// TestDisablePluginForceCascadesMountedStorages is the force-cascade path:
// with ?force=true the handler must disable every dependent storage first,
// then proceed to actually disable the plugin.
func TestDisablePluginForceCascadesMountedStorages(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	pkg := writeGuardPluginPackage(t, guardPluginForceID)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install guard plugin: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(guardPluginForceID) })

	mountGuardStorage(t, "/guard-force", guardPluginForceID)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: guardPluginForceID}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+guardPluginForceID+"/disable?force=true", nil)

	DisablePlugin(c)

	if code := respCode(t, w); code != 200 {
		t.Fatalf("expected 200 after forced cascade, got %d: %s", code, w.Body.String())
	}

	// The dependent storage must have been disabled (dropped from the live
	// map) by the cascade.
	if _, err := op.GetStorageByMountPath("/guard-force"); err == nil {
		t.Fatal("expected the dependent storage to be disabled by the force cascade")
	}

	// And the plugin disable itself must have gone through.
	st, ok := mgr.Get(guardPluginForceID)
	if !ok || st.Enabled {
		t.Fatalf("expected plugin disabled after forced cascade, got ok=%v state=%+v", ok, st)
	}
}

// TestUninstallPluginRejectsWithMountedStoragesWithoutForce exercises the
// same guard through UninstallPlugin, confirming the two handlers share the
// guard rather than each reimplementing it.
func TestUninstallPluginRejectsWithMountedStoragesWithoutForce(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)

	sid := mountGuardStorage(t, "/guard-noforce-uninstall", guardPluginNoForceID)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: guardPluginNoForceID}}
	c.Request = httptest.NewRequest("DELETE", "/api/v4/admin/plugins/"+guardPluginNoForceID, nil)

	UninstallPlugin(c)

	if code := respCode(t, w); code != 409 {
		t.Fatalf("expected 409, got %d: %s", code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Storages []mountedStorageDep `json:"storages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if len(resp.Data.Storages) != 1 || resp.Data.Storages[0].MountPath != "/guard-noforce-uninstall" || resp.Data.Storages[0].ID != sid {
		t.Fatalf("expected the mounted storage listed as a blocker, got %+v", resp.Data.Storages)
	}
	if _, err := op.GetStorageByMountPath("/guard-noforce-uninstall"); err != nil {
		t.Fatalf("storage should still be mounted after a refused uninstall: %v", err)
	}
}
