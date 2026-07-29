package ext

import (
	"context"
	"time"
)

// SearchIndexer is the extension point behind file search's real-index backends
// (design: 子项目 A). The v4 core ships an index-less, on-the-fly BFS search as
// the always-available fallback; a SearchIndexer adds a persistent index that
// the `search` base can build once (as admin, over the full path space) and then
// query cheaply — database (SQL LIKE), bleve (embedded full-text) and
// meilisearch (external service) each implement this interface as an independent
// built-in extension.
//
// Like ext.AuthNMethod, the contract is defined here so an out-of-process (gRPC)
// third-party backend is a stable future seam; this wave wires only in-process
// TRUSTED instances the host itself authors (RegisterTrustedSearchIndexerInstance
// on the host side). The host alone owns the security invariant — the index is
// built as admin and stores full paths, and the `search` base re-authorizes every
// hit against the requesting user before returning it — so an indexer never makes
// an access decision; it only stores and matches nodes.
type SearchIndexer interface {
	Extension

	// Config reports the backend's identity (Name, matched against the
	// `search_index` setting to resolve the active backend) and whether it
	// supports incremental auto-update.
	Config() SearchIndexerConfig

	// Search returns the nodes matching req plus the total hit count. The
	// returned nodes carry full virtual paths (path.Join(Parent, Name)); the
	// base re-authorizes each one per requesting user — Search itself performs
	// no access control.
	Search(ctx context.Context, req SearchReq) ([]SearchNode, int64, error)

	// Index adds or updates a single node.
	Index(ctx context.Context, node SearchNode) error

	// BatchIndex adds or updates many nodes at once (the build path fans out
	// through this to amortize per-write cost).
	BatchIndex(ctx context.Context, nodes []SearchNode) error

	// Get returns the direct children previously indexed under parent.
	Get(ctx context.Context, parent string) ([]SearchNode, error)

	// Del removes every node at or below prefix (a subtree delete).
	Del(ctx context.Context, prefix string) error

	// Clear drops the entire index.
	Clear(ctx context.Context) error
}

// SearchIndexerConfig is a backend's self-description. Name is the stable id the
// `search_index` setting selects on; AutoUpdate advertises whether the backend
// can apply incremental updates (consulted by the later auto-update wave).
type SearchIndexerConfig struct {
	Name       string `json:"name"`
	AutoUpdate bool   `json:"auto_update"`
}

// SearchNode is one indexed filesystem object. It mirrors v3's model.SearchNode:
// there is deliberately NO path field — the virtual path is derived as
// path.Join(Parent, Name) — so the index stores each object once, keyed by its
// parent directory and name.
type SearchNode struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Size   int64  `json:"size"`
}

// SearchReq parameterizes one index query. Scope selects object kind (0 all,
// 1 dir only, 2 file only); Page/PerPage paginate the surviving hits.
type SearchReq struct {
	Parent   string `json:"parent"`
	Keywords string `json:"keywords"`
	Scope    int    `json:"scope"` // 0 all, 1 dir, 2 file
	Page     int    `json:"page"`
	PerPage  int    `json:"per_page"`
}

// IndexProgress is the build-progress snapshot the host persists (as the
// index_progress setting) and the frontend polls: how many objects have been
// indexed, whether the build finished, when it last completed, and the last
// error if any.
type IndexProgress struct {
	ObjCount     uint64     `json:"obj_count"`
	IsDone       bool       `json:"is_done"`
	LastDoneTime *time.Time `json:"last_done_time"`
	Error        string     `json:"error"`
}
