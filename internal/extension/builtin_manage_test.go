package extension

import (
	"context"
	"sort"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// fakeDisabledStore is an in-memory DisabledStore for the manager tests. It
// records the persisted id set so a test can assert enable/disable round-trips
// through persistence without a DB.
type fakeDisabledStore struct {
	ids  []string
	load func() ([]string, error) // optional error injection
}

func (f *fakeDisabledStore) Load() ([]string, error) {
	if f.load != nil {
		return f.load()
	}
	return append([]string(nil), f.ids...), nil
}

func (f *fakeDisabledStore) Save(ids []string) error {
	f.ids = append([]string(nil), ids...)
	return nil
}

func (f *fakeDisabledStore) has(id string) bool {
	for _, x := range f.ids {
		if x == id {
			return true
		}
	}
	return false
}

func aggregatedHas(r *Registry, id string) bool {
	for _, e := range AggregateContributions(r).Extensions {
		if e.ID == id {
			return true
		}
	}
	return false
}

// TestDisableEnableBuiltinBackgroundService: disabling a built-in
// BackgroundService flips its entry off, stops the supervised service, records
// the id in the persisted set, and drops it from the aggregated contributions.
// Enabling reverses every leg.
func TestDisableEnableBuiltinBackgroundService(t *testing.T) {
	r := NewRegistry()
	svc := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	if err := RegisterBuiltinBackgroundService(r, "builtin.frp", svc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RegisterBackgroundServiceInstance("builtin.frp", nil) })

	sup := NewServiceSupervisor(r)
	sup.RestartBackoff = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)
	waitServiceStart(t, svc)
	waitServiceState(t, r, "builtin.frp", ext.ServiceStateRunning)

	store := &fakeDisabledStore{}
	m := NewBuiltinManager(r, sup, store)

	// Disable.
	if err := m.DisableBuiltin("builtin.frp"); err != nil {
		t.Fatalf("DisableBuiltin: %v", err)
	}
	if e, _ := r.Get("builtin.frp"); e.Enabled() {
		t.Fatal("entry still enabled after DisableBuiltin")
	}
	if !store.has("builtin.frp") {
		t.Fatalf("disabled id not persisted: %v", store.ids)
	}
	if aggregatedHas(r, "builtin.frp") {
		t.Fatal("disabled built-in still in aggregated contributions")
	}
	// StopService cancelled the run; the service reports stopped.
	waitServiceState2(t, r, "builtin.frp", ext.ServiceStateStopped)

	// Enable.
	if err := m.EnableBuiltin("builtin.frp"); err != nil {
		t.Fatalf("EnableBuiltin: %v", err)
	}
	if e, _ := r.Get("builtin.frp"); !e.Enabled() {
		t.Fatal("entry still disabled after EnableBuiltin")
	}
	if store.has("builtin.frp") {
		t.Fatalf("enabled id not removed from persisted set: %v", store.ids)
	}
	if !aggregatedHas(r, "builtin.frp") {
		t.Fatal("re-enabled built-in missing from aggregated contributions")
	}
	waitServiceState(t, r, "builtin.frp", ext.ServiceStateRunning)

	cancel()
	sup.Wait()
}

// waitServiceState2 polls the raw stub state (ServiceStatusOf refuses a disabled
// entry, so it can't be used to observe a service that was just disabled).
func waitServiceState2(t *testing.T, r *Registry, id, want string) {
	t.Helper()
	e, _ := r.Get(id)
	svc, _ := resolveBackgroundService(e)
	ss, ok := svc.(*stubService)
	if !ok {
		t.Fatalf("service %s is not a stubService", id)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := ss.Status(context.Background())
		if st.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := ss.Status(context.Background())
	t.Fatalf("service %s: want state %q, got %q", id, want, st.State)
}

// TestDisableEnableBuiltinFeatureBundle: disabling a built-in FeatureBundle
// (labels-shaped) flips it off, drops its contributions, and records the id —
// with no supervisor involved. Enabling restores the contributions.
func TestDisableEnableBuiltinFeatureBundle(t *testing.T) {
	r := NewRegistry()
	b := &decoratingBundle{id: "builtin.labels"}
	if err := RegisterBuiltinFeatureBundle(r, b); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CollapseFeatureBundle(b.id) })

	store := &fakeDisabledStore{}
	// A nil supervisor proves a FeatureBundle disable never touches one.
	m := NewBuiltinManager(r, nil, store)

	if !aggregatedHas(r, "builtin.labels") {
		t.Fatal("labels bundle missing from contributions before disable")
	}
	if err := m.DisableBuiltin("builtin.labels"); err != nil {
		t.Fatalf("DisableBuiltin: %v", err)
	}
	if e, _ := r.Get("builtin.labels"); e.Enabled() {
		t.Fatal("bundle still enabled")
	}
	if !store.has("builtin.labels") {
		t.Fatalf("disabled id not persisted: %v", store.ids)
	}
	if aggregatedHas(r, "builtin.labels") {
		t.Fatal("disabled bundle still contributes")
	}

	if err := m.EnableBuiltin("builtin.labels"); err != nil {
		t.Fatalf("EnableBuiltin: %v", err)
	}
	if store.has("builtin.labels") {
		t.Fatalf("enabled id not removed: %v", store.ids)
	}
	if !aggregatedHas(r, "builtin.labels") {
		t.Fatal("re-enabled bundle missing from contributions")
	}
}

// TestDisableEnableBuiltinMultiKindManifestUsesManifestTruthSource mirrors
// TestInvokeActionMultiKindManifestUsesManifestTruthSource: a built-in entry
// registered under a different primary Kind (Kind=FeatureBundle) whose
// Manifest additionally declares BackgroundService (Kinds: [FeatureBundle,
// BackgroundService]) never gets an extraKinds(BackgroundService) entry —
// BackgroundService has no FeatureBundle expansion path — so
// DisableBuiltin/EnableBuiltin's isBg check, if it read Entry.HasKind, would
// wrongly skip stopping/starting the supervised service despite the built-in
// having declared it. DeclaresKind must still gate the supervisor calls.
func TestDisableEnableBuiltinMultiKindManifestUsesManifestTruthSource(t *testing.T) {
	r := NewRegistry()
	svc := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
	e := &Entry{
		Manifest: ext.Manifest{ID: "builtin.multikind", Kinds: []ext.Kind{ext.KindFeatureBundle, ext.KindBackgroundService}},
		Kind:     ext.KindFeatureBundle, // registered/indexed under its primary Kind
		enabled:  true,
		builtin:  true,
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterBackgroundServiceInstance("builtin.multikind", svc)
	t.Cleanup(func() { RegisterBackgroundServiceInstance("builtin.multikind", nil) })

	sup := NewServiceSupervisor(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx) // ByKind(BackgroundService) misses it (primary Kind=FeatureBundle); start it directly.
	if err := sup.StartService("builtin.multikind"); err != nil {
		t.Fatal(err)
	}
	waitServiceStart(t, svc)

	store := &fakeDisabledStore{}
	m := NewBuiltinManager(r, sup, store)

	if err := m.DisableBuiltin("builtin.multikind"); err != nil {
		t.Fatalf("DisableBuiltin: %v", err)
	}
	waitServiceState2(t, r, "builtin.multikind", ext.ServiceStateStopped)

	cancel()
	sup.Wait()
}

// TestBuiltinManageRejectsNonBuiltinAndUnknown: a disk plugin id and an unknown
// id are both refused, and the refusal never mutates the persisted set.
func TestBuiltinManageRejectsNonBuiltinAndUnknown(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterPluginMode(
		ext.Manifest{ID: "disk.plugin", Kinds: []ext.Kind{ext.KindStorageDriver}},
		"plugin.bin", ExecGRPC); err != nil {
		t.Fatal(err)
	}
	store := &fakeDisabledStore{}
	m := NewBuiltinManager(r, nil, store)

	for _, id := range []string{"disk.plugin", "does.not.exist"} {
		if err := m.DisableBuiltin(id); err == nil {
			t.Fatalf("DisableBuiltin(%q) should error", id)
		}
		if err := m.EnableBuiltin(id); err == nil {
			t.Fatalf("EnableBuiltin(%q) should error", id)
		}
	}
	if len(store.ids) != 0 {
		t.Fatalf("rejected ops mutated the persisted set: %v", store.ids)
	}
	// The disk plugin must stay enabled (never flipped by the built-in manager).
	if e, _ := r.Get("disk.plugin"); !e.Enabled() {
		t.Fatal("disk plugin was disabled by a rejected built-in op")
	}
}

// TestApplyDisabledBuiltins: the persisted set is re-applied after registration
// — every listed built-in is disabled, a non-built-in id in the set is ignored.
func TestApplyDisabledBuiltins(t *testing.T) {
	r := NewRegistry()
	b := &decoratingBundle{id: "builtin.labels"}
	if err := RegisterBuiltinFeatureBundle(r, b); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CollapseFeatureBundle(b.id) })
	if err := r.RegisterPluginMode(
		ext.Manifest{ID: "disk.plugin", Kinds: []ext.Kind{ext.KindStorageDriver}},
		"plugin.bin", ExecGRPC); err != nil {
		t.Fatal(err)
	}

	store := &fakeDisabledStore{ids: []string{"builtin.labels", "disk.plugin", "ghost"}}
	m := NewBuiltinManager(r, nil, store)
	if err := m.ApplyDisabledBuiltins(); err != nil {
		t.Fatalf("ApplyDisabledBuiltins: %v", err)
	}

	if e, _ := r.Get("builtin.labels"); e.Enabled() {
		t.Fatal("built-in labels not disabled by ApplyDisabledBuiltins")
	}
	// A non-built-in id in the set must not disable the disk plugin.
	if e, _ := r.Get("disk.plugin"); !e.Enabled() {
		t.Fatal("ApplyDisabledBuiltins disabled a non-built-in")
	}
}

// TestPersistDisabledDedupAndSort: the persisted set is deduped and id-sorted,
// so repeated disables and a mixed order round-trip to a stable JSON array.
func TestPersistDisabledDedupAndSort(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"builtin.b", "builtin.a"} {
		svc := &stubService{state: ext.ServiceStateStopped, started: make(chan struct{}, 1)}
		if err := RegisterBuiltinBackgroundService(r, id, svc); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { RegisterBackgroundServiceInstance(id, nil) })
	}
	store := &fakeDisabledStore{}
	m := NewBuiltinManager(r, nil, store)

	if err := m.DisableBuiltin("builtin.b"); err != nil {
		t.Fatal(err)
	}
	if err := m.DisableBuiltin("builtin.b"); err != nil { // repeat is a no-op on the set
		t.Fatal(err)
	}
	if err := m.DisableBuiltin("builtin.a"); err != nil {
		t.Fatal(err)
	}
	want := []string{"builtin.a", "builtin.b"}
	got := append([]string(nil), store.ids...)
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("persisted set = %v, want %v (deduped + sorted)", got, want)
	}
}
