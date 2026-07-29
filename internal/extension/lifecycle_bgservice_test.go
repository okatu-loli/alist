package extension

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// TestManagerDriveBackgroundServiceLifecycle pins the fix for the W1-W3 gap the
// Wave 4 verification surfaced: a Kind=BackgroundService DISK plugin's supervised
// Run (which, for the external FRP plugin, spawns the frpc subprocess) must be
// started when the admin enables it and stopped when they disable/uninstall it.
//
// Before the fix, Manager.Enable only registered the entry in the Registry and
// never called the ServiceSupervisor, so the service never ran (Status stayed
// "stopped", no subprocess) — the external FRP plugin could be installed+enabled
// but never actually started its tunnel.
//
// The stub in-process service short-circuits resolveBackgroundService (it is
// registered under the plugin id) so this exercises the Manager↔supervisor
// wiring without launching a real gRPC subprocess.
func TestManagerDriveBackgroundServiceLifecycle(t *testing.T) {
	pub, priv := genKey(t)
	mgr, reg := newTestManager(t, pub)
	sup := NewServiceSupervisor(reg)
	sup.Start(context.Background()) // records rootCtx; no services yet
	mgr.SetSupervisor(sup)

	m := validManifest()
	m.Version = "1.0.0"
	m.Kinds = []ext.Kind{ext.KindBackgroundService}
	pkg := buildSignedPackage(t, m, priv)

	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	stub := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance(m.ID, stub)
	t.Cleanup(func() { RegisterBackgroundServiceInstance(m.ID, nil) })

	// Enable must start the supervised Run.
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	waitServiceStart(t, stub)
	if got := atomic.LoadInt32(&stub.runs); got != 1 {
		t.Fatalf("after enable: runs=%d want 1", got)
	}
	waitServiceState(t, reg, m.ID, ext.ServiceStateRunning)

	// Disable must stop it (Run's ctx cancelled → returns → stopped).
	if err := mgr.Disable(m.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	// After disable the entry is deregistered, so Status errors; assert the stub
	// itself observed the stop instead.
	deadlineState := func(want string) bool {
		st, _ := stub.Status(context.Background())
		return st.State == want
	}
	waitFor(t, func() bool { return deadlineState(ext.ServiceStateStopped) }, "stub did not reach stopped after disable")

	// Re-enable brings it back up with a fresh Run.
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("re-Enable: %v", err)
	}
	waitServiceStart(t, stub)
	if got := atomic.LoadInt32(&stub.runs); got != 2 {
		t.Fatalf("after re-enable: runs=%d want 2", got)
	}

	// Uninstall must also stop the supervised service before removing it.
	if err := mgr.Uninstall(m.ID); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	waitFor(t, func() bool { return deadlineState(ext.ServiceStateStopped) }, "stub did not reach stopped after uninstall")
}

// TestManagerDriveBackgroundServiceLifecycleMultiKindManifest mirrors
// TestManagerDriveBackgroundServiceLifecycle but for a plugin whose manifest
// declares BackgroundService alongside another, non-BackgroundService
// PRIMARY Kind (Kinds: [StorageDriver, BackgroundService], so registerActive
// indexes the entry under Kind=StorageDriver — RegisterPluginMode always
// picks Kinds[0]). isBackgroundService must still read the Manifest's
// declared Kinds (DeclaresKind), not Entry.HasKind: HasKind only sees the
// entry's primary Kind plus extraKinds, and extraKinds is populated solely
// by FeatureBundle indexing, so it would wrongly report false here and
// Enable/Disable would silently never start/stop the supervised Run despite
// the plugin having declared the service.
func TestManagerDriveBackgroundServiceLifecycleMultiKindManifest(t *testing.T) {
	pub, priv := genKey(t)
	mgr, reg := newTestManager(t, pub)
	sup := NewServiceSupervisor(reg)
	sup.Start(context.Background())
	mgr.SetSupervisor(sup)

	m := validManifest()
	m.Version = "1.0.0"
	m.Kinds = []ext.Kind{ext.KindStorageDriver, ext.KindBackgroundService}
	pkg := buildSignedPackage(t, m, priv)

	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	stub := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance(m.ID, stub)
	t.Cleanup(func() { RegisterBackgroundServiceInstance(m.ID, nil) })

	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	waitServiceStart(t, stub)
	if got := atomic.LoadInt32(&stub.runs); got != 1 {
		t.Fatalf("after enable: runs=%d want 1 (StartService must have been called despite the non-BackgroundService primary Kind)", got)
	}
	waitServiceState(t, reg, m.ID, ext.ServiceStateRunning)

	if err := mgr.Disable(m.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	waitFor(t, func() bool {
		st, _ := stub.Status(context.Background())
		return st.State == ext.ServiceStateStopped
	}, "stub did not reach stopped after disable (StopService must have been called)")
}

// waitFor polls cond up to the shared 2s test deadline used by the other
// bgservice helpers, failing with msg on timeout.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
