package extension

import (
	"sort"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// The SearchIndexer host registry mirrors internal/extension/authnmethod.go in
// structure (a package-level instance map behind an RWMutex, with a
// Register/Get/enumerate trio). Unlike AuthNMethod it carries NO trust
// distinction: every SearchIndexer registered here is host-authored — the index
// is built as admin over the full path space and the `search` base
// re-authorizes each hit per requesting user, so an indexer never makes an
// access decision and there is no "trusted vs untrusted method" axis to model.
// The Trusted name is kept only to match the AuthNMethod register idiom and to
// document that these instances are in-process built-ins (the out-of-process
// gRPC leg is a future wave, exactly as with AuthNMethod).
var (
	searchIndexerMu        sync.RWMutex
	searchIndexerInstances = map[string]ext.SearchIndexer{}
)

// RegisterTrustedSearchIndexerInstance registers a BUILT-IN, host-authored
// in-process ext.SearchIndexer (e.g. builtin.search.database/bleve/meilisearch)
// under id. It mirrors RegisterTrustedAuthNMethodInstance's shape: a nil value
// deregisters the id. A backend extension calls this from its own registration
// path so the `search` base can resolve it by id via the search_index setting.
func RegisterTrustedSearchIndexerInstance(id string, s ext.SearchIndexer) {
	searchIndexerMu.Lock()
	defer searchIndexerMu.Unlock()
	if s == nil {
		delete(searchIndexerInstances, id)
		return
	}
	searchIndexerInstances[id] = s
}

// GetSearchIndexer resolves a registered SearchIndexer instance by id. The bool
// reports whether one is registered — an unresolved id is NOT an error at this
// layer (a not-yet-registered or disabled backend simply means search falls back
// to the index-less BFS), so the base treats "not found" as "no active
// indexer", never as a failure.
func GetSearchIndexer(id string) (ext.SearchIndexer, bool) {
	searchIndexerMu.RLock()
	defer searchIndexerMu.RUnlock()
	s, ok := searchIndexerInstances[id]
	return s, ok
}

// SearchIndexerInstanceIDs returns the ids of every registered SearchIndexer
// instance, sorted, so the base / settings layer can list the selectable
// backends deterministically.
func SearchIndexerInstanceIDs() []string {
	searchIndexerMu.RLock()
	defer searchIndexerMu.RUnlock()
	ids := make([]string, 0, len(searchIndexerInstances))
	for id := range searchIndexerInstances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// searchIndexerFactory builds a live ext.SearchIndexer plus a release func
// for an enabled Kind=SearchIndexer gRPC plugin Entry (Phase E ring 1b). It
// is the gRPC-dispatch seam, mirroring offlineDownloadToolFactory: the server
// layer installs it (SetSearchIndexerFactory) so internal/extension need not
// import pluginproc. Until it is wired the factory is nil and only
// in-process instances (RegisterTrustedSearchIndexerInstance) are resolved.
var searchIndexerFactory func(e *Entry) (ext.SearchIndexer, func(), error)

// SetSearchIndexerFactory installs the gRPC SearchIndexer provider.
func SetSearchIndexerFactory(fn func(e *Entry) (ext.SearchIndexer, func(), error)) {
	searchIndexerFactory = fn
}

// resolveSearchIndexer returns the live backend for an entry (an in-process
// instance registered under the entry's manifest id first, then the gRPC
// factory seam for an ExecGRPC entry), or nil+noop when neither resolves.
// Mirrors resolveOfflineDownloadTool.
//
// NOTE: RegisterTrustedSearchIndexerInstance/RegisterBackend (like
// RegisterTrustedOfflineDownloadToolInstance) key built-in instances by the
// backend's NAME (e.g. "database_non_full_text"), not by e.Manifest.ID,
// because the search base resolves backends by name (the search_index
// setting value). So the searchIndexerInstances lookup here, keyed by
// e.Manifest.ID, only ever hits for a future in-process gRPC-Kind backend
// sharing its manifest id as its instance key, or a test double registered
// that way; it is kept for shape parity with every other Kind's resolve
// function and to leave that door open, not because a built-in backend is
// expected to hit it.
func resolveSearchIndexer(e *Entry) (ext.SearchIndexer, func()) {
	searchIndexerMu.RLock()
	if s, ok := searchIndexerInstances[e.Manifest.ID]; ok {
		searchIndexerMu.RUnlock()
		return s, func() {}
	}
	searchIndexerMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && searchIndexerFactory != nil {
		if s, release, err := searchIndexerFactory(e); err == nil {
			return s, release
		}
	}
	return nil, func() {}
}

// grpcSearchIndexerCacheEntry pairs a resolved gRPC SearchIndexer with the
// exact *Entry it was resolved from and the release func that returns its
// pluginproc reference. See FindSearchIndexerByName's doc comment for why
// this is cached rather than released right after each lookup.
//
// entry is the POINTER (not just the id): registerActive's
// Deregister+RegisterPluginMode dance (Update/Rollback/LoadEnabled) installs
// a brand-new *Entry under the same id with a new binPath, so comparing ids
// alone would keep serving a stale client resolved against the old binPath
// forever. Comparing pointers catches that swap.
type grpcSearchIndexerCacheEntry struct {
	entry   *Entry
	indexer ext.SearchIndexer
	release func()
}

var (
	grpcSearchIndexerCacheMu sync.Mutex
	grpcSearchIndexerCache   = map[string]grpcSearchIndexerCacheEntry{}
)

// grpcSearchIndexerExitedChecker is the structural interface a gRPC-backed
// SearchIndexer client satisfies (host.SearchIndexerClient.Exited, mirroring
// every other Kind's host client) — internal/extension cannot import
// internal/extension/grpc/host (that would be the reverse of the existing
// dependency direction), so liveness is probed structurally instead of by
// concrete type. An in-process instance/test double that does not implement
// this is simply never treated as crashed (there is no subprocess for it to
// crash independently of the host process).
type grpcSearchIndexerExitedChecker interface {
	Exited() bool
}

// grpcSearchIndexerCacheLookup validates a cache hit for name against r: the
// owning entry must still be the SAME *Entry pointer (not swapped out by an
// Update/Rollback/LoadEnabled re-register), still enabled, still ExecGRPC,
// and — if the cached indexer exposes Exited() — not a crashed subprocess. A
// stale/dead hit is evicted and its reference released before reporting a
// miss, so the caller's subsequent scan-and-resolve gets a fresh client
// instead of layering a second reference onto a corpse. Mirrors
// grpcToolCacheLookup.
func grpcSearchIndexerCacheLookup(r *Registry, name string) (ext.SearchIndexer, bool) {
	grpcSearchIndexerCacheMu.Lock()
	c, ok := grpcSearchIndexerCache[name]
	if !ok {
		grpcSearchIndexerCacheMu.Unlock()
		return nil, false
	}
	cur, ok2 := r.Get(c.entry.Manifest.ID)
	live := ok2 && cur == c.entry && cur.Enabled() && cur.execMode == ExecGRPC
	if live {
		if ec, ok3 := c.indexer.(grpcSearchIndexerExitedChecker); ok3 && ec.Exited() {
			live = false
		}
	}
	if live {
		grpcSearchIndexerCacheMu.Unlock()
		return c.indexer, true
	}
	delete(grpcSearchIndexerCache, name)
	grpcSearchIndexerCacheMu.Unlock()
	c.release()
	return nil, false
}

// FindSearchIndexerByName is the search_index setting's miss-fallback (Phase
// E ring 1b): searchIndexerInstances only ever holds the in-process built-in
// backends (database/bleve/meilisearch), registered by RegisterBackend under
// their backend NAME — a Kind=SearchIndexer gRPC plugin has no such
// registration. It is found here instead, by scanning every enabled ExecGRPC
// entry of that Kind and matching its self-reported Config().Name. Mirrors
// FindOfflineDownloadToolByName.
//
// Lifecycle: like a download tool, an active index backend is held for the
// WHOLE lifetime it stays selected — every search/index/build call routes
// through internal/search's ActiveIndexer(), potentially many times a
// second — so resolving fresh and releasing immediately on every call would
// let the plugin subprocess's pluginproc refcount hit zero (and be torn
// down) between calls. Since the search_index resolution path (unlike
// Acquire's) has no release func for a caller to hold onto, the resolved
// (indexer, release) pair is cached here instead and held for as long as the
// owning entry stays an enabled ExecGRPC match for that name
// (grpcSearchIndexerCacheLookup) — the same "just always there, never
// actively released" permanence a built-in backend's registration already
// has (RegisterTrustedSearchIndexerInstance is never itself paired with a
// teardown call either) — and is only actually released once that stops
// being true (the entry is disabled, removed, updated to a new *Entry, its
// subprocess has crashed, or it no longer reports this name), so a
// disabled/uninstalled/crashed plugin's subprocess is not leaked forever.
//
// Concurrent first resolution: two callers racing a cache miss for the same
// name both fall through to the scan-and-resolve loop below and each
// independently call resolveSearchIndexer (pluginproc.Acquire is safe to
// call concurrently — it just refcounts the shared subprocess up). Whichever
// goroutine reaches the lock first installs the cache entry; the loser
// re-checks under the SAME lock before installing its own, and — since it is
// racing against the SAME matching entry — finds the winner's entry already
// there and releases its own extra reference instead of stomping the
// winner's (which would otherwise strand that reference forever, since
// nothing else would ever call its release).
func FindSearchIndexerByName(r *Registry, name string) (ext.SearchIndexer, bool) {
	if s, ok := grpcSearchIndexerCacheLookup(r, name); ok {
		return s, true
	}

	for _, e := range r.ByKind(ext.KindSearchIndexer) {
		if e.execMode != ExecGRPC {
			continue
		}
		s, release := resolveSearchIndexer(e)
		if s == nil {
			continue
		}
		if s.Config().Name != name {
			release()
			continue
		}

		grpcSearchIndexerCacheMu.Lock()
		if cur, ok := grpcSearchIndexerCache[name]; ok {
			if cur.entry == e {
				// A concurrent caller already won the race to cache exactly
				// this resolution: keep theirs, drop our own extra reference.
				grpcSearchIndexerCacheMu.Unlock()
				release()
				return cur.indexer, true
			}
			// A different (stale) entry is cached under this name: release
			// its reference before replacing it with the fresh one below.
			cur.release()
		}
		grpcSearchIndexerCache[name] = grpcSearchIndexerCacheEntry{entry: e, indexer: s, release: release}
		grpcSearchIndexerCacheMu.Unlock()
		return s, true
	}
	return nil, false
}

// SearchIndexerGRPCNames returns the self-reported Config().Name of every
// enabled Kind=SearchIndexer ExecGRPC entry in r, sorted and deduplicated —
// the dynamic half of the search_index setting's Options list (Phase E ring
// 1b: server/handles/setting.go appends this to the 4 built-in names).
// Unlike FindSearchIndexerByName this is a one-shot metadata read (Config()
// only), so it resolves and releases each candidate immediately rather than
// caching it — nothing here is held across calls. Mirrors
// OfflineDownloadToolGRPCNames.
func SearchIndexerGRPCNames(r *Registry) []string {
	seen := map[string]bool{}
	var names []string
	for _, e := range r.ByKind(ext.KindSearchIndexer) {
		if e.execMode != ExecGRPC {
			continue
		}
		s, release := resolveSearchIndexer(e)
		if s == nil {
			continue
		}
		name := s.Config().Name
		release()
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
