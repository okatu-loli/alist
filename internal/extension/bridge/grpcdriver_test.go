package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
	"github.com/alist-org/alist/v3/internal/extension/pluginproc"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/stream"
)

// killPluginProcess sends SIGKILL to d's live subprocess (simulating a crash)
// and blocks until go-plugin's own process-reaper goroutine reports it via
// Exited(), so callers don't race that detection.
func killPluginProcess(t *testing.T, d *pluginDriver) {
	t.Helper()
	hc, ok := d.client.(*host.Client)
	if !ok {
		t.Fatalf("client is %T, want *host.Client", d.client)
	}
	pid := hc.Pid()
	if pid == 0 {
		t.Fatal("client has no pid")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process %d: %v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill process %d: %v", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !hc.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("subprocess did not report Exited() after kill -9")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// withTempDir points conf.Conf.TempDir (used by stream.CacheFullInTempFileAndUpdateProgress
// / utils.CreateTempFile) at a scratch directory for the duration of a test.
// conf.Conf.TempDir defaults to the relative "temp" (flags.DataDir is "" in a
// test binary, never set by the real CLI's flag default), which does not
// exist in the test working directory — without this, materializing the
// upload's random-access source fails with a plain os.Open error before ever
// reaching the plugin.
func withTempDir(t *testing.T) {
	t.Helper()
	old := conf.Conf.TempDir
	conf.Conf.TempDir = t.TempDir()
	t.Cleanup(func() { conf.Conf.TempDir = old })
}

func errorsIsSentinel(got, want error) bool { return errors.Is(got, want) }

func errsNew(msg string) error { return errors.New(msg) }

func TestTranslateExtErr(t *testing.T) {
	cases := map[ext.ErrCode]error{
		ext.ErrCodeNotImplement:     errs.NotImplement,
		ext.ErrCodeNotSupport:       errs.NotSupport,
		ext.ErrCodeObjectNotFound:   errs.ObjectNotFound,
		ext.ErrCodePermissionDenied: errs.PermissionDenied,
		ext.ErrCodeStorageNotFound:  errs.StorageNotFound,
	}
	for code, want := range cases {
		got := translateExtErr(ext.NewError(code, "x"))
		if !errorsIsSentinel(got, want) {
			t.Fatalf("code %s → %v, want %v", code, got, want)
		}
	}
	// non-ext.Error passes through unchanged
	raw := errsNew("boom")
	if translateExtErr(raw) != raw {
		t.Fatal("non-ext error must pass through unchanged")
	}
	if translateExtErr(nil) != nil {
		t.Fatal("nil stays nil")
	}
}

func TestExtObjToModel(t *testing.T) {
	o := extObjToModel(ext.Obj{Name: "a.txt", Size: 12, Modified: 1000, Created: 2000, IsDir: false, ID: "id1", Path: "/a.txt"})
	if o.GetName() != "a.txt" || o.GetSize() != 12 || o.IsDir() {
		t.Fatalf("mapped wrong: %+v", o)
	}
	if o.GetID() != "id1" || o.GetPath() != "/a.txt" {
		t.Fatalf("id/path wrong: %+v", o)
	}
}

// buildURLPlugin compiles the gRPC parity test's urlplugin fixture, mirroring
// pluginproc's own test helper. It skips the test if the go toolchain build
// fails (e.g. no network for module resolution in a locked-down environment).
func buildURLPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "urlplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urlplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginDriver: build urlplugin: " + err.Error() + "\n" + string(b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginDriver: built binary missing: " + err.Error())
	}
	return out
}

// TestPluginDriverEndToEnd exercises NewPluginDriver against the real urlplugin
// gRPC subprocess: Init acquires the shared process, List returns the fixture's
// stub object, Drop tears the instance (and, since it's the only referent, the
// subprocess) down. This is the "adapter + process manager" path, independent
// of op/DB.
func TestPluginDriverEndToEnd(t *testing.T) {
	bin := buildURLPlugin(t)
	pm := pluginproc.New()
	caps := []ext.Capability{ext.CapGetter}

	d := NewPluginDriver("com.example.urldriver", bin, caps, false, pm)
	d.SetStorage(model.Storage{ID: 1, Driver: "com.example.urldriver"})

	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if pm.Count() != 1 {
		t.Fatalf("proc count=%d want 1 after Init", pm.Count())
	}

	root, err := d.(driver.GetRooter).GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}

	objs, err := d.List(ctx, root, model.ListArgs{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 || objs[0].GetName() != "hello.txt" || objs[0].GetSize() != 11 || objs[0].IsDir() {
		t.Fatalf("List returned unexpected objs: %+v", objs)
	}

	if err := d.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if pm.Count() != 0 {
		t.Fatalf("proc count=%d want 0 after Drop", pm.Count())
	}
}

// TestGenericAdditionMarshalJSONRoundTrip is the regression pin for the
// credential-loss bug: op.saveDriverStorage runs on every initStorage (i.e.
// every server restart, not just an admin edit) and persists
// json.Marshal(driver.GetAddition()). Before MarshalJSON existed,
// genericAddition's default struct encoding only emitted the exported
// RootPath/RootID fields; plugin-specific fields (client_id, client_secret,
// ...) live only in the unexported raw string and were silently dropped —
// wiping saved credentials on every restart. MarshalJSON must round-trip raw
// verbatim so nothing is lost.
func TestGenericAdditionMarshalJSONRoundTrip(t *testing.T) {
	in := `{"client_id":"cid","client_secret":"sec","root_folder_id":"123"}`
	var g genericAddition
	if err := g.UnmarshalJSON([]byte(in)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	out, err := json.Marshal(&g)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-unmarshal output: %v", err)
	}
	want := map[string]string{"client_id": "cid", "client_secret": "sec", "root_folder_id": "123"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("round-trip lost field %q: got %v, want %q (full output: %s)", k, got[k], v, out)
		}
	}
}

// TestGenericAdditionMarshalJSONZeroValue guards the raw=="" fallback: a
// never-unmarshaled genericAddition (e.g. a fresh zero value) must marshal
// to "{}" rather than panicking or emitting an empty/invalid byte slice.
func TestGenericAdditionMarshalJSONZeroValue(t *testing.T) {
	var g genericAddition
	out, err := json.Marshal(&g)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != "{}" {
		t.Fatalf("zero-value marshal = %q, want \"{}\"", out)
	}
}

// TestPluginDriverGetRootAnchor pins the fallback branch of GetRoot (no
// GetRooter capability, so it synthesizes the root from DriverConfig's
// DefaultRoot): an ID-anchored plugin (pathAnchored=false, the ext-contract
// default) must get a root with ID set and Path empty, and a path-anchored
// plugin (root_folder_path-declared) the reverse. Getting this wrong is
// exactly the bug this test guards: a root with both ID and Path set makes
// op/fs.go's List "set path" step stamp a path-joined string onto every
// child object, and refAnchor (below) then prefers that stamped string over
// the child's real ID for every deeper List/Get/Link call — which an
// ID-anchored driver (e.g. 123 Open's numeric fileID) rejects, so nested
// browsing 500s while the (unaffected) root listing works.
//
// Constructed directly against the unexported struct (white-box, same
// package) rather than through Init/a subprocess: GetRoot's fallback branch
// only reads d.client (nil-safe type assertion), d.caps and d.cfg, none of
// which require a live plugin process.
func TestPluginDriverGetRootAnchor(t *testing.T) {
	ctx := context.Background()
	cfg := &ext.DriverConfig{DefaultRoot: "0"}

	idAnchored := &pluginDriver{cfg: cfg, pathAnchored: false}
	root, err := idAnchored.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot (id-anchored): %v", err)
	}
	if root.GetID() != "0" || root.GetPath() != "" {
		t.Fatalf("id-anchored root = ID:%q Path:%q, want ID:\"0\" Path:\"\"", root.GetID(), root.GetPath())
	}

	pathAnchored := &pluginDriver{cfg: cfg, pathAnchored: true}
	root2, err := pathAnchored.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot (path-anchored): %v", err)
	}
	if root2.GetPath() != "0" || root2.GetID() != "" {
		t.Fatalf("path-anchored root = ID:%q Path:%q, want ID:\"\" Path:\"0\"", root2.GetID(), root2.GetPath())
	}
}

// TestPluginDriverGetRootHonorsStorageAddition pins the actual bug report: a
// storage instance that configures root_folder_id (e.g. 123 Open's admin UI)
// must have GetRoot's fallback branch (no GetRooter capability) anchor on
// that configured root — not silently fall back to the manifest's
// DriverConfig.DefaultRoot, which is what an unconfigured storage gets.
// Before this fix, GetRoot's fallback only ever read d.cfg.DefaultRoot and
// never consulted d.add (the storage's own root_folder_id/root_folder_path),
// so a user-configured root_folder_id had no effect on browsing.
func TestPluginDriverGetRootHonorsStorageAddition(t *testing.T) {
	ctx := context.Background()
	cfg := &ext.DriverConfig{DefaultRoot: "0"}

	// ID-anchored: root_folder_id set on the storage addition wins over
	// DefaultRoot.
	idAnchored := &pluginDriver{cfg: cfg, pathAnchored: false}
	if err := idAnchored.add.UnmarshalJSON([]byte(`{"root_folder_id":"20565900"}`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	root, err := idAnchored.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot (id-anchored, configured): %v", err)
	}
	if root.GetID() != "20565900" || root.GetPath() != "" {
		t.Fatalf("id-anchored configured root = ID:%q Path:%q, want ID:\"20565900\" Path:\"\"", root.GetID(), root.GetPath())
	}

	// ID-anchored: root_folder_id unset on the addition falls back to
	// DefaultRoot, same as the zero-value case above.
	idAnchoredUnset := &pluginDriver{cfg: cfg, pathAnchored: false}
	if err := idAnchoredUnset.add.UnmarshalJSON([]byte(`{"client_id":"cid"}`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	root, err = idAnchoredUnset.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot (id-anchored, unconfigured): %v", err)
	}
	if root.GetID() != "0" || root.GetPath() != "" {
		t.Fatalf("id-anchored unconfigured root = ID:%q Path:%q, want ID:\"0\" Path:\"\"", root.GetID(), root.GetPath())
	}

	// Path-anchored: root_folder_path set on the storage addition wins over
	// DefaultRoot.
	pathAnchoredConfigured := &pluginDriver{cfg: cfg, pathAnchored: true}
	if err := pathAnchoredConfigured.add.UnmarshalJSON([]byte(`{"root_folder_path":"/configured/root"}`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	root2, err := pathAnchoredConfigured.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot (path-anchored, configured): %v", err)
	}
	if root2.GetPath() != "/configured/root" || root2.GetID() != "" {
		t.Fatalf("path-anchored configured root = ID:%q Path:%q, want ID:\"\" Path:\"/configured/root\"", root2.GetID(), root2.GetPath())
	}

	// Path-anchored: root_folder_path unset on the addition falls back to
	// DefaultRoot.
	pathAnchoredUnset := &pluginDriver{cfg: cfg, pathAnchored: true}
	if err := pathAnchoredUnset.add.UnmarshalJSON([]byte(`{}`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	root2, err = pathAnchoredUnset.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot (path-anchored, unconfigured): %v", err)
	}
	if root2.GetPath() != "0" || root2.GetID() != "" {
		t.Fatalf("path-anchored unconfigured root = ID:%q Path:%q, want ID:\"\" Path:\"0\"", root2.GetID(), root2.GetPath())
	}
}

// buildLocalPlugin compiles the 9.2 parity fixture that serves the built-in
// local driver as a go-plugin gRPC subprocess (its manifest — derived from
// the local driver's implemented legacy interfaces via
// extension.DeriveStorageCapabilities — declares CapPut, since the local
// driver implements driver.Put). It skips the test if the go toolchain build
// fails, mirroring buildURLPlugin.
func buildLocalPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "localplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/localplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginDriver Put: build localplugin: " + err.Error() + "\n" + string(b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginDriver Put: built binary missing: " + err.Error())
	}
	return out
}

// TestPluginDriverPutRealSubprocess is the strong-evidence end-to-end check
// for this task: op-shaped Put (a model.FileStreamer, exactly what
// internal/op/fs.go's op.Put hands a driver.Driver) travels
// pluginDriver.Put -> newUploadStreamAdapter -> the shared gRPC host
// proxy's ext.Putter.Put -> the Put bidi stream -> the localplugin
// subprocess -> the real local driver, and lands real bytes on the real
// filesystem. It also pins the progress plumbing (materialize phase 0-50,
// upload-stream Progress reports 50-100) and the PutResult Obj mapping.
func TestPluginDriverPutRealSubprocess(t *testing.T) {
	withTempDir(t)
	bin := buildLocalPlugin(t)
	pm := pluginproc.New()
	root := t.TempDir()

	// caps is only consulted by pluginDriver's hasCap-gated paths (GetRoot's
	// GetRooter branch, Link's stream branch) — Put's own gating happens
	// inside the gRPC host proxy from the plugin's self-reported manifest, so
	// an empty caps slice here is deliberate: it proves Put does not
	// (incorrectly) depend on this constructor argument.
	d := NewPluginDriver("com.example.localdriver.put", bin, nil, true, pm)
	d.SetStorage(model.Storage{ID: 1, Driver: "com.example.localdriver.put"})

	cfg, _ := json.Marshal(map[string]any{
		"root_folder_path": root,
		"mkdir_perm":       "777",
		"recycle_bin_path": "delete permanently",
	})
	if err := json.Unmarshal(cfg, d.GetAddition()); err != nil {
		t.Fatalf("unmarshal addition: %v", err)
	}

	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Drop(ctx) })

	dstDir, err := d.(driver.GetRooter).GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}

	content := []byte("uploaded through pluginDriver -> gRPC -> real local driver")
	fs := &stream.FileStream{
		Ctx:    ctx,
		Obj:    &model.Object{Name: "put.txt", Size: int64(len(content))},
		Reader: io.NopCloser(bytes.NewReader(content)),
	}

	var progress []float64
	up := func(p float64) { progress = append(progress, p) }

	pu, ok := d.(driver.PutResult)
	if !ok {
		t.Fatal("pluginDriver does not implement driver.PutResult")
	}
	newObj, err := pu.Put(ctx, dstDir, fs, up)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if newObj == nil || newObj.GetName() != "put.txt" || newObj.GetSize() != int64(len(content)) {
		t.Fatalf("Put returned obj = %+v", newObj)
	}

	got, err := os.ReadFile(filepath.Join(root, "put.txt"))
	if err != nil {
		t.Fatalf("uploaded file missing on disk: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("uploaded content = %q, want %q", got, content)
	}
	if len(progress) == 0 {
		t.Fatal("Put reported no progress at all")
	}
	// The materialize phase (newUploadStreamAdapter's 0-50 range) must reach
	// 50: this fixture's content is a plain io.Reader (not already a
	// model.File), so stream.CacheFullInTempFileAndUpdateProgress takes the
	// real-copy path and reports up to 100% scaled into [0,50]. The 50-100
	// half (the plugin's own Put-stream Progress reports, wired via
	// progressReporter above) never advances here: localplugin wraps the
	// built-in local driver through bridge.StorageBridge.Put (ops.go), which
	// passes the legacy driver a no-op UpdateProgress and so never calls the
	// plugin-side UploadStream's ReportProgress — a separate, pre-existing
	// gap in the *forward* (legacy-driver-as-plugin) bridge, out of this
	// task's scope (see report). A real out-of-tree ext.Putter plugin that
	// calls ReportProgress itself is unaffected: that path doesn't go
	// through ops.go at all.
	if last := progress[len(progress)-1]; last != 50 {
		t.Fatalf("Put's final progress report = %v, want 50 (materialize phase only, see comment)", last)
	}
}

// TestPluginDriverPutNotImplementedWithoutCapability confirms the capability
// gate: a plugin whose manifest does not declare CapPut (urlplugin, Getter
// only) must fail Put with errs.NotImplement rather than silently no-op'ing
// or panicking through the ext.Putter type assertion (the gRPC host proxy
// structurally satisfies ext.Putter regardless of declared capabilities —
// gating happens inside its Put via the plugin's self-reported manifest, 9.0
// §4), mirroring TestURLCapabilityGate at the host.Client layer one level up.
func TestPluginDriverPutNotImplementedWithoutCapability(t *testing.T) {
	withTempDir(t)
	bin := buildURLPlugin(t)
	pm := pluginproc.New()

	d := NewPluginDriver("com.example.urldriver.noput", bin, []ext.Capability{ext.CapGetter}, false, pm)
	d.SetStorage(model.Storage{ID: 1, Driver: "com.example.urldriver.noput"})

	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Drop(ctx) })

	fs := &stream.FileStream{
		Ctx:    ctx,
		Obj:    &model.Object{Name: "x.txt", Size: 1},
		Reader: io.NopCloser(bytes.NewReader([]byte("x"))),
	}
	_, err := d.(driver.PutResult).Put(ctx, &model.Object{Name: "root"}, fs, nil)
	if !errors.Is(err, errs.NotImplement) {
		t.Fatalf("Put on non-declaring plugin = %v, want errs.NotImplement", err)
	}
}

// TestPluginDriverSurvivesSubprocessCrash is the core regression test for the
// auto-restart feature: List succeeds once, the plugin subprocess is then
// killed (simulating a real crash — panic, OOM, segfault), and a subsequent
// List on the *same* driver — with no manual restart of alist, no re-mount —
// must self-heal: detect the dead connection, relaunch the subprocess,
// re-register this driver's storage instance on it, and complete the call.
func TestPluginDriverSurvivesSubprocessCrash(t *testing.T) {
	bin := buildURLPlugin(t)
	pm := pluginproc.New()
	caps := []ext.Capability{ext.CapGetter}

	d := NewPluginDriver("com.example.urldriver.crash", bin, caps, false, pm).(*pluginDriver)
	d.SetStorage(model.Storage{ID: 1, Driver: "com.example.urldriver.crash"})

	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Drop(ctx) })

	root, err := d.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}
	if _, err := d.List(ctx, root, model.ListArgs{}); err != nil {
		t.Fatalf("List before crash: %v", err)
	}

	killPluginProcess(t, d)

	objs, err := d.List(ctx, root, model.ListArgs{})
	if err != nil {
		t.Fatalf("List after crash should self-heal, got: %v", err)
	}
	if len(objs) != 1 || objs[0].GetName() != "hello.txt" {
		t.Fatalf("List after crash returned unexpected objs: %+v", objs)
	}
	if pm.Count() != 1 {
		t.Fatalf("proc count=%d want 1 (single relaunch, no leaked process)", pm.Count())
	}

	// The healed connection must keep working for subsequent calls too, not
	// just the one retry.
	if _, err := d.List(ctx, root, model.ListArgs{}); err != nil {
		t.Fatalf("List after self-heal: %v", err)
	}
}

// TestPluginDriverConcurrentCallersCollapseRestart pins the concurrency
// guard end-to-end: two pluginDriver instances standing in for two storages
// mounted on the same plugin (sharing one pm and pluginID, exactly like
// GetStorageDriver's factory wiring in server/handles/plugins.go) both hit
// the dead subprocess concurrently. Both calls must succeed (each
// re-registers its own storage instance on whichever client — old racer's or
// the swapped-in fresh one — it ends up using), and only one subprocess
// relaunch may happen.
func TestPluginDriverConcurrentCallersCollapseRestart(t *testing.T) {
	bin := buildURLPlugin(t)
	pm := pluginproc.New()
	caps := []ext.Capability{ext.CapGetter}
	ctx := context.Background()

	d1 := NewPluginDriver("com.example.urldriver.concurrent", bin, caps, false, pm).(*pluginDriver)
	d1.SetStorage(model.Storage{ID: 1, Driver: "com.example.urldriver.concurrent"})
	if err := d1.Init(ctx); err != nil {
		t.Fatalf("Init d1: %v", err)
	}
	t.Cleanup(func() { _ = d1.Drop(ctx) })

	d2 := NewPluginDriver("com.example.urldriver.concurrent", bin, caps, false, pm).(*pluginDriver)
	d2.SetStorage(model.Storage{ID: 2, Driver: "com.example.urldriver.concurrent"})
	if err := d2.Init(ctx); err != nil {
		t.Fatalf("Init d2: %v", err)
	}
	t.Cleanup(func() { _ = d2.Drop(ctx) })

	if pm.Count() != 1 {
		t.Fatalf("proc count=%d want 1 (both storages share one subprocess)", pm.Count())
	}

	root, err := d1.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}
	killPluginProcess(t, d1) // d1 and d2 share the subprocess; killing via either observes the same process

	var wg sync.WaitGroup
	errs1 := make([]error, 4)
	errs2 := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, errs1[i] = d1.List(ctx, root, model.ListArgs{})
		}(i)
		go func(i int) {
			defer wg.Done()
			_, errs2[i] = d2.List(ctx, root, model.ListArgs{})
		}(i)
	}
	wg.Wait()

	for i, err := range errs1 {
		if err != nil {
			t.Fatalf("d1.List[%d] after crash: %v", i, err)
		}
	}
	for i, err := range errs2 {
		if err != nil {
			t.Fatalf("d2.List[%d] after crash: %v", i, err)
		}
	}
	if pm.Count() != 1 {
		t.Fatalf("proc count=%d want 1 (concurrent restarts from both storages must collapse into one subprocess)", pm.Count())
	}
}

// TestPluginDriverRestartFailureReturnsCallerError pins the bounded-retry,
// no-panic failure path: if the plugin binary is gone by the time the crash
// is detected, restart cannot succeed — the original call must return an
// error (not panic, not hang retrying) and it must be the underlying
// operation's error, not an opaque wrapper.
func TestPluginDriverRestartFailureReturnsCallerError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "urlplugin")
	fixtureData, err := os.ReadFile(buildURLPlugin(t))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(bin, fixtureData, 0o755); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	pm := pluginproc.New()
	caps := []ext.Capability{ext.CapGetter}
	d := NewPluginDriver("com.example.urldriver.gone", bin, caps, false, pm).(*pluginDriver)
	d.SetStorage(model.Storage{ID: 1, Driver: "com.example.urldriver.gone"})

	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	root, err := d.GetRoot(ctx)
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}
	killPluginProcess(t, d)
	if err := os.Remove(bin); err != nil {
		t.Fatalf("remove binary: %v", err)
	}

	if _, err := d.List(ctx, root, model.ListArgs{}); err == nil {
		t.Fatal("List after crash with a missing binary must return an error, not silently succeed")
	}
}
