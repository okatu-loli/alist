package extension

import (
	"sort"
	"strings"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// The Thumbnailer host registry mirrors archive_tool.go in structure and for
// the same reason: dispatch is keyed by the FILE EXTENSION a thumbnailer
// claims (Exts()), not a user-facing setting value, since thumbnailing a
// .png/.jpg/... file has no per-request "which renderer" choice -- the
// file's own extension picks it. A trusted in-process instance (the built-in
// imaging renderer, builtin.thumbnail.image) is registered here exactly like
// an archive tool or search backend; the extension-to-id index built from
// Exts() is what internal/thumbnail's Generate consults.
var (
	thumbnailerMu        sync.RWMutex
	thumbnailerInstances = map[string]ext.Thumbnailer{}
	// thumbnailerExtIndex maps a normalized (lowercase, dot-less) file
	// extension to the id of the trusted in-process instance currently
	// claiming it. Built/rebuilt on every RegisterTrustedThumbnailerInstance
	// call. If two in-process instances somehow claim the same extension,
	// the most recent registration wins -- this only matters for built-ins,
	// which the host itself authors and does not do today.
	thumbnailerExtIndex = map[string]string{}
)

// normalizeThumbnailerExt lowercases e and strips a single leading dot, so
// ".PNG", "png", and "Png" all key the same registry slot.
func normalizeThumbnailerExt(e string) string {
	return strings.ToLower(strings.TrimPrefix(e, "."))
}

// RegisterTrustedThumbnailerInstance registers a BUILT-IN, host-authored
// in-process ext.Thumbnailer (e.g. builtin.thumbnail.image) under id. Mirrors
// RegisterTrustedArchiveToolInstance's shape: a nil value deregisters the id
// (and every extension it had claimed). A thumbnailer extension calls this
// from its own registration path so internal/thumbnail's Generate can
// resolve it by the source file's extension.
func RegisterTrustedThumbnailerInstance(id string, t ext.Thumbnailer) {
	thumbnailerMu.Lock()
	defer thumbnailerMu.Unlock()
	for e, owner := range thumbnailerExtIndex {
		if owner == id {
			delete(thumbnailerExtIndex, e)
		}
	}
	if t == nil {
		delete(thumbnailerInstances, id)
		return
	}
	thumbnailerInstances[id] = t
	for _, e := range t.Exts() {
		ne := normalizeThumbnailerExt(e)
		if ne == "" {
			continue
		}
		thumbnailerExtIndex[ne] = id
	}
}

// GetThumbnailer resolves a registered Thumbnailer instance by id. The bool
// reports whether one is registered.
func GetThumbnailer(id string) (ext.Thumbnailer, bool) {
	thumbnailerMu.RLock()
	defer thumbnailerMu.RUnlock()
	t, ok := thumbnailerInstances[id]
	return t, ok
}

// ThumbnailerInstanceIDs returns the ids of every registered in-process
// Thumbnailer instance, sorted.
func ThumbnailerInstanceIDs() []string {
	thumbnailerMu.RLock()
	defer thumbnailerMu.RUnlock()
	ids := make([]string, 0, len(thumbnailerInstances))
	for id := range thumbnailerInstances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// thumbnailerFactory builds a live ext.Thumbnailer plus a release func for an
// enabled Kind=Thumbnailer gRPC plugin Entry. It is the gRPC-dispatch seam,
// mirroring archiveToolFactory: left UNWIRED this wave (Phase E wave 3) --
// the binary-stream wire format for a Kind=Thumbnailer plugin is deferred --
// but installed here so a later wave can wire it via SetThumbnailerFactory
// without another registry reshape.
var thumbnailerFactory func(e *Entry) (ext.Thumbnailer, func(), error)

// SetThumbnailerFactory installs the gRPC Thumbnailer provider. Unused this
// wave (see thumbnailerFactory's doc comment) -- the seam is wired ahead of
// its consumer, mirroring how SetArchiveToolFactory was added in Phase E
// wave 2 before any Kind=ArchiveTool plugin used it.
func SetThumbnailerFactory(fn func(e *Entry) (ext.Thumbnailer, func(), error)) {
	thumbnailerFactory = fn
}

// resolveThumbnailer returns the live renderer for an entry (an in-process
// instance registered under the entry's manifest id first, then the gRPC
// factory seam for an ExecGRPC entry), or nil+noop when neither resolves.
// Mirrors resolveArchiveTool.
func resolveThumbnailer(e *Entry) (ext.Thumbnailer, func()) {
	thumbnailerMu.RLock()
	if t, ok := thumbnailerInstances[e.Manifest.ID]; ok {
		thumbnailerMu.RUnlock()
		return t, func() {}
	}
	thumbnailerMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && thumbnailerFactory != nil {
		if t, release, err := thumbnailerFactory(e); err == nil {
			return t, release
		}
	}
	return nil, func() {}
}

// FindThumbnailerForExt resolves the Thumbnailer that claims fileExt (any
// case, with or without a leading dot): a registered in-process (built-in)
// instance first -- provided its owning registry Entry is still enabled --
// then, failing that, an enabled ExecGRPC Kind=Thumbnailer plugin whose
// self-reported Exts() contains it (a no-op path this wave: no gRPC factory
// is installed, see thumbnailerFactory's doc comment).
//
// remote reports whether the returned renderer would cross a gRPC boundary
// (mirrors FindArchiveToolForExt); always false in practice this wave.
// release must be called exactly once when the caller is done with the
// renderer.
func FindThumbnailerForExt(r *Registry, fileExt string) (t ext.Thumbnailer, remote bool, release func(), ok bool) {
	fileExt = normalizeThumbnailerExt(fileExt)
	if fileExt == "" {
		return nil, false, func() {}, false
	}

	thumbnailerMu.RLock()
	id, claimed := thumbnailerExtIndex[fileExt]
	thumbnailerMu.RUnlock()
	if claimed {
		if e, found := r.Get(id); found && e.Enabled() {
			thumbnailerMu.RLock()
			inst, ok2 := thumbnailerInstances[id]
			thumbnailerMu.RUnlock()
			if ok2 {
				return inst, false, func() {}, true
			}
		}
	}

	for _, e := range r.ByKind(ext.KindThumbnailer) {
		if e.execMode != ExecGRPC {
			continue
		}
		t, release := resolveThumbnailer(e)
		if t == nil {
			continue
		}
		matched := false
		for _, claim := range t.Exts() {
			if normalizeThumbnailerExt(claim) == fileExt {
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
