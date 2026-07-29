package extension

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// fakeGRPCIndexer is a minimal ext.SearchIndexer test double that reports a
// configurable Config().Name — the gRPC miss-fallback (Phase E ring 1b)
// matches candidates by this field, not by registry id.
type fakeGRPCIndexer struct{ name string }

func (f fakeGRPCIndexer) Manifest() ext.Manifest               { return ext.Manifest{} }
func (f fakeGRPCIndexer) Init(context.Context, ext.Host) error { return nil }
func (f fakeGRPCIndexer) Shutdown(context.Context) error       { return nil }
func (f fakeGRPCIndexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: f.name}
}
func (f fakeGRPCIndexer) Search(context.Context, ext.SearchReq) ([]ext.SearchNode, int64, error) {
	return nil, 0, nil
}
func (f fakeGRPCIndexer) Index(context.Context, ext.SearchNode) error        { return nil }
func (f fakeGRPCIndexer) BatchIndex(context.Context, []ext.SearchNode) error { return nil }
func (f fakeGRPCIndexer) Get(context.Context, string) ([]ext.SearchNode, error) {
	return nil, nil
}
func (f fakeGRPCIndexer) Del(context.Context, string) error { return nil }
func (f fakeGRPCIndexer) Clear(context.Context) error       { return nil }

// exitedFakeIndexer is fakeGRPCIndexer's counterpart with a mutable Exited()
// flag, so a test can simulate the real gRPC host client crashing between two
// FindSearchIndexerByName calls (grpcSearchIndexerExitedChecker's structural
// interface). A pointer receiver so the test can flip Exited after caching.
type exitedFakeIndexer struct {
	name   string
	exited bool
}

func (f *exitedFakeIndexer) Manifest() ext.Manifest               { return ext.Manifest{} }
func (f *exitedFakeIndexer) Init(context.Context, ext.Host) error { return nil }
func (f *exitedFakeIndexer) Shutdown(context.Context) error       { return nil }
func (f *exitedFakeIndexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: f.name}
}
func (f *exitedFakeIndexer) Search(context.Context, ext.SearchReq) ([]ext.SearchNode, int64, error) {
	return nil, 0, nil
}
func (f *exitedFakeIndexer) Index(context.Context, ext.SearchNode) error        { return nil }
func (f *exitedFakeIndexer) BatchIndex(context.Context, []ext.SearchNode) error { return nil }
func (f *exitedFakeIndexer) Get(context.Context, string) ([]ext.SearchNode, error) {
	return nil, nil
}
func (f *exitedFakeIndexer) Del(context.Context, string) error { return nil }
func (f *exitedFakeIndexer) Clear(context.Context) error       { return nil }
func (f *exitedFakeIndexer) Exited() bool                      { return f.exited }

var _ grpcSearchIndexerExitedChecker = (*exitedFakeIndexer)(nil)

// grpcSearchIndexerEntry builds a declared, enabled, ExecGRPC
// Kind=SearchIndexer entry — the shape resolveSearchIndexer requires to even
// attempt the factory branch. Mirrors grpcOfflineDownloadToolEntry.
func grpcSearchIndexerEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindSearchIndexer}},
		Kind:     ext.KindSearchIndexer,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-search-indexer",
	}
}

// TestResolveSearchIndexerUsesFactory pins the gRPC-dispatch seam: an
// ExecGRPC entry with a binPath and no in-process instance must resolve
// through the installed searchIndexerFactory (the plugins.go init wires this
// to pluginproc.Default.AcquireSearchIndexer). Without a factory the same
// entry resolves to nil. Mirrors TestResolveOfflineDownloadToolUsesFactory.
func TestResolveSearchIndexerUsesFactory(t *testing.T) {
	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	e := grpcSearchIndexerEntry("indexer.grpc")

	// No factory wired -> no host-side handler -> nil.
	searchIndexerFactory = nil
	if s, _ := resolveSearchIndexer(e); s != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", s)
	}

	// Factory wired -> the ExecGRPC branch is hit and its indexer is returned.
	stub := fakeGRPCIndexer{name: "grpcindexer"}
	var released bool
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		if got.ID() != "indexer.grpc" || got.BinPath() != "/bin/example-search-indexer" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	s, release := resolveSearchIndexer(e)
	if s != stub {
		t.Fatalf("resolveSearchIndexer must return the factory's indexer, got %v", s)
	}
	release()
	if !released {
		t.Fatal("resolveSearchIndexer must return the factory's release func")
	}
}

// resetGRPCSearchIndexerCache clears FindSearchIndexerByName's cache and
// calls every cached entry's release, so tests do not leak stub "releases" or
// leak state into one another.
func resetGRPCSearchIndexerCache(t *testing.T) {
	t.Helper()
	grpcSearchIndexerCacheMu.Lock()
	for k, c := range grpcSearchIndexerCache {
		c.release()
		delete(grpcSearchIndexerCache, k)
	}
	grpcSearchIndexerCacheMu.Unlock()
}

// TestFindSearchIndexerByNameMatchesConfigName pins the ring-1b miss
// fallback: a registered ExecGRPC Kind=SearchIndexer entry is found by its
// self-reported Config().Name, not by its registry id, and a non-matching
// name is not found.
func TestFindSearchIndexerByNameMatchesConfigName(t *testing.T) {
	resetGRPCSearchIndexerCache(t)
	t.Cleanup(func() { resetGRPCSearchIndexerCache(t) })

	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	r := NewRegistry()
	e := grpcSearchIndexerEntry("indexer.grpc.byname")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := fakeGRPCIndexer{name: "mygrpcindexer"}
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		return stub, func() {}, nil
	}

	if _, ok := FindSearchIndexerByName(r, "nonexistent"); ok {
		t.Fatal("a name with no matching Config().Name must not be found")
	}

	s, ok := FindSearchIndexerByName(r, "mygrpcindexer")
	if !ok || s != stub {
		t.Fatalf("FindSearchIndexerByName(mygrpcindexer) = %v, %v; want stub, true", s, ok)
	}
}

// TestFindSearchIndexerByNameCachesResolution pins the held-for-the-entry's-
// lifetime cache (Phase E ring 1b's doc comment): a second lookup for the
// same name must not call the factory again (it reuses the cached
// resolution), matching the "resolve once, hold long" semantics
// ActiveIndexer needs across many search/index calls.
func TestFindSearchIndexerByNameCachesResolution(t *testing.T) {
	resetGRPCSearchIndexerCache(t)
	t.Cleanup(func() { resetGRPCSearchIndexerCache(t) })

	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	r := NewRegistry()
	e := grpcSearchIndexerEntry("indexer.grpc.cache")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := fakeGRPCIndexer{name: "cachedindexer"}
	var factoryCalls int
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		factoryCalls++
		return stub, func() {}, nil
	}

	if _, ok := FindSearchIndexerByName(r, "cachedindexer"); !ok {
		t.Fatal("first lookup must resolve")
	}
	if _, ok := FindSearchIndexerByName(r, "cachedindexer"); !ok {
		t.Fatal("second lookup must still resolve (from cache)")
	}
	if factoryCalls != 1 {
		t.Fatalf("factory calls = %d, want 1 (second lookup must reuse the cached resolution)", factoryCalls)
	}
}

// TestFindSearchIndexerByNameEvictsOnDisable pins the auto-eviction half of
// the cache: once the owning entry is disabled, the cached resolution must be
// released and the name must no longer resolve — mirroring
// ActiveIndexer's existing enabled-gate behavior for built-in backends.
func TestFindSearchIndexerByNameEvictsOnDisable(t *testing.T) {
	resetGRPCSearchIndexerCache(t)
	t.Cleanup(func() { resetGRPCSearchIndexerCache(t) })

	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	r := NewRegistry()
	e := grpcSearchIndexerEntry("indexer.grpc.evict")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := fakeGRPCIndexer{name: "evictindexer"}
	var released bool
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		return stub, func() { released = true }, nil
	}

	if _, ok := FindSearchIndexerByName(r, "evictindexer"); !ok {
		t.Fatal("first lookup must resolve")
	}

	if err := r.Disable("indexer.grpc.evict"); err != nil {
		t.Fatal(err)
	}

	if _, ok := FindSearchIndexerByName(r, "evictindexer"); ok {
		t.Fatal("a disabled owning entry must no longer resolve")
	}
	if !released {
		t.Fatal("disabling the owning entry must release the cached resolution")
	}
}

// TestFindSearchIndexerByNameConcurrentFirstResolveNoLeak pins the
// concurrent-first-resolution safety: two goroutines racing a cache miss for
// the same name each independently resolve (mirroring pluginproc.Acquire,
// which is safe to call concurrently and just refcounts the shared
// subprocess up), but only ONE reference may end up held by the cache — the
// loser must release its own extra reference instead of letting the winner's
// cache write silently strand it. Net simulated pluginproc refs after both
// goroutines finish must be exactly 1. Mirrors
// TestFindOfflineDownloadToolByNameConcurrentFirstResolveNoLeak.
func TestFindSearchIndexerByNameConcurrentFirstResolveNoLeak(t *testing.T) {
	resetGRPCSearchIndexerCache(t)
	t.Cleanup(func() { resetGRPCSearchIndexerCache(t) })

	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	r := NewRegistry()
	e := grpcSearchIndexerEntry("indexer.grpc.concurrent")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	// refs simulates pluginproc.Manager's refcount for the shared subprocess:
	// every factory call increments it (an Acquire), every release decrements
	// it exactly once (release funcs in production are idempotent via
	// sync.Once; mirrored here as well for realism).
	var refs int32
	stub := fakeGRPCIndexer{name: "concurrentindexer"}
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		atomic.AddInt32(&refs, 1)
		var once sync.Once
		return stub, func() { once.Do(func() { atomic.AddInt32(&refs, -1) }) }, nil
	}

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, ok := FindSearchIndexerByName(r, "concurrentindexer")
			results[i] = ok
		}(i)
	}
	close(start)
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Fatalf("goroutine %d: FindSearchIndexerByName must resolve", i)
		}
	}
	if got := atomic.LoadInt32(&refs); got != 1 {
		t.Fatalf("net refs after concurrent first-resolve = %d, want 1 (every losing goroutine's extra reference must be released)", got)
	}
}

// TestFindSearchIndexerByNameReResolvesAfterUpdate pins the Update-doesn't-
// invalidate-cache fix: registerActive's Update/Rollback/LoadEnabled path is
// Deregister + RegisterPluginMode, which installs a brand-new *Entry under
// the SAME id (with, in practice, a new binPath). A cache keyed only by
// entry id would keep serving the client resolved against the OLD *Entry
// forever; comparing the entry pointer must detect the swap and re-resolve.
func TestFindSearchIndexerByNameReResolvesAfterUpdate(t *testing.T) {
	resetGRPCSearchIndexerCache(t)
	t.Cleanup(func() { resetGRPCSearchIndexerCache(t) })

	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	r := NewRegistry()
	e1 := grpcSearchIndexerEntry("indexer.grpc.update")
	if err := r.Register(e1); err != nil {
		t.Fatal(err)
	}

	var calls int
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		calls++
		return fakeGRPCIndexer{name: "updateindexer"}, func() {}, nil
	}

	if _, ok := FindSearchIndexerByName(r, "updateindexer"); !ok {
		t.Fatal("first resolve must succeed")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	// Simulate registerActive's Update path: Deregister + re-Register installs
	// a brand-new *Entry for the same id (a new binPath in practice).
	r.Deregister("indexer.grpc.update")
	e2 := grpcSearchIndexerEntry("indexer.grpc.update")
	e2.binPath = "/bin/example-search-indexer-v2"
	if err := r.Register(e2); err != nil {
		t.Fatal(err)
	}
	if e1 == e2 {
		t.Fatal("test setup bug: e1 and e2 must be distinct *Entry values")
	}

	if _, ok := FindSearchIndexerByName(r, "updateindexer"); !ok {
		t.Fatal("second resolve (after update) must succeed")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (an Entry swap must invalidate the stale cache entry and re-resolve)", calls)
	}
}

// TestFindSearchIndexerByNameReResolvesAfterExited pins crash self-heal: a
// cache hit whose indexer reports Exited()==true (the real
// host.SearchIndexerClient's liveness check, mirrored here via
// exitedFakeIndexer) must be released and re-resolved rather than handed
// back as a corpse.
func TestFindSearchIndexerByNameReResolvesAfterExited(t *testing.T) {
	resetGRPCSearchIndexerCache(t)
	t.Cleanup(func() { resetGRPCSearchIndexerCache(t) })

	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	r := NewRegistry()
	e := grpcSearchIndexerEntry("indexer.grpc.exited")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := &exitedFakeIndexer{name: "exitedindexer"}
	var calls int
	// releaseCalls counts EVERY release invocation across both resolutions
	// (unlike a single bool, it is not reset by the second resolve's own
	// factory call, so it can distinguish "the stale entry's release ran
	// before re-resolving" from "only the fresh entry's release func exists").
	var releaseCalls int
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		calls++
		return stub, func() { releaseCalls++ }, nil
	}

	if _, ok := FindSearchIndexerByName(r, "exitedindexer"); !ok {
		t.Fatal("first resolve must succeed")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	stub.exited = true
	if _, ok := FindSearchIndexerByName(r, "exitedindexer"); !ok {
		t.Fatal("resolve after crash must still succeed (self-heal via re-resolve)")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (a crashed cached client must be released and re-resolved)", calls)
	}
	if releaseCalls < 1 {
		t.Fatal("the crashed cached client's release must be called before re-resolving")
	}
}

// TestSearchIndexerGRPCNames pins the settings-Options half (Phase E ring
// 1b): every enabled ExecGRPC Kind=SearchIndexer entry's Config().Name is
// returned, sorted and deduplicated, and each candidate is released
// immediately (no cache entry left behind, unlike FindSearchIndexerByName).
func TestSearchIndexerGRPCNames(t *testing.T) {
	r := NewRegistry()
	eA := grpcSearchIndexerEntry("indexer.grpc.a")
	eB := grpcSearchIndexerEntry("indexer.grpc.b")
	if err := r.Register(eA); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(eB); err != nil {
		t.Fatal(err)
	}

	saved := searchIndexerFactory
	t.Cleanup(func() { searchIndexerFactory = saved })

	var releases int
	searchIndexerFactory = func(got *Entry) (ext.SearchIndexer, func(), error) {
		switch got.ID() {
		case "indexer.grpc.a":
			return fakeGRPCIndexer{name: "zzz"}, func() { releases++ }, nil
		case "indexer.grpc.b":
			return fakeGRPCIndexer{name: "aaa"}, func() { releases++ }, nil
		}
		return nil, nil, ext.NewError(ext.ErrCodeNotImplement, "unexpected entry")
	}

	names := SearchIndexerGRPCNames(r)
	if len(names) != 2 || names[0] != "aaa" || names[1] != "zzz" {
		t.Fatalf("SearchIndexerGRPCNames = %v, want sorted [aaa zzz]", names)
	}
	if releases != 2 {
		t.Fatalf("releases = %d, want 2 (each candidate released immediately, no cache)", releases)
	}
}
