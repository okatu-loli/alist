package search

import (
	"context"
	"errors"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// baseFakeIndexer is a SearchIndexer double that records that its methods were
// reached, so the base's delegation can be observed.
type baseFakeIndexer struct {
	name     string
	searched bool
	indexed  bool
	batched  bool
	deleted  bool
	cleared  bool
}

func (f *baseFakeIndexer) Manifest() ext.Manifest                     { return ext.Manifest{ID: f.name} }
func (f *baseFakeIndexer) Init(ctx context.Context, h ext.Host) error { return nil }
func (f *baseFakeIndexer) Shutdown(ctx context.Context) error         { return nil }
func (f *baseFakeIndexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: f.name, AutoUpdate: true}
}
func (f *baseFakeIndexer) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	f.searched = true
	return []ext.SearchNode{{Parent: "/x", Name: "hit"}}, 1, nil
}
func (f *baseFakeIndexer) Index(ctx context.Context, node ext.SearchNode) error {
	f.indexed = true
	return nil
}
func (f *baseFakeIndexer) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	f.batched = true
	return nil
}
func (f *baseFakeIndexer) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	return nil, nil
}
func (f *baseFakeIndexer) Del(ctx context.Context, prefix string) error {
	f.deleted = true
	return nil
}
func (f *baseFakeIndexer) Clear(ctx context.Context) error {
	f.cleared = true
	return nil
}

// resetBase clears any active instance so each test starts from the fallback.
func resetBase(t *testing.T) {
	t.Helper()
	if err := Init("none"); err != nil {
		t.Fatalf("Init(none): %v", err)
	}
}

// regBackend registers a fake backend the way a real one is wired (A2): an
// enabled, owning builtin extension entry in DefaultRegistry PLUS the
// searcherName→extensionID mapping and trusted instance via RegisterBackend, so
// ActiveIndexer's enabled-gate resolves it as active. Cleanup deregisters both.
// The extension id is derived from name so each backend owns a distinct entry.
func regBackend(t *testing.T, name string) *baseFakeIndexer {
	t.Helper()
	fake := &baseFakeIndexer{name: name}
	extID := "builtin.search." + name
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    extID,
		Name:  map[string]string{"en": name},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}); err != nil {
		t.Fatalf("register owning extension %q: %v", extID, err)
	}
	RegisterBackend(name, extID, fake)
	t.Cleanup(func() {
		RegisterBackend(name, extID, nil)
		extension.DefaultRegistry.Deregister(extID)
	})
	return fake
}

func TestBaseInitNoneAndRegistryResolve(t *testing.T) {
	resetBase(t)
	if ActiveIndexer() != nil {
		t.Fatal("Init(none) must leave no active indexer")
	}

	// A selected-but-unregistered backend is not an error: base stays on
	// fallback (nil active).
	if err := Init("ghost"); err != nil {
		t.Fatalf("Init(unregistered) returned error: %v", err)
	}
	if ActiveIndexer() != nil {
		t.Fatal("unregistered backend must not become active")
	}

	fake := regBackend(t, "basefake")
	t.Cleanup(func() { resetBase(t) })

	if err := Init("basefake"); err != nil {
		t.Fatalf("Init(basefake): %v", err)
	}
	if ActiveIndexer() != ext.SearchIndexer(fake) {
		t.Fatal("Init(basefake) must adopt the registered instance")
	}
	if Config().Name != "basefake" {
		t.Fatalf("Config().Name = %q, want basefake", Config().Name)
	}
}

func TestBaseInitSwitchReleasesOld(t *testing.T) {
	resetBase(t)
	_ = regBackend(t, "aaa")
	b := regBackend(t, "bbb")
	t.Cleanup(func() { resetBase(t) })

	if err := Init("aaa"); err != nil {
		t.Fatalf("Init(aaa): %v", err)
	}
	if ActiveIndexer().Config().Name != "aaa" {
		t.Fatalf("active = %q, want aaa", ActiveIndexer().Config().Name)
	}
	if err := Init("bbb"); err != nil {
		t.Fatalf("Init(bbb): %v", err)
	}
	if ActiveIndexer() != ext.SearchIndexer(b) {
		t.Fatal("switching backends must release the old and adopt the new")
	}

	// Switching to none releases entirely.
	if err := Init("none"); err != nil {
		t.Fatalf("Init(none): %v", err)
	}
	if ActiveIndexer() != nil {
		t.Fatal("Init(none) must release the active backend")
	}
}

func TestBaseDelegatesToActive(t *testing.T) {
	resetBase(t)
	fake := regBackend(t, "deleg")
	t.Cleanup(func() { resetBase(t) })
	if err := Init("deleg"); err != nil {
		t.Fatalf("Init(deleg): %v", err)
	}
	ctx := context.Background()

	hits, total, err := IndexSearch(ctx, ext.SearchReq{Keywords: "hit"})
	if err != nil || total != 1 || len(hits) != 1 {
		t.Fatalf("IndexSearch = %v, %d, %v", hits, total, err)
	}
	if !fake.searched {
		t.Error("IndexSearch did not reach the active backend")
	}
	if err := Index(ctx, ext.SearchNode{Parent: "/a", Name: "b"}); err != nil || !fake.indexed {
		t.Fatalf("Index delegation failed: err=%v indexed=%v", err, fake.indexed)
	}
	if err := BatchIndex(ctx, nil); err != nil || !fake.batched {
		t.Fatalf("BatchIndex delegation failed: err=%v batched=%v", err, fake.batched)
	}
	if err := Del(ctx, "/a"); err != nil || !fake.deleted {
		t.Fatalf("Del delegation failed: err=%v deleted=%v", err, fake.deleted)
	}
	if err := Clear(ctx); err != nil || !fake.cleared {
		t.Fatalf("Clear delegation failed: err=%v cleared=%v", err, fake.cleared)
	}
}

func TestBaseNilInstanceReturnsSearchNotAvailable(t *testing.T) {
	resetBase(t)
	ctx := context.Background()
	if _, _, err := IndexSearch(ctx, ext.SearchReq{}); !errors.Is(err, errs.SearchNotAvailable) {
		t.Errorf("IndexSearch err = %v, want SearchNotAvailable", err)
	}
	if err := Index(ctx, ext.SearchNode{}); !errors.Is(err, errs.SearchNotAvailable) {
		t.Errorf("Index err = %v, want SearchNotAvailable", err)
	}
	if err := BatchIndex(ctx, nil); !errors.Is(err, errs.SearchNotAvailable) {
		t.Errorf("BatchIndex err = %v, want SearchNotAvailable", err)
	}
	if err := Del(ctx, "/a"); !errors.Is(err, errs.SearchNotAvailable) {
		t.Errorf("Del err = %v, want SearchNotAvailable", err)
	}
	if err := Clear(ctx); !errors.Is(err, errs.SearchNotAvailable) {
		t.Errorf("Clear err = %v, want SearchNotAvailable", err)
	}
	if Config() != (ext.SearchIndexerConfig{}) {
		t.Errorf("Config() with nil instance = %+v, want zero", Config())
	}
}

// TestActiveIndexerEnabledGate proves the A2 enabled-gate: a resolved backend
// is active only while its OWNING extension is enabled. Disabling that extension
// makes ActiveIndexer (and the delegating API) fall back to nil / SearchNotAvailable
// at resolution time, with no manager→search callback; re-enabling restores it.
func TestActiveIndexerEnabledGate(t *testing.T) {
	resetBase(t)
	fake := regBackend(t, "gated")
	t.Cleanup(func() { resetBase(t) })

	if err := Init("gated"); err != nil {
		t.Fatalf("Init(gated): %v", err)
	}
	if ActiveIndexer() != ext.SearchIndexer(fake) {
		t.Fatal("backend must be active while its extension is enabled")
	}

	// Disable the owning extension: gate closes.
	if err := extension.DefaultRegistry.Disable("builtin.search.gated"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if ActiveIndexer() != nil {
		t.Fatal("disabled owning extension must gate ActiveIndexer to nil")
	}
	if _, _, err := IndexSearch(context.Background(), ext.SearchReq{}); !errors.Is(err, errs.SearchNotAvailable) {
		t.Errorf("gated IndexSearch err = %v, want SearchNotAvailable", err)
	}

	// Re-enable: gate opens.
	if err := extension.DefaultRegistry.Enable("builtin.search.gated"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if ActiveIndexer() != ext.SearchIndexer(fake) {
		t.Fatal("re-enabling the owning extension must restore the active backend")
	}
}

// TestActiveIndexerFallsBackToGRPCBackend pins Phase E ring 1b's miss
// fallback: a Kind=SearchIndexer gRPC plugin entry (no RegisterBackend
// mapping, since only built-in backends get one) is still resolved by
// ActiveIndexer as long as its self-reported Config().Name matches the
// search_index mode, through extension.FindSearchIndexerByName. Disabling the
// plugin entry gates it back to nil, the same enabled-gate the built-in path
// already has. Mirrors offline_download's
// TestToolByNameFallsBackToGRPCTool.
func TestActiveIndexerFallsBackToGRPCBackend(t *testing.T) {
	resetBase(t)
	t.Cleanup(func() { resetBase(t) })

	const pluginID = "com.example.grpcindexer"
	const backendName = "grpcindexer"

	if err := extension.DefaultRegistry.RegisterPlugin(ext.Manifest{
		ID:    pluginID,
		Name:  map[string]string{"en": "gRPC Search Indexer"},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(pluginID) })

	stub := &baseFakeIndexer{name: backendName}
	extension.SetSearchIndexerFactory(func(e *extension.Entry) (ext.SearchIndexer, func(), error) {
		return stub, func() {}, nil
	})
	t.Cleanup(func() { extension.SetSearchIndexerFactory(nil) })

	// No in-process instance is registered under backendName (only
	// RegisterBackend built-ins get one), so Init leaves instance nil and
	// records activeMode — the gRPC fallback is what ActiveIndexer must use.
	if err := Init(backendName); err != nil {
		t.Fatalf("Init(%s): %v", backendName, err)
	}
	if ActiveIndexer() != ext.SearchIndexer(stub) {
		t.Fatal("a gRPC-registered backend must resolve by its Config().Name via the miss fallback")
	}

	if err := extension.DefaultRegistry.Disable(pluginID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if ActiveIndexer() != nil {
		t.Fatal("a disabled gRPC indexer plugin must gate ActiveIndexer to nil")
	}
}

// TestSettingHookReinits proves the search_index setting hook is wired: firing
// the registered hook with a value re-inits the base to resolve that backend.
func TestSettingHookReinits(t *testing.T) {
	resetBase(t)
	fake := regBackend(t, "hooked")
	t.Cleanup(func() { resetBase(t) })

	hasHook, err := op.HandleSettingItemHook(&model.SettingItem{Key: conf.SearchIndex, Value: "hooked"})
	if err != nil {
		t.Fatalf("HandleSettingItemHook: %v", err)
	}
	if !hasHook {
		t.Fatalf("no hook registered for %q", conf.SearchIndex)
	}
	if ActiveIndexer() != ext.SearchIndexer(fake) {
		t.Fatal("firing the search_index hook must re-init to the selected backend")
	}

	// And flipping back to none releases it through the same hook.
	if _, err := op.HandleSettingItemHook(&model.SettingItem{Key: conf.SearchIndex, Value: "none"}); err != nil {
		t.Fatalf("HandleSettingItemHook(none): %v", err)
	}
	if ActiveIndexer() != nil {
		t.Fatal("search_index=none via hook must release the backend")
	}
}
