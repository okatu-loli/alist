// Package database is the first concrete SearchIndexer backend (子项目 A, wave
// A2): builtin.search.database, an in-process, dependency-free index built on
// SQL LIKE over the x_search_nodes table. It is registered as an independent,
// manageable built-in extension (Kinds:[SearchIndexer]) whose instance the
// `search` base resolves by its Config().Name ("database_non_full_text") when
// the extension is enabled; disabling the extension makes the base fall back to
// the index-less BFS.
package database

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// ExtensionID is the manageable built-in extension id under which this backend
// registers in the extension registry. It differs from the searcher Name
// (SearcherName): the base's search_index setting selects on the Name, while
// the enabled-gating is keyed on this extension id.
const ExtensionID = "builtin.search.database"

// SearcherName is the stable backend identity the search_index setting selects
// on and Config().Name reports. "non_full_text" marks this as the SQL-LIKE
// (substring) variant; a future FTS backend would register a distinct name over
// the same table.
const SearcherName = "database_non_full_text"

// Indexer implements ext.SearchIndexer over the db search-node layer. It is
// stateless: every method delegates to internal/db, so a bare &Indexer{} is a
// complete, ready instance.
type Indexer struct{}

// --- ext.Extension ---

// Manifest identifies the backend as an independent built-in of Kind
// SearchIndexer. The base surfaces its config through the shared INDEX settings
// group (same /@manage/setting/index route the search base uses); this manifest
// only names the extension for the registry.
func (*Indexer) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ExtensionID,
		Name:  map[string]string{"en": "Search: Database index", "zh": "搜索:数据库索引"},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}
}

func (*Indexer) Init(ctx context.Context, host ext.Host) error { return nil }
func (*Indexer) Shutdown(ctx context.Context) error            { return nil }

// --- ext.SearchIndexer ---

// Config reports the backend identity and that it supports incremental
// auto-update (single-row Index/Del land immediately in the table).
func (*Indexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: SearcherName, AutoUpdate: true}
}

// Search runs the SQL-LIKE subtree/keyword query (useFullText=false: this is
// the database_non_full_text backend) and maps the rows back to ext.SearchNode.
func (*Indexer) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	rows, total, err := db.SearchNode(req, false)
	if err != nil {
		return nil, 0, err
	}
	out := make([]ext.SearchNode, len(rows))
	for i, r := range rows {
		out[i] = toExt(r)
	}
	return out, total, nil
}

// Index adds/updates a single node.
func (*Indexer) Index(ctx context.Context, node ext.SearchNode) error {
	m := toModel(node)
	return db.CreateSearchNode(&m)
}

// BatchIndex bulk-inserts many nodes (CreateInBatches under the hood).
func (*Indexer) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	if len(nodes) == 0 {
		return nil
	}
	rows := make([]model.SearchNode, len(nodes))
	for i, n := range nodes {
		rows[i] = toModel(n)
	}
	return db.BatchCreateSearchNodes(&rows)
}

// Get returns the direct children indexed under parent.
func (*Indexer) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	rows, err := db.GetSearchNodesByParent(parent)
	if err != nil {
		return nil, err
	}
	out := make([]ext.SearchNode, len(rows))
	for i, r := range rows {
		out[i] = toExt(r)
	}
	return out, nil
}

// Del removes the subtree at prefix (descendants + the node itself).
func (*Indexer) Del(ctx context.Context, prefix string) error {
	return db.DeleteSearchNodesByParent(prefix)
}

// Clear drops the entire index.
func (*Indexer) Clear(ctx context.Context) error {
	return db.ClearSearchNodes()
}

func toModel(n ext.SearchNode) model.SearchNode {
	return model.SearchNode{Parent: n.Parent, Name: n.Name, IsDir: n.IsDir, Size: n.Size}
}

func toExt(n model.SearchNode) ext.SearchNode {
	return ext.SearchNode{Parent: n.Parent, Name: n.Name, IsDir: n.IsDir, Size: n.Size}
}
