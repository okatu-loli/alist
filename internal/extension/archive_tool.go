package extension

import (
	"sort"
	"strings"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// The ArchiveTool host registry mirrors search_indexer.go / offline_download_tool.go
// in structure, with one difference: dispatch is keyed by the FILE EXTENSION an
// archive tool claims (Exts()), not a user-facing setting value, since browsing a
// .zip/.rar/... file has no per-request "which tool" choice -- the file's own
// extension picks it. A trusted in-process instance (the built-in zip fallback,
// builtin.archive.zip) is registered here exactly like a search backend or
// offline-download tool; the extension-to-id index built from Exts() is what
// internal/archive's dispatch layer consults.
var (
	archiveToolMu        sync.RWMutex
	archiveToolInstances = map[string]ext.ArchiveTool{}
	// archiveToolExtIndex maps a normalized (lowercase, dot-less) file
	// extension to the id of the trusted in-process instance currently
	// claiming it. Built/rebuilt on every RegisterTrustedArchiveToolInstance
	// call. If two in-process instances somehow claim the same extension, the
	// most recent registration wins -- this only matters for built-ins, which
	// the host itself authors and does not do today.
	archiveToolExtIndex = map[string]string{}
)

// normalizeArchiveExt lowercases e and strips a single leading dot, so ".ZIP",
// "zip", and "Zip" all key the same registry slot.
func normalizeArchiveExt(e string) string {
	return strings.ToLower(strings.TrimPrefix(e, "."))
}

// RegisterTrustedArchiveToolInstance registers a BUILT-IN, host-authored
// in-process ext.ArchiveTool (e.g. builtin.archive.zip) under id. Mirrors
// RegisterTrustedSearchIndexerInstance's shape: a nil value deregisters the id
// (and every extension it had claimed). A tool extension calls this from its
// own registration path so internal/archive's dispatch layer can resolve it
// by the archive file's extension.
func RegisterTrustedArchiveToolInstance(id string, t ext.ArchiveTool) {
	archiveToolMu.Lock()
	defer archiveToolMu.Unlock()
	for e, owner := range archiveToolExtIndex {
		if owner == id {
			delete(archiveToolExtIndex, e)
		}
	}
	if t == nil {
		delete(archiveToolInstances, id)
		return
	}
	archiveToolInstances[id] = t
	for _, e := range t.Exts() {
		ne := normalizeArchiveExt(e)
		if ne == "" {
			continue
		}
		archiveToolExtIndex[ne] = id
	}
}

// GetArchiveTool resolves a registered ArchiveTool instance by id. The bool
// reports whether one is registered.
func GetArchiveTool(id string) (ext.ArchiveTool, bool) {
	archiveToolMu.RLock()
	defer archiveToolMu.RUnlock()
	t, ok := archiveToolInstances[id]
	return t, ok
}

// ArchiveToolInstanceIDs returns the ids of every registered in-process
// ArchiveTool instance, sorted.
func ArchiveToolInstanceIDs() []string {
	archiveToolMu.RLock()
	defer archiveToolMu.RUnlock()
	ids := make([]string, 0, len(archiveToolInstances))
	for id := range archiveToolInstances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// archiveToolFactory builds a live ext.ArchiveTool plus a release func for an
// enabled Kind=ArchiveTool gRPC plugin Entry. It is the gRPC-dispatch seam,
// mirroring offlineDownloadToolFactory: the server layer installs it
// (SetArchiveToolFactory) so internal/extension need not import pluginproc.
var archiveToolFactory func(e *Entry) (ext.ArchiveTool, func(), error)

// SetArchiveToolFactory installs the gRPC ArchiveTool provider.
func SetArchiveToolFactory(fn func(e *Entry) (ext.ArchiveTool, func(), error)) {
	archiveToolFactory = fn
}

// resolveArchiveTool returns the live tool for an entry (an in-process
// instance registered under the entry's manifest id first, then the gRPC
// factory seam for an ExecGRPC entry), or nil+noop when neither resolves.
// Mirrors resolveOfflineDownloadTool.
func resolveArchiveTool(e *Entry) (ext.ArchiveTool, func()) {
	archiveToolMu.RLock()
	if t, ok := archiveToolInstances[e.Manifest.ID]; ok {
		archiveToolMu.RUnlock()
		return t, func() {}
	}
	archiveToolMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && archiveToolFactory != nil {
		if t, release, err := archiveToolFactory(e); err == nil {
			return t, release
		}
	}
	return nil, func() {}
}

// FindArchiveToolForExt resolves the ArchiveTool that claims fileExt (any
// case, with or without a leading dot): a registered in-process (built-in)
// instance first -- provided its owning registry Entry is still enabled --
// then, failing that, an enabled ExecGRPC Kind=ArchiveTool plugin whose
// self-reported Exts() contains it.
//
// remote reports whether the returned tool crosses a gRPC boundary: the
// caller (internal/archive's dispatch layer) must localize the archive to a
// local temp file and hand the tool an ArchiveSource{Path,Size} rather than
// the in-process ReaderAt fast path. release must be called exactly once when
// the caller is done with the tool -- immediately after Meta/List, or once
// Extract's returned stream is closed.
//
// Unlike FindOfflineDownloadToolByName/FindSearchIndexerByName this does NOT
// cache a gRPC resolution across calls: an archive Meta/List/Extract is a
// short, one-shot dispatch per HTTP request (unlike a download task that
// holds its tool for the task's whole lifetime), so resolving fresh and
// releasing right away (or on stream Close) is simpler and cheap --
// pluginproc.Acquire is just a refcount bump on an already-running shared
// subprocess.
func FindArchiveToolForExt(r *Registry, fileExt string) (tool ext.ArchiveTool, remote bool, release func(), ok bool) {
	fileExt = normalizeArchiveExt(fileExt)
	if fileExt == "" {
		return nil, false, func() {}, false
	}

	archiveToolMu.RLock()
	id, claimed := archiveToolExtIndex[fileExt]
	archiveToolMu.RUnlock()
	if claimed {
		if e, found := r.Get(id); found && e.Enabled() {
			archiveToolMu.RLock()
			t, ok2 := archiveToolInstances[id]
			archiveToolMu.RUnlock()
			if ok2 {
				return t, false, func() {}, true
			}
		}
	}

	for _, e := range r.ByKind(ext.KindArchiveTool) {
		if e.execMode != ExecGRPC {
			continue
		}
		t, release := resolveArchiveTool(e)
		if t == nil {
			continue
		}
		matched := false
		for _, claim := range t.Exts() {
			if normalizeArchiveExt(claim) == fileExt {
				matched = true
				break
			}
		}
		if matched {
			return t, true, release, true
		}
		release()
	}
	return nil, false, func() {}, false
}

// ArchiveToolGRPCExts returns the self-reported Exts() of every enabled
// Kind=ArchiveTool ExecGRPC entry in r, sorted and deduplicated. Metadata-only
// enumeration (mirrors OfflineDownloadToolGRPCNames): resolves and releases
// each candidate immediately, nothing is held across calls.
func ArchiveToolGRPCExts(r *Registry) []string {
	seen := map[string]bool{}
	var exts []string
	for _, e := range r.ByKind(ext.KindArchiveTool) {
		if e.execMode != ExecGRPC {
			continue
		}
		t, release := resolveArchiveTool(e)
		if t == nil {
			continue
		}
		for _, claim := range t.Exts() {
			ne := normalizeArchiveExt(claim)
			if ne == "" || seen[ne] {
				continue
			}
			seen[ne] = true
			exts = append(exts, ne)
		}
		release()
	}
	sort.Strings(exts)
	return exts
}
