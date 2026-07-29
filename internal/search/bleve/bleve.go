// Package bleve is the second concrete SearchIndexer backend (子项目 A, wave A5):
// builtin.search.bleve, an in-process embedded full-text index built on
// blevesearch/bleve/v2. Like builtin.search.database it is registered as an
// independent, manageable built-in extension (Kinds:[SearchIndexer]) whose
// instance the `search` base resolves by its Config().Name ("bleve") when the
// extension is enabled; disabling the extension makes the base fall back to the
// index-less BFS.
//
// Ported from v3 internal/search/bleve/{init,search}.go, adapted to the v4
// ext.SearchIndexer contract. The index lives on disk at conf.Conf.BleveDir and
// is opened lazily on first use (the trusted instance is registered at bootstrap,
// before BleveDir is necessarily materialized). Bleve has no reliable per-path
// delete, so Get/Del return errs.NotSupport and Config().AutoUpdate is false —
// mirroring v3.
package bleve

import (
	"context"
	"os"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/blevesearch/bleve/v2"
	search2 "github.com/blevesearch/bleve/v2/search"
	query2 "github.com/blevesearch/bleve/v2/search/query"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// ExtensionID is the manageable built-in extension id under which this backend
// registers in the extension registry. It differs from the searcher Name
// (SearcherName): the base's search_index setting selects on the Name, while
// the enabled-gating is keyed on this extension id.
const ExtensionID = "builtin.search.bleve"

// SearcherName is the stable backend identity the search_index setting selects
// on and Config().Name reports.
const SearcherName = "bleve"

// Indexer implements ext.SearchIndexer over a bleve full-text index. The
// underlying bleve.Index is opened lazily (guarded by mu) so a bare
// &Indexer{} is a complete instance that materializes conf.Conf.BleveDir on the
// first operation.
type Indexer struct {
	mu     sync.Mutex
	bindex bleve.Index
}

// --- ext.Extension ---

// Manifest identifies the backend as an independent built-in of Kind
// SearchIndexer. The base surfaces its config through the shared INDEX settings
// group (same /@manage/setting/index route the search base uses); this manifest
// only names the extension for the registry.
func (*Indexer) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ExtensionID,
		Name:  map[string]string{"en": "Search: Bleve full-text", "zh": "搜索:Bleve 全文"},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}
}

func (*Indexer) Init(ctx context.Context, host ext.Host) error { return nil }

// Shutdown closes the open index (mirrors v3's Release).
func (ix *Indexer) Shutdown(ctx context.Context) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.closeLocked()
}

// --- ext.SearchIndexer ---

// Config reports the backend identity and that it does NOT support incremental
// auto-update: bleve cannot reliably delete by path (Get/Del are NotSupport),
// so the auto-update wave never routes through it. Mirrors v3.
func (*Indexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: SearcherName, AutoUpdate: false}
}

// Search runs a conjunction of a MatchQuery on the name field with an optional
// is_dir bool filter (Scope 1=dir, 2=file), paginated by Page/PerPage and
// sorted by name, then maps each hit back to ext.SearchNode. Ported verbatim
// from v3 search.go (no parent-prefix filter — v3 did none).
func (ix *Indexer) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	idx, err := ix.index()
	if err != nil {
		return nil, 0, err
	}

	var queries []query2.Query
	q := bleve.NewMatchQuery(req.Keywords)
	q.SetField("name")
	queries = append(queries, q)
	if req.Scope != 0 {
		isDir := req.Scope == 1
		queries = append(queries, bleve.NewBoolFieldQuery(isDir))
	}
	reqQuery := bleve.NewConjunctionQuery(queries...)
	sr := bleve.NewSearchRequest(reqQuery)
	sr.SortBy([]string{"name"})
	sr.From = (req.Page - 1) * req.PerPage
	sr.Size = req.PerPage
	sr.Fields = []string{"*"}

	results, err := idx.Search(sr)
	if err != nil {
		log.Errorf("bleve search error: %+v", err)
		return nil, 0, err
	}
	nodes, err := utils.SliceConvert(results.Hits, func(src *search2.DocumentMatch) (ext.SearchNode, error) {
		return ext.SearchNode{
			Parent: src.Fields["parent"].(string),
			Name:   src.Fields["name"].(string),
			IsDir:  src.Fields["is_dir"].(bool),
			Size:   int64(src.Fields["size"].(float64)),
		}, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return nodes, int64(results.Total), nil
}

// Index adds a single node under a fresh uuid doc id.
func (ix *Indexer) Index(ctx context.Context, node ext.SearchNode) error {
	idx, err := ix.index()
	if err != nil {
		return err
	}
	return idx.Index(uuid.NewString(), node)
}

// BatchIndex bulk-indexes many nodes, each under a fresh uuid doc id.
func (ix *Indexer) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	idx, err := ix.index()
	if err != nil {
		return err
	}
	batch := idx.NewBatch()
	for _, node := range nodes {
		if err := batch.Index(uuid.NewString(), node); err != nil {
			return err
		}
	}
	return idx.Batch(batch)
}

// Get is unsupported: bleve indexes by opaque uuid doc id, so there is no
// reliable per-parent lookup. Mirrors v3.
func (*Indexer) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	return nil, errs.NotSupport
}

// Del is unsupported: bleve has no reliable per-path (subtree) delete. Mirrors
// v3 — this is why Config().AutoUpdate is false.
func (*Indexer) Del(ctx context.Context, prefix string) error {
	return errs.NotSupport
}

// Clear drops the entire index: close it, remove the on-disk directory, then
// reopen a fresh empty index. Mirrors v3.
func (ix *Indexer) Clear(ctx context.Context) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if err := ix.closeLocked(); err != nil {
		return err
	}
	path := indexPath()
	log.Infof("bleve: removing old index at %s", path)
	if err := os.RemoveAll(path); err != nil {
		log.Errorf("bleve clear error: %+v", err)
		return err
	}
	b, err := openIndex(path)
	if err != nil {
		return err
	}
	ix.bindex = b
	return nil
}

// indexPath resolves where the bleve index lives: the admin-editable
// bleve_index_path DB setting (子项目 C, wave C4) when set, else the config-file
// default (conf.Conf.BleveDir, <data>/bleve). Read at open/Clear time so a
// settings change takes effect on the next open (Clear/re-init already reopens).
func indexPath() string {
	if p := setting.GetStr(conf.BleveIndexPath, ""); p != "" {
		return p
	}
	return conf.Conf.BleveDir
}

// index returns the open bleve index, opening (or creating) it lazily on first
// use under mu.
func (ix *Indexer) index() (bleve.Index, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.bindex != nil {
		return ix.bindex, nil
	}
	b, err := openIndex(indexPath())
	if err != nil {
		return nil, err
	}
	ix.bindex = b
	return b, nil
}

// closeLocked closes the open index (if any) and clears the pointer. Callers
// hold mu.
func (ix *Indexer) closeLocked() error {
	if ix.bindex == nil {
		return nil
	}
	err := ix.bindex.Close()
	ix.bindex = nil
	return err
}

// openIndex opens the bleve index at path, creating it with the SearchNode
// mapping when it does not yet exist. Ported verbatim from v3 init.go: is_dir is
// a boolean field, parent a text field, and name a KEYWORD field so name matches
// are on the whole token.
func openIndex(path string) (bleve.Index, error) {
	log.Debugf("bleve path: %s", path)
	idx, err := bleve.Open(path)
	if err == bleve.ErrorIndexPathDoesNotExist {
		log.Infof("bleve: creating new index at %s", path)
		indexMapping := bleve.NewIndexMapping()
		searchNodeMapping := bleve.NewDocumentMapping()
		searchNodeMapping.AddFieldMappingsAt("is_dir", bleve.NewBooleanFieldMapping())
		searchNodeMapping.AddFieldMappingsAt("parent", bleve.NewTextFieldMapping())
		searchNodeMapping.AddFieldMappingsAt("name", bleve.NewKeywordFieldMapping())
		indexMapping.AddDocumentMapping("SearchNode", searchNodeMapping)
		return bleve.New(path, indexMapping)
	}
	if err != nil {
		return nil, err
	}
	return idx, nil
}

var _ ext.SearchIndexer = (*Indexer)(nil)
