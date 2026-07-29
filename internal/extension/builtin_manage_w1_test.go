package extension

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// TestEntryBuiltinFlag: the two RegisterBuiltin* helpers mark their entry
// builtin==true; a plain plugin registration does not. BuiltinEntries lists
// every built-in (including a disabled one) and excludes disk plugins.
func TestEntryBuiltinFlag(t *testing.T) {
	r := NewRegistry()

	bg := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	if err := RegisterBuiltinBackgroundService(r, "builtin.frp", bg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RegisterBackgroundServiceInstance("builtin.frp", nil) })

	fb := &decoratingBundle{id: "builtin.labels"}
	if err := RegisterBuiltinFeatureBundle(r, fb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CollapseFeatureBundle(fb.id) })

	if err := r.RegisterPluginMode(
		ext.Manifest{ID: "disk.plugin", Kinds: []ext.Kind{ext.KindStorageDriver}},
		"plugin.bin", ExecGRPC); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]bool{
		"builtin.frp":    true,
		"builtin.labels": true,
		"disk.plugin":    false,
	} {
		e, ok := r.Get(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		if e.Builtin() != want {
			t.Fatalf("%s Builtin()=%v want %v", id, e.Builtin(), want)
		}
	}

	// Disable one built-in: it must still show up in BuiltinEntries (the list
	// page lists disabled built-ins so they can be re-enabled) but drop out of
	// EnabledEntries.
	if err := r.Disable("builtin.frp"); err != nil {
		t.Fatal(err)
	}
	got := r.BuiltinEntries()
	if len(got) != 2 {
		t.Fatalf("BuiltinEntries len=%d want 2: %+v", len(got), got)
	}
	if got[0].Manifest.ID != "builtin.frp" || got[1].Manifest.ID != "builtin.labels" {
		t.Fatalf("BuiltinEntries ids/order wrong: %q %q", got[0].Manifest.ID, got[1].Manifest.ID)
	}
	for _, e := range r.EnabledEntries() {
		if e.Manifest.ID == "builtin.frp" {
			t.Fatal("disabled built-in leaked into EnabledEntries")
		}
	}
}

func waitServiceStart(t *testing.T, svc *stubService) {
	t.Helper()
	select {
	case <-svc.started:
	case <-time.After(2 * time.Second):
		t.Fatal("service Run did not start")
	}
}

func waitServiceState(t *testing.T, r *Registry, id, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := ServiceStatusOf(context.Background(), r, id)
		if err == nil && st.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := ServiceStatusOf(context.Background(), r, id)
	t.Fatalf("service %s: want state %q, got %q", id, want, st.State)
}

// TestSupervisorStartStopService: Start brings the service up; StopService
// cancels it (Run returns → stopped); StartService restarts it; a redundant
// StartService while running is idempotent (no second Run).
func TestSupervisorStartStopService(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bgEntry("svc.frp")); err != nil {
		t.Fatal(err)
	}
	svc := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance("svc.frp", svc)
	t.Cleanup(func() { RegisterBackgroundServiceInstance("svc.frp", nil) })

	sup := NewServiceSupervisor(r)
	sup.RestartBackoff = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)

	waitServiceStart(t, svc)
	waitServiceState(t, r, "svc.frp", ext.ServiceStateRunning)

	// Idempotent: calling StartService while running must not start a 2nd Run.
	before := atomic.LoadInt32(&svc.runs)
	if err := sup.StartService("svc.frp"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&svc.runs); got != before {
		t.Fatalf("StartService not idempotent: runs %d→%d", before, got)
	}

	// StopService → ctx cancelled → Run returns → goroutine exits → stopped.
	sup.StopService("svc.frp")
	waitServiceState(t, r, "svc.frp", ext.ServiceStateStopped)

	// StartService brings it back up.
	if err := sup.StartService("svc.frp"); err != nil {
		t.Fatal(err)
	}
	waitServiceStart(t, svc)
	waitServiceState(t, r, "svc.frp", ext.ServiceStateRunning)

	cancel()
	sup.Wait()
}

// TestSupervisorConcurrentStartStop: hammer StartService/StopService/Status
// concurrently. -race must stay clean, nothing panics, and after a clean
// shutdown the running map holds no leaked handles.
func TestSupervisorConcurrentStartStop(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bgEntry("svc.race")); err != nil {
		t.Fatal(err)
	}
	svc := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	RegisterBackgroundServiceInstance("svc.race", svc)
	t.Cleanup(func() { RegisterBackgroundServiceInstance("svc.race", nil) })

	sup := NewServiceSupervisor(r)
	sup.RestartBackoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = sup.StartService("svc.race")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				sup.StopService("svc.race")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = ServiceStatusOf(context.Background(), r, "svc.race")
			}
		}()
	}
	wg.Wait()

	cancel()
	sup.Wait()

	sup.mu.Lock()
	n := len(sup.running)
	sup.mu.Unlock()
	if n != 0 {
		t.Fatalf("running map leaked %d entries after shutdown", n)
	}
}
