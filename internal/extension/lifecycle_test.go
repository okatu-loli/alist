package extension

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// buildSignedPackage writes a package dir (manifest + dummy binary), signs it
// with priv, packs it to a .tar.gz, and returns the archive path.
func buildSignedPackage(t *testing.T, m ext.Manifest, priv ed25519.PrivateKey) string {
	t.Helper()
	dir := writePackageDir(t, m)
	if err := SignDir(dir, priv); err != nil {
		t.Fatalf("SignDir: %v", err)
	}
	out := filepath.Join(t.TempDir(), "pkg.tar.gz")
	tarGzDir(t, dir, out)
	return out
}

func tarGzDir(t *testing.T, dir, out string) {
	t.Helper()
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
}

func newTestManager(t *testing.T, pub ed25519.PublicKey) (*Manager, *Registry) {
	t.Helper()
	reg := NewRegistry()
	mgr, err := NewManager(filepath.Join(t.TempDir(), "plugins"), reg, NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	return mgr, reg
}

func TestLifecycleRoundTrip(t *testing.T) {
	pub, priv := genKey(t)
	mgr, reg := newTestManager(t, pub)

	m := validManifest()
	m.Version = "1.0.0"
	pkg := buildSignedPackage(t, m, priv)

	// install
	got, err := mgr.Install(pkg)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got.Version != "1.0.0" {
		t.Fatalf("installed version = %s", got.Version)
	}
	st, ok := mgr.Get(m.ID)
	if !ok || st.ActiveVersion != "1.0.0" || st.Enabled {
		t.Fatalf("post-install state wrong: %+v ok=%v", st, ok)
	}
	// Version dir exists on disk.
	if _, err := os.Stat(mgr.versionDir(m.ID, "1.0.0")); err != nil {
		t.Fatalf("version dir missing: %v", err)
	}
	// Not registered until enabled.
	if _, ok := reg.Get(m.ID); ok {
		t.Fatal("plugin registered before enable")
	}

	// enable
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	entry, ok := reg.Get(m.ID)
	if !ok || !entry.Enabled() || entry.ExecMode() != ExecGRPC {
		t.Fatalf("enable did not register gRPC entry: ok=%v entry=%+v", ok, entry)
	}
	if entry.BinPath() == "" {
		t.Fatal("registered entry has no binary path")
	}
	if st, _ := mgr.Get(m.ID); !st.Enabled {
		t.Fatal("state not marked enabled")
	}

	// disable
	if err := mgr.Disable(m.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := reg.Get(m.ID); ok {
		t.Fatal("plugin still registered after disable")
	}
	if st, _ := mgr.Get(m.ID); st.Enabled {
		t.Fatal("state still enabled after disable")
	}

	// re-enable so update re-registration path is exercised
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("re-Enable: %v", err)
	}

	// update to 1.1.0
	m2 := validManifest()
	m2.Version = "1.1.0"
	pkg2 := buildSignedPackage(t, m2, priv)
	if _, err := mgr.Update(pkg2); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st, _ = mgr.Get(m.ID)
	if st.ActiveVersion != "1.1.0" || st.PreviousVersion != "1.0.0" {
		t.Fatalf("post-update state wrong: %+v", st)
	}
	if len(st.Versions) != 2 {
		t.Fatalf("expected 2 versions on disk, got %v", st.Versions)
	}
	// enabled entry now points at the new version's binary
	entry, _ = reg.Get(m.ID)
	if want := mgr.versionDir(m.ID, "1.1.0"); filepath.Dir(entry.BinPath()) != want {
		t.Fatalf("entry binary not on updated version: %s want under %s", entry.BinPath(), want)
	}

	// rollback to 1.0.0
	if err := mgr.Rollback(m.ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	st, _ = mgr.Get(m.ID)
	if st.ActiveVersion != "1.0.0" || st.PreviousVersion != "1.1.0" {
		t.Fatalf("post-rollback state wrong: %+v", st)
	}
	entry, _ = reg.Get(m.ID)
	if want := mgr.versionDir(m.ID, "1.0.0"); filepath.Dir(entry.BinPath()) != want {
		t.Fatalf("rollback did not re-point binary: %s", entry.BinPath())
	}

	// uninstall
	if err := mgr.Uninstall(m.ID); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, ok := mgr.Get(m.ID); ok {
		t.Fatal("state remains after uninstall")
	}
	if _, ok := reg.Get(m.ID); ok {
		t.Fatal("registry entry remains after uninstall")
	}
	if _, err := os.Stat(filepath.Join(mgr.dir, m.ID)); !os.IsNotExist(err) {
		t.Fatalf("plugin dir remains after uninstall: %v", err)
	}
}

func TestInstallRejectsUnsigned(t *testing.T) {
	pub, _ := genKey(t)
	_, otherPriv := genKey(t) // a different key than the manager trusts
	mgr, _ := newTestManager(t, pub)

	m := validManifest()
	pkg := buildSignedPackage(t, m, otherPriv) // signed, but not by a trusted key
	if _, err := mgr.Install(pkg); err == nil {
		t.Fatal("expected install to reject package signed by untrusted key")
	}
	if _, ok := mgr.Get(m.ID); ok {
		t.Fatal("rejected package left state behind")
	}
}

func TestInstallRejectsIncompatibleCoreAPI(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m := validManifest()
	m.CoreAPI = "^4.0.0" // incompatible with host CoreAPIVersion 0.1.0
	pkg := buildSignedPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err == nil {
		t.Fatal("expected install to reject incompatible core_api")
	}
}

func TestInstallRejectsTamperedAfterSign(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	dir := writePackageDir(t, validManifest())
	if err := SignDir(dir, priv); err != nil {
		t.Fatal(err)
	}
	// tamper post-signature
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "pkg.tar.gz")
	tarGzDir(t, dir, out)
	if _, err := mgr.Install(out); err == nil {
		t.Fatal("expected install to reject tampered package")
	}
}

// TestLoadEnabledRegistersEnabledPlugins reproduces a server restart: a fresh
// Manager+Registry built over a plugins dir whose state.json already records
// enabled plugins (the DefaultManager lazy-init path — construction alone does
// NOT touch the Registry, only Enable does). Before the fix this left restarted
// plugin-backed storages failing with "no driver named"; LoadEnabled must
// re-register every enabled plugin, skip disabled ones, and skip (not panic on)
// a broken entry while still registering the healthy ones.
func TestLoadEnabledRegistersEnabledPlugins(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")

	// First "process": install+enable a healthy plugin, install (but leave
	// disabled) a second, and install+enable a third that will be broken by
	// deleting its version dir out from under the persisted state — simulating
	// a corrupted/removed install that must not take the others down.
	reg1 := NewRegistry()
	mgr1, err := NewManager(dir, reg1, NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}

	healthy := validManifest()
	healthy.ID = "com.example.healthy"
	if _, err := mgr1.Install(buildSignedPackage(t, healthy, priv)); err != nil {
		t.Fatalf("install healthy: %v", err)
	}
	if err := mgr1.Enable(healthy.ID); err != nil {
		t.Fatalf("enable healthy: %v", err)
	}

	disabled := validManifest()
	disabled.ID = "com.example.disabled"
	if _, err := mgr1.Install(buildSignedPackage(t, disabled, priv)); err != nil {
		t.Fatalf("install disabled: %v", err)
	}
	// left disabled deliberately

	broken := validManifest()
	broken.ID = "com.example.broken"
	if _, err := mgr1.Install(buildSignedPackage(t, broken, priv)); err != nil {
		t.Fatalf("install broken: %v", err)
	}
	if err := mgr1.Enable(broken.ID); err != nil {
		t.Fatalf("enable broken: %v", err)
	}
	// Corrupt it after enabling: remove its on-disk version dir so a later
	// re-registration attempt (LoadManifest) fails, while state.json still says
	// enabled=true — the "binary missing" restart scenario.
	if err := os.RemoveAll(mgr1.versionDir(broken.ID, broken.Version)); err != nil {
		t.Fatalf("remove broken version dir: %v", err)
	}

	// Second "process": a fresh Manager + fresh Registry over the same dir,
	// exactly what DefaultManager's lazy init produces on restart.
	reg2 := NewRegistry()
	mgr2, err := NewManager(dir, reg2, NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}

	// Reproduce the bug: construction alone must not have registered anything.
	if _, ok := reg2.Get(healthy.ID); ok {
		t.Fatal("healthy plugin already registered before LoadEnabled — test setup invalid")
	}

	err = mgr2.LoadEnabled()

	// The healthy plugin must be registered and enabled.
	entry, ok := reg2.Get(healthy.ID)
	if !ok || !entry.Enabled() || entry.ExecMode() != ExecGRPC {
		t.Fatalf("LoadEnabled did not register healthy plugin: ok=%v entry=%+v", ok, entry)
	}
	if entry.BinPath() == "" {
		t.Fatal("registered entry has no binary path")
	}

	// The disabled plugin must stay unregistered.
	if _, ok := reg2.Get(disabled.ID); ok {
		t.Fatal("disabled plugin was registered by LoadEnabled")
	}

	// The broken plugin must not be registered either, but must not have taken
	// down the healthy plugin's registration or panicked LoadEnabled.
	if _, ok := reg2.Get(broken.ID); ok {
		t.Fatal("broken plugin should not have been registered")
	}
	if err == nil {
		t.Fatal("expected LoadEnabled to report the broken plugin's failure")
	}
	if !strings.Contains(err.Error(), broken.ID) {
		t.Fatalf("LoadEnabled error does not mention broken plugin id: %v", err)
	}
}

// TestSetGrant_NetworkOverridePersists exercises the allowExtraNetwork axis end
// to end through the real Install/SetGrant path: a submitted grant asking for a
// network host the manifest never declared is dropped unless the caller passes
// allowExtraNetwork=true (the server layer's stand-in for an admin's explicit
// risk acknowledgement), and the outcome is what Get reads back.
func TestSetGrant_NetworkOverridePersists(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m := manifestWithPerms("1.0.0", ext.Permissions{Network: []string{"a.com"}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	id := m.ID

	// Not acknowledged: the extra network host is clamped away.
	if err := mgr.SetGrant(id, &Grant{Network: []string{"a.com", "b.com"}}, false); err != nil {
		t.Fatalf("SetGrant(false): %v", err)
	}
	ps, _ := mgr.Get(id)
	if containsStr(ps.Grant.Network, "b.com") {
		t.Fatal("SetGrant(false) must not persist extra network host")
	}

	// Acknowledged: the extra network host is kept.
	if err := mgr.SetGrant(id, &Grant{Network: []string{"a.com", "b.com"}}, true); err != nil {
		t.Fatalf("SetGrant(true): %v", err)
	}
	ps, _ = mgr.Get(id)
	if !containsStr(ps.Grant.Network, "b.com") {
		t.Fatal("SetGrant(true) must persist extra network host")
	}
}

// TestManager_ConfigRoundTripAndPersist covers the Stage 1 config_form
// persistence surface: GetConfig/SetConfig round-trip, nil before any save,
// persistence across a fresh Manager over the same dir (a restart), rejection
// of invalid JSON, and an error for an unknown plugin id.
func TestManager_ConfigRoundTripAndPersist(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	mgr, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}

	m := validManifest()
	pkg := buildSignedPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	id := m.ID

	// Not set yet -> nil.
	got, err := mgr.GetConfig(id)
	if err != nil {
		t.Fatalf("GetConfig before set: %v", err)
	}
	if got != nil {
		t.Fatalf("GetConfig before set = %s, want nil", got)
	}

	// Set + get round-trip.
	if err := mgr.SetConfig(id, json.RawMessage(`{"greeting":"hi"}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	got, err = mgr.GetConfig(id)
	if err != nil {
		t.Fatalf("GetConfig after set: %v", err)
	}
	if string(got) != `{"greeting":"hi"}` {
		t.Fatalf("GetConfig = %s, want the set value", got)
	}

	// Persist: a fresh Manager over the same dir (NewManager loads state in its
	// constructor) reads the saved value back, simulating a restart.
	mgr2, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatalf("NewManager (reload): %v", err)
	}
	got2, err := mgr2.GetConfig(id)
	if err != nil {
		t.Fatalf("GetConfig after reload: %v", err)
	}
	// persistState round-trips through json.MarshalIndent, which reformats (but
	// does not change the value of) the embedded RawMessage, so compare after
	// compacting rather than byte-for-byte.
	var gotVal, wantVal map[string]any
	if err := json.Unmarshal(got2, &gotVal); err != nil {
		t.Fatalf("reloaded config is not valid JSON: %v (%s)", err, got2)
	}
	if err := json.Unmarshal([]byte(`{"greeting":"hi"}`), &wantVal); err != nil {
		t.Fatal(err)
	}
	if gotVal["greeting"] != wantVal["greeting"] {
		t.Fatalf("after reload GetConfig = %s, want persisted value", got2)
	}

	// Invalid JSON rejected.
	if err := mgr.SetConfig(id, json.RawMessage(`[}`)); err == nil {
		t.Fatal("SetConfig with invalid JSON must error")
	}

	// Unknown id.
	if _, err := mgr.GetConfig("nope.nope"); err == nil {
		t.Fatal("GetConfig unknown id must error")
	}
	if err := mgr.SetConfig("nope.nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("SetConfig unknown id must error")
	}
}

// TestPluginState_NameFromManifest covers the manifest i18n display name
// (design: UI shows this instead of the bare plugin id): Install must copy
// the manifest's LangMap Name onto the persisted PluginState, and — for
// plugins installed before the field existed (state.json predating this
// change, simulated here by clearing it directly) — registerActive (driven
// by Enable) must backfill it from the freshly loaded manifest.
func TestPluginState_NameFromManifest(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m := validManifest()
	m.Name = map[string]string{"en": "Demo Name"}
	pkg := buildSignedPackage(t, m, priv)

	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	id := m.ID

	ps, ok := mgr.Get(id)
	if !ok || ps.Name["en"] != "Demo Name" {
		t.Fatalf("PluginState.Name = %v, want en=Demo Name", ps.Name)
	}

	// Backfill: simulate a state.json persisted before Name existed by
	// clearing it directly, then Enable (which calls registerActive with the
	// freshly loaded manifest) must repopulate it.
	mgr.state.Plugins[id].Name = nil
	if err := mgr.Enable(id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	ps, ok = mgr.Get(id)
	if !ok || ps.Name["en"] != "Demo Name" {
		t.Fatalf("post-backfill PluginState.Name = %v, want en=Demo Name", ps.Name)
	}
}

// bundleManifest builds a valid, signable manifest declaring Kind=FeatureBundle
// alongside Kind=EventHook (the F1 wiring point exercised below), granted the
// "events" host_api so DispatchEvent's grant gate does not skip it.
func bundleManifest(id, version string) ext.Manifest {
	m := validManifest()
	m.ID = id
	m.Version = version
	m.Kinds = []ext.Kind{ext.KindFeatureBundle, ext.KindEventHook}
	m.Permissions = ext.Permissions{HostAPIs: []string{CapEvents}}
	return m
}

// fakeGRPCEventHook counts OnEvent deliveries, installed via
// SetEventHookFactory to stand in for a real gRPC subprocess client.
type fakeGRPCEventHook struct{ calls *int }

func (h fakeGRPCEventHook) Manifest() ext.Manifest               { return ext.Manifest{} }
func (h fakeGRPCEventHook) Init(context.Context, ext.Host) error { return nil }
func (h fakeGRPCEventHook) Shutdown(context.Context) error       { return nil }
func (h fakeGRPCEventHook) OnEvent(context.Context, ext.Event) error {
	*h.calls++
	return nil
}

// installFakeEventHookFactory wires SetEventHookFactory to a fresh counter and
// returns it plus a cleanup that clears the factory. It t.Cleanup's the clear.
func installFakeEventHookFactory(t *testing.T) *int {
	t.Helper()
	calls := 0
	SetEventHookFactory(func(e *Entry) (ext.EventHook, func(), error) {
		return fakeGRPCEventHook{calls: &calls}, func() {}, nil
	})
	t.Cleanup(func() { SetEventHookFactory(nil) })
	return &calls
}

// TestManagerEnableExpandsFeatureBundle is the disk-plugin (Manager) half of
// the F2c gRPC FeatureBundle property (registry-level coverage lives in
// featurebundle_test.go): Enable must expand a Kind=FeatureBundle plugin's
// contributed EventHook onto the registry, and DispatchEvent must actually
// reach it through the gRPC factory seam — before this wiring, Enable never
// called ExpandFeatureBundle at all, so a disk-installed bundle's EventHook/
// APIRoute/ScheduledJob/FileDecorator legs were permanently unreachable.
func TestManagerEnableExpandsFeatureBundle(t *testing.T) {
	pub, priv := genKey(t)
	mgr, reg := newTestManager(t, pub)
	calls := installFakeEventHookFactory(t)

	m := bundleManifest("com.example.bundle", "1.0.0")
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	entry, ok := reg.Get(m.ID)
	if !ok || !entry.HasKind(ext.KindEventHook) {
		t.Fatalf("Enable did not expand the feature bundle onto EventHook: ok=%v entry=%+v", ok, entry)
	}

	DispatchEvent(context.Background(), reg, ext.Event{Type: ext.EventFSMove})
	if *calls != 1 {
		t.Fatalf("DispatchEvent after Enable: calls = %d, want 1", *calls)
	}
}

// TestManagerDisableCollapsesFeatureBundle proves the reverse leg: after
// Disable, the bundle's EventHook is no longer reachable (the entry — and
// with it every extraKinds index ExpandFeatureBundle added — is gone), and
// re-Enabling restores it.
func TestManagerDisableCollapsesFeatureBundle(t *testing.T) {
	pub, priv := genKey(t)
	mgr, reg := newTestManager(t, pub)
	calls := installFakeEventHookFactory(t)

	m := bundleManifest("com.example.bundle.disable", "1.0.0")
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, ok := reg.Get(m.ID); !ok {
		t.Fatal("setup: plugin not registered after Enable")
	}

	if err := mgr.Disable(m.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := reg.Get(m.ID); ok {
		t.Fatal("plugin still registered after Disable")
	}
	if len(reg.ByKind(ext.KindEventHook)) != 0 {
		t.Fatal("disabled bundle's EventHook must not be reachable via ByKind")
	}
	DispatchEvent(context.Background(), reg, ext.Event{Type: ext.EventFSMove})
	if *calls != 0 {
		t.Fatalf("DispatchEvent after Disable: calls = %d, want 0 (disabled bundle unreachable)", *calls)
	}

	// Re-Enable restores reachability.
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("re-Enable: %v", err)
	}
	entry, ok := reg.Get(m.ID)
	if !ok || !entry.HasKind(ext.KindEventHook) {
		t.Fatal("re-Enable did not re-expand the feature bundle")
	}
	DispatchEvent(context.Background(), reg, ext.Event{Type: ext.EventFSMove})
	if *calls != 1 {
		t.Fatalf("DispatchEvent after re-Enable: calls = %d, want 1", *calls)
	}
}

// TestManagerLoadEnabledReexpandsFeatureBundle reproduces a server restart
// (mirrors TestLoadEnabledRegistersEnabledPlugins one Kind over): a
// FeatureBundle plugin installed+enabled in "process 1" must have its
// contributed EventHook reachable again after "process 2" (a fresh
// Manager+Registry over the same state dir) calls LoadEnabled — construction
// alone must not do this; the restart-time re-registration inside LoadEnabled
// must re-run ExpandFeatureBundle exactly as Enable does, or the bundle's
// sub-Kinds go silently unreachable until an admin happens to toggle it.
func TestManagerLoadEnabledReexpandsFeatureBundle(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")

	reg1 := NewRegistry()
	mgr1, err := NewManager(dir, reg1, NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := bundleManifest("com.example.bundle.restart", "1.0.0")
	if _, err := mgr1.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr1.Enable(m.ID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if entry, ok := reg1.Get(m.ID); !ok || !entry.HasKind(ext.KindEventHook) {
		t.Fatal("setup: bundle not expanded in process 1")
	}

	// "Process 2": fresh Manager + fresh Registry over the same dir.
	calls := installFakeEventHookFactory(t)
	reg2 := NewRegistry()
	mgr2, err := NewManager(dir, reg2, NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg2.Get(m.ID); ok {
		t.Fatal("bundle already registered before LoadEnabled — test setup invalid")
	}

	if err := mgr2.LoadEnabled(); err != nil {
		t.Fatalf("LoadEnabled: %v", err)
	}
	entry, ok := reg2.Get(m.ID)
	if !ok || !entry.HasKind(ext.KindEventHook) {
		t.Fatalf("LoadEnabled did not re-expand the feature bundle: ok=%v entry=%+v", ok, entry)
	}
	DispatchEvent(context.Background(), reg2, ext.Event{Type: ext.EventFSMove})
	if *calls != 1 {
		t.Fatalf("DispatchEvent after restart: calls = %d, want 1", *calls)
	}
}

// TestManagerUpdateReexpandsFeatureBundle covers Update's re-registration
// branch: updating an ENABLED bundle plugin to a new version re-registers its
// entry (registerActive: Deregister + fresh RegisterPluginMode, which drops
// the prior expansion's extraKinds), so the new entry must be re-expanded too
// or the bundle's EventHook goes unreachable immediately after every update.
func TestManagerUpdateReexpandsFeatureBundle(t *testing.T) {
	pub, priv := genKey(t)
	mgr, reg := newTestManager(t, pub)

	m1 := bundleManifest("com.example.bundle.update", "1.0.0")
	if _, err := mgr.Install(buildSignedPackage(t, m1, priv)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Enable(m1.ID); err != nil {
		t.Fatalf("enable: %v", err)
	}

	m2 := bundleManifest("com.example.bundle.update", "1.1.0")
	if _, err := mgr.Update(buildSignedPackage(t, m2, priv)); err != nil {
		t.Fatalf("update: %v", err)
	}

	entry, ok := reg.Get(m1.ID)
	if !ok || !entry.HasKind(ext.KindEventHook) {
		t.Fatalf("Update did not re-expand the feature bundle: ok=%v entry=%+v", ok, entry)
	}
}

// TestInstallRejectsReservedPublicPrefix covers the STATELESS half of the
// public_prefix conflict check (CheckReservedPrefix) wired into gate(), which
// installVersion calls: a manifest requesting a core-owned top-level prefix
// ("api") must be refused at INSTALL time, not only when an admin later
// enables it — closing the gap CheckReservedPrefix had (defined but never
// called in production) before this wave.
func TestInstallRejectsReservedPublicPrefix(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m := validManifest()
	m.Contributions.PublicPrefix = "/api"
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err == nil {
		t.Fatal("expected install to reject a reserved public_prefix")
	}
	if _, ok := mgr.Get(m.ID); ok {
		t.Fatal("rejected install left state behind")
	}
}

func TestStatePersistsAcrossManagers(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	reg := NewRegistry()
	mgr, err := NewManager(dir, reg, NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := validManifest()
	pkg := buildSignedPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatal(err)
	}
	// New manager over the same dir sees the installed plugin.
	mgr2, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr2.Get(m.ID); !ok {
		t.Fatal("state did not persist across manager instances")
	}
}
