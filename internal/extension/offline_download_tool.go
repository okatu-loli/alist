package extension

import (
	"sort"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// The OfflineDownloadTool host registry mirrors search_indexer.go in structure
// (a package-level instance map behind an RWMutex, with a Register/Get/enumerate
// trio). Every OfflineDownloadTool registered here is host-authored (aria2 /
// qBittorrent / transmission built-in extensions) — the daemon endpoints are
// admin-configured and admin-trusted, so there is no "trusted vs untrusted"
// axis to model. The Trusted name is kept only to match the SearchIndexer /
// AuthNMethod register idiom and to document that these instances are in-process
// built-ins (the out-of-process gRPC leg is a future wave).
var (
	offlineDownloadToolMu        sync.RWMutex
	offlineDownloadToolInstances = map[string]ext.OfflineDownloadTool{}
)

// RegisterTrustedOfflineDownloadToolInstance registers a BUILT-IN, host-authored
// in-process ext.OfflineDownloadTool (e.g. builtin.offline_download.aria2) under
// id. It mirrors RegisterTrustedSearchIndexerInstance's shape: a nil value
// deregisters the id. A tool extension calls this from its own registration path
// so the offline_download base can resolve it by id via the offline_download_tool
// setting.
func RegisterTrustedOfflineDownloadToolInstance(id string, t ext.OfflineDownloadTool) {
	offlineDownloadToolMu.Lock()
	defer offlineDownloadToolMu.Unlock()
	if t == nil {
		delete(offlineDownloadToolInstances, id)
		return
	}
	offlineDownloadToolInstances[id] = t
}

// GetOfflineDownloadTool resolves a registered OfflineDownloadTool instance by
// id. The bool reports whether one is registered — an unresolved id is NOT an
// error at this layer (a not-yet-registered or disabled tool simply means
// offline download falls back to the built-in SimpleHttp direct path), so the
// base treats "not found" as "no active tool", never as a failure.
func GetOfflineDownloadTool(id string) (ext.OfflineDownloadTool, bool) {
	offlineDownloadToolMu.RLock()
	defer offlineDownloadToolMu.RUnlock()
	t, ok := offlineDownloadToolInstances[id]
	return t, ok
}

// OfflineDownloadToolInstanceIDs returns the ids of every registered
// OfflineDownloadTool instance, sorted, so the base / settings layer can list
// the selectable tools deterministically.
func OfflineDownloadToolInstanceIDs() []string {
	offlineDownloadToolMu.RLock()
	defer offlineDownloadToolMu.RUnlock()
	ids := make([]string, 0, len(offlineDownloadToolInstances))
	for id := range offlineDownloadToolInstances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// offlineDownloadToolFactory builds a live ext.OfflineDownloadTool plus a
// release func for an enabled Kind=OfflineDownloadTool gRPC plugin Entry. It
// is the gRPC-dispatch seam, mirroring notificationFactory: the server layer
// installs it (SetOfflineDownloadToolFactory) so internal/extension need not
// import pluginproc. Until it is wired the factory is nil and only in-process
// instances (RegisterTrustedOfflineDownloadToolInstance) are delivered to.
var offlineDownloadToolFactory func(e *Entry) (ext.OfflineDownloadTool, func(), error)

// SetOfflineDownloadToolFactory installs the gRPC OfflineDownloadTool
// provider.
func SetOfflineDownloadToolFactory(fn func(e *Entry) (ext.OfflineDownloadTool, func(), error)) {
	offlineDownloadToolFactory = fn
}

// resolveOfflineDownloadTool returns the live tool for an entry (an
// in-process instance registered under the entry's manifest id first, then
// the gRPC factory seam for an ExecGRPC entry), or nil+noop when neither
// resolves. Mirrors resolveNotification.
//
// NOTE: RegisterTrustedOfflineDownloadToolInstance (unlike
// RegisterNotificationInstance/RegisterEventHookInstance/...) keys built-in
// instances by the tool's NAME (e.g. "aria2"), not by e.Manifest.ID, because
// the offline_download base resolves tools by name (ToolByName) — see that
// registration function's doc comment. So the offlineDownloadToolInstances
// lookup here, keyed by e.Manifest.ID, only ever hits for a future in-process
// gRPC-Kind tool sharing its manifest id as its instance key, or a test
// double registered that way; it is kept for shape parity with every other
// Kind's resolve function and to leave that door open, not because a built-in
// tool is expected to hit it.
func resolveOfflineDownloadTool(e *Entry) (ext.OfflineDownloadTool, func()) {
	offlineDownloadToolMu.RLock()
	if t, ok := offlineDownloadToolInstances[e.Manifest.ID]; ok {
		offlineDownloadToolMu.RUnlock()
		return t, func() {}
	}
	offlineDownloadToolMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && offlineDownloadToolFactory != nil {
		if t, release, err := offlineDownloadToolFactory(e); err == nil {
			return t, release
		}
	}
	return nil, func() {}
}

// grpcToolCacheEntry pairs a resolved gRPC OfflineDownloadTool with the exact
// *Entry it was resolved from and the release func that returns its
// pluginproc reference. See FindOfflineDownloadToolByName's doc comment for
// why this is cached rather than released right after each lookup.
//
// entry is the POINTER (not just the id): registerActive's
// Deregister+RegisterPluginMode dance (Update/Rollback/LoadEnabled) installs
// a brand-new *Entry under the same id with a new binPath, so comparing ids
// alone would keep serving a stale client resolved against the old binPath
// forever. Comparing pointers catches that swap.
type grpcToolCacheEntry struct {
	entry   *Entry
	tool    ext.OfflineDownloadTool
	release func()
}

var (
	grpcToolCacheMu sync.Mutex
	grpcToolCache   = map[string]grpcToolCacheEntry{}
)

// grpcToolExitedChecker is the structural interface a gRPC-backed
// OfflineDownloadTool client satisfies (host.OfflineDownloadToolClient.Exited,
// mirroring every other Kind's host client) — internal/extension cannot
// import internal/extension/grpc/host (that would be the reverse of the
// existing dependency direction), so liveness is probed structurally instead
// of by concrete type. An in-process instance/test double that does not
// implement this is simply never treated as crashed (there is no subprocess
// for it to crash independently of the host process).
type grpcToolExitedChecker interface {
	Exited() bool
}

// grpcToolCacheLookup validates a cache hit for name against r: the owning
// entry must still be the SAME *Entry pointer (not swapped out by an
// Update/Rollback/LoadEnabled re-register), still enabled, still ExecGRPC,
// and — if the cached tool exposes Exited() — not a crashed subprocess. A
// stale/dead hit is evicted and its reference released before reporting a
// miss, so the caller's subsequent scan-and-resolve gets a fresh client
// instead of layering a second reference onto a corpse.
func grpcToolCacheLookup(r *Registry, name string) (ext.OfflineDownloadTool, bool) {
	grpcToolCacheMu.Lock()
	c, ok := grpcToolCache[name]
	if !ok {
		grpcToolCacheMu.Unlock()
		return nil, false
	}
	cur, ok2 := r.Get(c.entry.Manifest.ID)
	live := ok2 && cur == c.entry && cur.Enabled() && cur.execMode == ExecGRPC
	if live {
		if ec, ok3 := c.tool.(grpcToolExitedChecker); ok3 && ec.Exited() {
			live = false
		}
	}
	if live {
		grpcToolCacheMu.Unlock()
		return c.tool, true
	}
	delete(grpcToolCache, name)
	grpcToolCacheMu.Unlock()
	c.release()
	return nil, false
}

// FindOfflineDownloadToolByName is ToolByName's miss-fallback (Phase E ring
// 8): offlineDownloadToolInstances only ever holds the in-process built-in
// tools (aria2/qBittorrent/Transmission), registered by RegisterTool under
// their tool NAME — a Kind=OfflineDownloadTool gRPC plugin has no such
// registration. It is found here instead, by scanning every enabled ExecGRPC
// entry of that Kind and matching its self-reported Config().Name.
//
// Lifecycle: unlike a one-shot dispatch (DispatchNotification, ...), a
// download task holds its resolved tool for the WHOLE task run — AddURL, then
// Status polled every few seconds for as long as the download takes, then
// Remove (internal/offline_download/task.go's runDaemon) — so resolving fresh
// and releasing immediately on every call would let the plugin subprocess's
// pluginproc refcount hit zero (and be torn down) between polls, against a
// client value the caller is still holding and about to call again. Since
// ToolByName's own signature (unlike Acquire's) has no release func for a
// caller to hold onto, the resolved (tool, release) pair is cached here
// instead and held for as long as the owning entry stays an enabled ExecGRPC
// match for that name (grpcToolCacheLookup) — the same "just always there,
// never actively released" permanence a built-in tool's registration already
// has (RegisterTrustedOfflineDownloadToolInstance is never itself paired with
// a teardown call either) — and is only actually released once that stops
// being true (the entry is disabled, removed, updated to a new *Entry, its
// subprocess has crashed, or it no longer reports this name), so a
// disabled/uninstalled/crashed plugin's subprocess is not leaked forever.
//
// Concurrent first resolution: two callers racing a cache miss for the same
// name both fall through to the scan-and-resolve loop below and each
// independently call resolveOfflineDownloadTool (pluginproc.Acquire is safe
// to call concurrently — it just refcounts the shared subprocess up).
// Whichever goroutine reaches the lock first installs the cache entry; the
// loser re-checks under the SAME lock before installing its own, and — since
// it is racing against the SAME matching entry — finds the winner's entry
// already there and releases its own extra reference instead of stomping the
// winner's (which would otherwise strand that reference forever, since
// nothing else would ever call its release).
func FindOfflineDownloadToolByName(r *Registry, name string) (ext.OfflineDownloadTool, bool) {
	if t, ok := grpcToolCacheLookup(r, name); ok {
		return t, true
	}

	for _, e := range r.ByKind(ext.KindOfflineDownloadTool) {
		if e.execMode != ExecGRPC {
			continue
		}
		t, release := resolveOfflineDownloadTool(e)
		if t == nil {
			continue
		}
		if t.Config().Name != name {
			release()
			continue
		}

		grpcToolCacheMu.Lock()
		if cur, ok := grpcToolCache[name]; ok {
			if cur.entry == e {
				// A concurrent caller already won the race to cache exactly
				// this resolution: keep theirs, drop our own extra reference.
				grpcToolCacheMu.Unlock()
				release()
				return cur.tool, true
			}
			// A different (stale) entry is cached under this name: release
			// its reference before replacing it with the fresh one below.
			cur.release()
		}
		grpcToolCache[name] = grpcToolCacheEntry{entry: e, tool: t, release: release}
		grpcToolCacheMu.Unlock()
		return t, true
	}
	return nil, false
}

// OfflineDownloadToolGRPCNames returns the self-reported Config().Name of
// every enabled Kind=OfflineDownloadTool ExecGRPC entry in r, sorted and
// deduplicated — the dynamic half of the offline_download_tool setting's
// Options list (Phase E ring 9: server/handles/setting.go appends this to the
// 4 built-in names). Unlike FindOfflineDownloadToolByName this is a one-shot
// metadata read (Config() only), so it resolves and releases each candidate
// immediately rather than caching it — nothing here is held across calls.
func OfflineDownloadToolGRPCNames(r *Registry) []string {
	seen := map[string]bool{}
	var names []string
	for _, e := range r.ByKind(ext.KindOfflineDownloadTool) {
		if e.execMode != ExecGRPC {
			continue
		}
		t, release := resolveOfflineDownloadTool(e)
		if t == nil {
			continue
		}
		name := t.Config().Name
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
