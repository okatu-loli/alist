package extension

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// fakeGRPCTool is a minimal ext.OfflineDownloadTool test double that reports a
// configurable Config().Name — the gRPC miss-fallback (Phase E ring 8)
// matches candidates by this field, not by registry id.
type fakeGRPCTool struct{ name string }

func (f fakeGRPCTool) Manifest() ext.Manifest               { return ext.Manifest{} }
func (f fakeGRPCTool) Init(context.Context, ext.Host) error { return nil }
func (f fakeGRPCTool) Shutdown(context.Context) error       { return nil }
func (f fakeGRPCTool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: f.name}
}
func (f fakeGRPCTool) AddURL(context.Context, ext.AddArgs) (string, error) {
	return "", nil
}
func (f fakeGRPCTool) Remove(context.Context, string) error { return nil }
func (f fakeGRPCTool) Status(context.Context, string) (ext.ToolStatus, error) {
	return ext.ToolStatus{}, nil
}

// exitedFakeTool is fakeGRPCTool's counterpart with a mutable Exited() flag,
// so a test can simulate the real gRPC host client crashing between two
// FindOfflineDownloadToolByName calls (grpcToolExitedChecker's structural
// interface). A pointer receiver so the test can flip Exited after caching.
type exitedFakeTool struct {
	name   string
	exited bool
}

func (f *exitedFakeTool) Manifest() ext.Manifest               { return ext.Manifest{} }
func (f *exitedFakeTool) Init(context.Context, ext.Host) error { return nil }
func (f *exitedFakeTool) Shutdown(context.Context) error       { return nil }
func (f *exitedFakeTool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: f.name}
}
func (f *exitedFakeTool) AddURL(context.Context, ext.AddArgs) (string, error) {
	return "", nil
}
func (f *exitedFakeTool) Remove(context.Context, string) error { return nil }
func (f *exitedFakeTool) Status(context.Context, string) (ext.ToolStatus, error) {
	return ext.ToolStatus{}, nil
}
func (f *exitedFakeTool) Exited() bool { return f.exited }

var _ grpcToolExitedChecker = (*exitedFakeTool)(nil)

// grpcOfflineDownloadToolEntry builds a declared, enabled, ExecGRPC
// Kind=OfflineDownloadTool entry — the shape resolveOfflineDownloadTool
// requires to even attempt the factory branch. Mirrors grpcNotificationEntry.
func grpcOfflineDownloadToolEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindOfflineDownloadTool}},
		Kind:     ext.KindOfflineDownloadTool,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-offline-download-tool",
	}
}

// TestResolveOfflineDownloadToolUsesFactory pins the gRPC-dispatch seam: an
// ExecGRPC entry with a binPath and no in-process instance must resolve
// through the installed offlineDownloadToolFactory (the plugins.go init wires
// this to pluginproc.Default.AcquireOfflineDownloadTool). Without a factory
// the same entry resolves to nil. Mirrors TestResolveNotificationUsesFactory.
func TestResolveOfflineDownloadToolUsesFactory(t *testing.T) {
	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	e := grpcOfflineDownloadToolEntry("tool.grpc")

	// No factory wired -> no host-side handler -> nil.
	offlineDownloadToolFactory = nil
	if tool, _ := resolveOfflineDownloadTool(e); tool != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", tool)
	}

	// Factory wired -> the ExecGRPC branch is hit and its tool is returned.
	stub := fakeGRPCTool{name: "grpctool"}
	var released bool
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
		if got.ID() != "tool.grpc" || got.BinPath() != "/bin/example-offline-download-tool" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	tool, release := resolveOfflineDownloadTool(e)
	if tool != stub {
		t.Fatalf("resolveOfflineDownloadTool must return the factory's tool, got %v", tool)
	}
	release()
	if !released {
		t.Fatal("resolveOfflineDownloadTool must return the factory's release func")
	}
}

// resetGRPCToolCache clears FindOfflineDownloadToolByName's cache and calls
// every cached entry's release, so tests do not leak stub "releases" or leak
// state into one another.
func resetGRPCToolCache(t *testing.T) {
	t.Helper()
	grpcToolCacheMu.Lock()
	for k, c := range grpcToolCache {
		c.release()
		delete(grpcToolCache, k)
	}
	grpcToolCacheMu.Unlock()
}

// TestFindOfflineDownloadToolByNameMatchesConfigName pins the ring-8 miss
// fallback: a registered ExecGRPC Kind=OfflineDownloadTool entry is found by
// its self-reported Config().Name, not by its registry id, and a non-matching
// name is not found.
func TestFindOfflineDownloadToolByNameMatchesConfigName(t *testing.T) {
	resetGRPCToolCache(t)
	t.Cleanup(func() { resetGRPCToolCache(t) })

	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	r := NewRegistry()
	e := grpcOfflineDownloadToolEntry("tool.grpc.byname")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := fakeGRPCTool{name: "mygrpctool"}
	var releaseCount int
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
		return stub, func() { releaseCount++ }, nil
	}

	if _, ok := FindOfflineDownloadToolByName(r, "nonexistent"); ok {
		t.Fatal("a name with no matching Config().Name must not be found")
	}

	tool, ok := FindOfflineDownloadToolByName(r, "mygrpctool")
	if !ok || tool != stub {
		t.Fatalf("FindOfflineDownloadToolByName(mygrpctool) = %v, %v; want stub, true", tool, ok)
	}
}

// TestFindOfflineDownloadToolByNameCachesResolution pins the held-for-the-
// entry's-lifetime cache (Phase E ring 8's doc comment): a second lookup for
// the same name must not call the factory again (it reuses the cached
// resolution), matching the "resolve once, hold long" semantics a download
// task's runDaemon needs.
func TestFindOfflineDownloadToolByNameCachesResolution(t *testing.T) {
	resetGRPCToolCache(t)
	t.Cleanup(func() { resetGRPCToolCache(t) })

	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	r := NewRegistry()
	e := grpcOfflineDownloadToolEntry("tool.grpc.cache")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := fakeGRPCTool{name: "cachedtool"}
	var factoryCalls int
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
		factoryCalls++
		return stub, func() {}, nil
	}

	if _, ok := FindOfflineDownloadToolByName(r, "cachedtool"); !ok {
		t.Fatal("first lookup must resolve")
	}
	if _, ok := FindOfflineDownloadToolByName(r, "cachedtool"); !ok {
		t.Fatal("second lookup must still resolve (from cache)")
	}
	if factoryCalls != 1 {
		t.Fatalf("factory calls = %d, want 1 (second lookup must reuse the cached resolution)", factoryCalls)
	}
}

// TestFindOfflineDownloadToolByNameEvictsOnDisable pins the auto-eviction half
// of the cache: once the owning entry is disabled, the cached resolution must
// be released and the name must no longer resolve — mirroring ToolByName's
// existing enabled-gate behavior for built-in tools.
func TestFindOfflineDownloadToolByNameEvictsOnDisable(t *testing.T) {
	resetGRPCToolCache(t)
	t.Cleanup(func() { resetGRPCToolCache(t) })

	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	r := NewRegistry()
	e := grpcOfflineDownloadToolEntry("tool.grpc.evict")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := fakeGRPCTool{name: "evicttool"}
	var released bool
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
		return stub, func() { released = true }, nil
	}

	if _, ok := FindOfflineDownloadToolByName(r, "evicttool"); !ok {
		t.Fatal("first lookup must resolve")
	}

	if err := r.Disable("tool.grpc.evict"); err != nil {
		t.Fatal(err)
	}

	if _, ok := FindOfflineDownloadToolByName(r, "evicttool"); ok {
		t.Fatal("a disabled owning entry must no longer resolve")
	}
	if !released {
		t.Fatal("disabling the owning entry must release the cached resolution")
	}
}

// TestFindOfflineDownloadToolByNameConcurrentFirstResolveNoLeak pins the
// review-fix for the concurrent-first-resolution leak: two goroutines racing
// a cache miss for the same name each independently resolve (mirroring
// pluginproc.Acquire, which is safe to call concurrently and just refcounts
// the shared subprocess up), but only ONE reference may end up held by the
// cache — the loser must release its own extra reference instead of letting
// the winner's cache write silently strand it. Net simulated pluginproc refs
// after both goroutines finish must be exactly 1.
func TestFindOfflineDownloadToolByNameConcurrentFirstResolveNoLeak(t *testing.T) {
	resetGRPCToolCache(t)
	t.Cleanup(func() { resetGRPCToolCache(t) })

	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	r := NewRegistry()
	e := grpcOfflineDownloadToolEntry("tool.grpc.concurrent")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	// refs simulates pluginproc.Manager's refcount for the shared subprocess:
	// every factory call increments it (an Acquire), every release decrements
	// it exactly once (release funcs in production are idempotent via
	// sync.Once; mirrored here as well for realism).
	var refs int32
	stub := fakeGRPCTool{name: "concurrenttool"}
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
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
			_, ok := FindOfflineDownloadToolByName(r, "concurrenttool")
			results[i] = ok
		}(i)
	}
	close(start)
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Fatalf("goroutine %d: FindOfflineDownloadToolByName must resolve", i)
		}
	}
	if got := atomic.LoadInt32(&refs); got != 1 {
		t.Fatalf("net refs after concurrent first-resolve = %d, want 1 (every losing goroutine's extra reference must be released)", got)
	}
}

// TestFindOfflineDownloadToolByNameReResolvesAfterUpdate pins the review-fix
// for the Update-doesn't-invalidate-cache bug: registerActive's Update/
// Rollback/LoadEnabled path is Deregister + RegisterPluginMode, which installs
// a brand-new *Entry under the SAME id (with, in practice, a new binPath). A
// cache keyed only by entry id would keep serving the client resolved against
// the OLD *Entry forever; comparing the entry pointer must detect the swap
// and re-resolve.
func TestFindOfflineDownloadToolByNameReResolvesAfterUpdate(t *testing.T) {
	resetGRPCToolCache(t)
	t.Cleanup(func() { resetGRPCToolCache(t) })

	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	r := NewRegistry()
	e1 := grpcOfflineDownloadToolEntry("tool.grpc.update")
	if err := r.Register(e1); err != nil {
		t.Fatal(err)
	}

	var calls int
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
		calls++
		return fakeGRPCTool{name: "updatetool"}, func() {}, nil
	}

	if _, ok := FindOfflineDownloadToolByName(r, "updatetool"); !ok {
		t.Fatal("first resolve must succeed")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	// Simulate registerActive's Update path: Deregister + re-Register installs
	// a brand-new *Entry for the same id (a new binPath in practice).
	r.Deregister("tool.grpc.update")
	e2 := grpcOfflineDownloadToolEntry("tool.grpc.update")
	e2.binPath = "/bin/example-offline-download-tool-v2"
	if err := r.Register(e2); err != nil {
		t.Fatal(err)
	}
	if e1 == e2 {
		t.Fatal("test setup bug: e1 and e2 must be distinct *Entry values")
	}

	if _, ok := FindOfflineDownloadToolByName(r, "updatetool"); !ok {
		t.Fatal("second resolve (after update) must succeed")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (an Entry swap must invalidate the stale cache entry and re-resolve)", calls)
	}
}

// TestFindOfflineDownloadToolByNameReResolvesAfterExited pins the review-fix
// for crash self-heal: a cache hit whose tool reports Exited()==true (the
// real host.OfflineDownloadToolClient's liveness check, mirrored here via
// exitedFakeTool) must be released and re-resolved rather than handed back as
// a corpse.
func TestFindOfflineDownloadToolByNameReResolvesAfterExited(t *testing.T) {
	resetGRPCToolCache(t)
	t.Cleanup(func() { resetGRPCToolCache(t) })

	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	r := NewRegistry()
	e := grpcOfflineDownloadToolEntry("tool.grpc.exited")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := &exitedFakeTool{name: "exitedtool"}
	var calls int
	// releaseCalls counts EVERY release invocation across both resolutions
	// (unlike a single bool, it is not reset by the second resolve's own
	// factory call, so it can distinguish "the stale entry's release ran
	// before re-resolving" from "only the fresh entry's release func exists").
	var releaseCalls int
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
		calls++
		return stub, func() { releaseCalls++ }, nil
	}

	if _, ok := FindOfflineDownloadToolByName(r, "exitedtool"); !ok {
		t.Fatal("first resolve must succeed")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	stub.exited = true
	if _, ok := FindOfflineDownloadToolByName(r, "exitedtool"); !ok {
		t.Fatal("resolve after crash must still succeed (self-heal via re-resolve)")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (a crashed cached client must be released and re-resolved)", calls)
	}
	if releaseCalls < 1 {
		t.Fatal("the crashed cached client's release must be called before re-resolving")
	}
}

// TestOfflineDownloadToolGRPCNames pins the settings-Options half (Phase E
// ring 9): every enabled ExecGRPC Kind=OfflineDownloadTool entry's
// Config().Name is returned, sorted and deduplicated, and each candidate is
// released immediately (no cache entry left behind, unlike
// FindOfflineDownloadToolByName).
func TestOfflineDownloadToolGRPCNames(t *testing.T) {
	r := NewRegistry()
	eA := grpcOfflineDownloadToolEntry("tool.grpc.a")
	eB := grpcOfflineDownloadToolEntry("tool.grpc.b")
	if err := r.Register(eA); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(eB); err != nil {
		t.Fatal(err)
	}

	saved := offlineDownloadToolFactory
	t.Cleanup(func() { offlineDownloadToolFactory = saved })

	var releases int
	offlineDownloadToolFactory = func(got *Entry) (ext.OfflineDownloadTool, func(), error) {
		switch got.ID() {
		case "tool.grpc.a":
			return fakeGRPCTool{name: "zzz"}, func() { releases++ }, nil
		case "tool.grpc.b":
			return fakeGRPCTool{name: "aaa"}, func() { releases++ }, nil
		}
		return nil, nil, ext.NewError(ext.ErrCodeNotImplement, "unexpected entry")
	}

	names := OfflineDownloadToolGRPCNames(r)
	if len(names) != 2 || names[0] != "aaa" || names[1] != "zzz" {
		t.Fatalf("OfflineDownloadToolGRPCNames = %v, want sorted [aaa zzz]", names)
	}
	if releases != 2 {
		t.Fatalf("releases = %d, want 2 (each candidate released immediately, no cache)", releases)
	}
}
