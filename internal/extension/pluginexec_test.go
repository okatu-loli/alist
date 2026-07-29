// This file exercises the 9.3 op-level mount/browse seam end to end: an
// enabled gRPC plugin entry, wired through SetPluginDriverFactory, must be
// mountable and browsable through the same op.CreateStorage/op.List/op.MakeDir/
// op.Rename surface a built-in driver uses. It lives in the black-box
// extension_test package (rather than package extension) because it needs
// internal/op, and internal/op already imports internal/extension — importing
// op from inside package extension itself would be a cycle.
package extension_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/bridge"
	"github.com/alist-org/alist/v3/internal/extension/pluginproc"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// init boots an in-memory sqlite DB and runs migrations, mirroring
// internal/op/storage_test.go's bootstrap so op.CreateStorage/DeleteStorageById
// have somewhere to persist storages. This test file's package
// (extension_test) is its own binary from internal/op's tests, so there is no
// double-init hazard across packages.
func init() {
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic("failed to migrate test database: " + err.Error())
	}
}

// pluginEntryID is the extension registry id under which the fixture is
// mounted. It is deliberately distinct from urldriver's own Manifest.ID
// ("com.example.urldriver"): pluginID is only a pluginproc process-registry
// key, not something the plugin binary itself needs to agree with.
const pluginEntryID = "com.example.url"

// buildURLPluginFixture compiles the gRPC parity test's urlplugin fixture,
// mirroring bridge_test.go's own helper. It skips the test outright if the go
// toolchain build fails (e.g. no module cache / no network in a locked-down
// environment).
func buildURLPluginFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "urlplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urlplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip plugin storage e2e: build urlplugin: " + err.Error() + "\n" + string(b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip plugin storage e2e: built binary missing: " + err.Error())
	}
	return out
}

// TestPluginStorageEndToEnd is the join point: GetStorageDriver's plugin
// branch, bridge.NewPluginDriver, and op's mount/browse/mutate surface all
// exercised together. It runs red-then-green in place: op.CreateStorage must
// fail with "no driver named" both before the entry is registered and again
// after registration but before SetPluginDriverFactory is wired, proving the
// factory seam (not just registry presence) is what makes mounting work.
func TestPluginStorageEndToEnd(t *testing.T) {
	if _, err := op.GetDriver(pluginEntryID); err == nil {
		t.Fatal("red check: expected \"no driver named\" before the plugin entry is registered")
	}

	bin := buildURLPluginFixture(t)
	pm := pluginproc.New() // isolated manager: does not touch pluginproc.Default

	m := ext.Manifest{
		ID:           pluginEntryID,
		Name:         map[string]string{"en": "URL Test Plugin"},
		Version:      "0.1.0",
		CoreAPI:      "^4.0.0",
		Kinds:        []ext.Kind{ext.KindStorageDriver},
		Capabilities: []ext.Capability{ext.CapGetter},
		Provenance:   ext.Provenance{Source: "official", License: "MIT"},
	}
	if err := extension.DefaultRegistry.RegisterPluginMode(m, bin, extension.ExecGRPC); err != nil {
		t.Fatalf("RegisterPluginMode: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(pluginEntryID) })

	if _, err := op.GetDriver(pluginEntryID); err == nil {
		t.Fatal("red check: expected \"no driver named\" before SetPluginDriverFactory is installed")
	}

	extension.SetPluginDriverFactory(func(e *extension.Entry) driver.Driver {
		return bridge.NewPluginDriver(e.ID(), e.BinPath(), e.Capabilities(), extension.IsPathAnchored(e.Manifest), pm)
	})
	t.Cleanup(func() { extension.SetPluginDriverFactory(nil) })

	// Green: the factory is wired, mounting must now succeed.
	ctx := context.Background()
	id, err := op.CreateStorage(ctx, model.Storage{Driver: pluginEntryID, MountPath: "/plug", Addition: "{}"})
	if err != nil {
		t.Fatalf("CreateStorage: %v", err)
	}
	storageDriver, err := op.GetStorageByMountPath("/plug")
	if err != nil {
		t.Fatalf("GetStorageByMountPath: %v", err)
	}
	if status := storageDriver.GetStorage().Status; status != op.WORK {
		t.Fatalf("storage status = %q, want %q", status, op.WORK)
	}

	t.Cleanup(func() {
		if err := op.DeleteStorageById(context.Background(), id); err != nil {
			t.Errorf("DeleteStorageById: %v", err)
		}
		if n := pm.Count(); n != 0 {
			t.Errorf("pluginproc count = %d, want 0 after storage teardown", n)
		}
	})

	if n := pm.Count(); n != 1 {
		t.Fatalf("pluginproc count = %d, want 1 after mount", n)
	}

	// List: browse the mounted plugin storage, expect urlplugin's fixed stub
	// object back.
	objs, err := op.List(ctx, storageDriver, "/", model.ListArgs{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("List returned %d objs, want 1: %+v", len(objs), objs)
	}
	if objs[0].GetName() != "hello.txt" || objs[0].GetSize() != 11 || objs[0].IsDir() {
		t.Fatalf("List returned unexpected obj: %+v", objs[0])
	}

	// MakeDir/Rename: urlplugin declares only CapGetter, so these must fail
	// gracefully with NotImplement (translated back across the gRPC boundary by
	// bridge.translateExtErr) rather than panicking or behaving as no-ops.
	if err := op.MakeDir(ctx, storageDriver, "/newdir"); !errs.IsNotImplement(err) {
		t.Fatalf("MakeDir on a Getter-only plugin: got %v, want errs.NotImplement", err)
	}
	if err := op.Rename(ctx, storageDriver, "/hello.txt", "renamed.txt"); !errs.IsNotImplement(err) {
		t.Fatalf("Rename on a Getter-only plugin: got %v, want errs.NotImplement", err)
	}
}
