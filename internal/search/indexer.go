package search

import (
	"context"
	"sync"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// This file adds the real-index base ALONGSIDE the index-less BFS in search.go
// (子项目 A, wave A1). It does NOT replace or touch the BFS: Search(ctx, user,
// Options) stays the always-available fallback. The base holds the currently
// active ext.SearchIndexer (resolved from the search_index setting via the host
// registry) and exposes a thin delegating API a later wave routes FsSearch
// through when a backend is active.
//
// Naming: the BFS entry point is already exported as Search, so the index query
// delegator here is named IndexSearch to avoid a hard collision — the two are
// deliberately distinct (BFS takes a *model.User and enforces per-child access
// itself; the index path stores full paths and re-authorizes hits one layer up).
var (
	instanceMu sync.RWMutex
	instance   ext.SearchIndexer

	// activeMode is the last mode Init was called with (the search_index
	// setting's value). It is consulted by ActiveIndexer's gRPC fallback: when
	// no in-process trusted instance resolved for mode (instance is nil), a
	// Kind=SearchIndexer gRPC plugin self-reporting this name may still exist
	// (extension.FindSearchIndexerByName, Phase E ring 1b) — that lookup needs
	// the mode string, which ActiveIndexer itself does not otherwise carry.
	activeMode string

	// backendExtID maps a backend's searcher Name (Config().Name, what the
	// search_index setting selects on, e.g. "database_non_full_text") to the
	// manageable extension id that owns it (e.g. "builtin.search.database").
	// The two deliberately differ, so this indirection lets ActiveIndexer gate
	// on whether the OWNING extension is enabled — disabling that extension
	// takes the backend offline (search falls back to BFS) with no manager hook.
	// Populated only by RegisterBackend and read under instanceMu.
	backendExtID = map[string]string{}
)

// RegisterBackend records a backend's searcherName→extensionID mapping AND
// registers its trusted in-process instance with the host registry in one step.
// A backend extension's bootstrap uses this instead of calling
// extension.RegisterTrustedSearchIndexerInstance directly, so the gating map is
// never left unpopulated (an unmapped active backend would be treated as always
// disabled by ActiveIndexer). A nil instance clears both the mapping and the
// host registration.
func RegisterBackend(searcherName, extensionID string, s ext.SearchIndexer) {
	instanceMu.Lock()
	if s == nil {
		delete(backendExtID, searcherName)
	} else {
		backendExtID[searcherName] = extensionID
	}
	instanceMu.Unlock()
	extension.RegisterTrustedSearchIndexerInstance(searcherName, s)
}

func init() {
	// Changing search_index re-resolves the active backend. Mirrors v3, where
	// the index setting drives which searcher is live.
	op.RegisterSettingItemHook(conf.SearchIndex, func(item *model.SettingItem) error {
		return Init(item.Value)
	})
}

// Init resolves the active SearchIndexer from mode (the search_index value).
// mode=="none" releases any active backend and leaves the base on the BFS
// fallback. Otherwise it looks the id up in the host registry: a hit becomes the
// active instance; a miss is NOT an error (a not-yet-registered or disabled
// backend simply means search falls back), so the instance is cleared and nil is
// returned. Switching to a different backend releases the previous one first.
//
// A build in progress blocks re-init (Running); the real guard lands in A3, so
// Running currently reports false and Init always proceeds.
func Init(mode string) error {
	if Running() {
		return errs.BuildIndexIsRunning
	}

	instanceMu.Lock()
	defer instanceMu.Unlock()

	activeMode = mode

	if mode == "" || mode == "none" {
		release()
		return nil
	}

	s, ok := extension.GetSearchIndexer(mode)
	if !ok {
		// Unregistered/disabled in-process backend: not an error — mode is
		// still recorded (activeMode, above), so ActiveIndexer's gRPC fallback
		// (extension.FindSearchIndexerByName) still gets a chance to resolve a
		// Kind=SearchIndexer gRPC plugin self-reporting this name.
		release()
		return nil
	}

	// Already active under the same identity: nothing to do.
	if instance != nil && instance.Config().Name == s.Config().Name {
		return nil
	}
	// Switching backends: drop the previous one before adopting the new.
	release()
	instance = s
	return nil
}

// release drops the reference to the active backend. The SearchIndexer instances
// are owned by the host extension registry (RegisterTrustedSearchIndexerInstance),
// which drives their real lifecycle (Init/Shutdown); the base only holds the
// "active" pointer, so releasing is just clearing that pointer. Callers hold
// instanceMu.
func release() {
	instance = nil
}

// Running reports whether an index build is in progress. The guard is the Quit
// atomic pointer (installed by BuildIndex for the duration of a build, cleared on
// exit): a non-nil pointer means a build holds it, so Init refuses to swap a
// backend mid-build.
func Running() bool {
	return Quit.Load() != nil
}

// ActiveIndexer returns the currently active SearchIndexer, or nil when none is
// active (search_index == none, the selected backend is not registered, or —
// the enabled-gate — the backend's OWNING extension is currently disabled). A
// nil return is the signal to fall back to the BFS.
//
// The gate resolves the owning extension id from backendExtID[Config().Name]
// and consults the registry: if the entry is absent or !Enabled(), the backend
// is treated as inactive even though the instance pointer is still set. This
// gates at RESOLUTION time, so an admin disabling builtin.search.database makes
// search fall back to BFS on the very next query, without any manager→search
// callback.
//
// When no in-process trusted instance is active for the current mode (Init
// found nothing under extension.GetSearchIndexer, e.g. the selected backend
// is a third-party plugin with no in-process registration), this falls back
// to extension.FindSearchIndexerByName (Phase E ring 1b) — a Kind=
// SearchIndexer gRPC plugin self-reporting activeMode as its Config().Name.
// That lookup is itself a self-validating cache (entry-liveness + enabled +
// not-crashed, re-checked on every call), so calling it on every
// ActiveIndexer() is the same "just always there" permanence the in-process
// path gets from its own instance pointer, without ActiveIndexer needing to
// separately track a release func.
func ActiveIndexer() ext.SearchIndexer {
	instanceMu.RLock()
	s := instance
	mode := activeMode
	var extID string
	if s != nil {
		extID = backendExtID[s.Config().Name]
	}
	instanceMu.RUnlock()
	if s != nil {
		if e, ok := extension.DefaultRegistry.Get(extID); !ok || !e.Enabled() {
			return nil
		}
		return s
	}
	if mode == "" || mode == "none" {
		return nil
	}
	if g, ok := extension.FindSearchIndexerByName(extension.DefaultRegistry, mode); ok {
		return g
	}
	return nil
}

// Config reports the active backend's config, or the zero SearchIndexerConfig
// when none is active (honoring the enabled-gate via ActiveIndexer).
func Config() ext.SearchIndexerConfig {
	s := ActiveIndexer()
	if s == nil {
		return ext.SearchIndexerConfig{}
	}
	return s.Config()
}

// IndexSearch queries the active index. With no active backend (none selected,
// unregistered, or its extension disabled) it returns errs.SearchNotAvailable
// so the caller can fall back to the BFS. Routed through ActiveIndexer so the
// enabled-gate applies uniformly.
func IndexSearch(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	s := ActiveIndexer()
	if s == nil {
		return nil, 0, errs.SearchNotAvailable
	}
	return s.Search(ctx, req)
}

// Index adds/updates a single node in the active index, or returns
// errs.SearchNotAvailable when none is active (enabled-gate via ActiveIndexer).
func Index(ctx context.Context, node ext.SearchNode) error {
	s := ActiveIndexer()
	if s == nil {
		return errs.SearchNotAvailable
	}
	return s.Index(ctx, node)
}

// BatchIndex adds/updates many nodes in the active index, or returns
// errs.SearchNotAvailable when none is active (enabled-gate via ActiveIndexer).
func BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	s := ActiveIndexer()
	if s == nil {
		return errs.SearchNotAvailable
	}
	return s.BatchIndex(ctx, nodes)
}

// Del removes the subtree at prefix from the active index, or returns
// errs.SearchNotAvailable when none is active (enabled-gate via ActiveIndexer).
func Del(ctx context.Context, prefix string) error {
	s := ActiveIndexer()
	if s == nil {
		return errs.SearchNotAvailable
	}
	return s.Del(ctx, prefix)
}

// Clear drops the entire active index, or returns errs.SearchNotAvailable when
// none is active (enabled-gate via ActiveIndexer).
func Clear(ctx context.Context) error {
	s := ActiveIndexer()
	if s == nil {
		return errs.SearchNotAvailable
	}
	return s.Clear(ctx)
}
