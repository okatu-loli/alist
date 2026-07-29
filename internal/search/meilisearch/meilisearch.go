// Package meilisearch is the third concrete SearchIndexer backend (子项目 A, wave
// A6): builtin.search.meilisearch, an out-of-process full-text index backed by an
// external Meilisearch service. Like builtin.search.database and
// builtin.search.bleve it is registered as an independent, manageable built-in
// extension (Kinds:[SearchIndexer]) whose instance the `search` base resolves by
// its Config().Name ("meilisearch") when the extension is enabled; disabling the
// extension makes the base fall back to the index-less BFS.
//
// Ported from v3 internal/search/meilisearch/{init,search}.go, adapted to the v4
// ext.SearchIndexer contract AND to an injectable client seam: the Indexer talks
// to Meilisearch only through the small meiliClient interface, whose real
// implementation wraps meilisearch.NewClient({conf.Meilisearch.Host, APIKey}).
// Tests inject a fake meiliClient, so the whole backend is unit-testable with no
// live Meilisearch server. The index is set up lazily on first use (the trusted
// instance is registered at bootstrap, before conf.Meilisearch is necessarily
// populated), mirroring v3's factory setup and bleve's lazy open.
package meilisearch

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/google/uuid"
	"github.com/meilisearch/meilisearch-go"
)

// ExtensionID is the manageable built-in extension id under which this backend
// registers in the extension registry. It differs from the searcher Name
// (SearcherName): the base's search_index setting selects on the Name, while
// the enabled-gating is keyed on this extension id.
const ExtensionID = "builtin.search.meilisearch"

// SearcherName is the stable backend identity the search_index setting selects
// on and Config().Name reports.
const SearcherName = "meilisearch"

// filterableAttributes / searchableAttributes are the index settings the backend
// requires (ported from v3). parent/is_dir/name must be filterable so Search's
// is_dir filter and Del's parent filters work; name must be searchable so the
// keyword query hits it.
var (
	filterableAttributes = []string{"parent", "is_dir", "name"}
	searchableAttributes = []string{"name"}
)

// searchDocument is the on-wire index document: the ext.SearchNode fields plus a
// synthetic uuid primary key ("id"). The embedded SearchNode flattens into the
// JSON (parent/name/is_dir/size), mirroring v3.
type searchDocument struct {
	ID string `json:"id"`
	ext.SearchNode
}

// meiliClient is the injectable seam over the external Meilisearch service: it
// captures ONLY the handful of operations the Indexer performs, so a fake
// implementation makes the backend unit-testable with no live server. Each
// method is a single Meilisearch round-trip; all index-selection / filtering /
// recursion / mapping logic lives in the Indexer above it. The real
// implementation (realMeiliClient) wraps meilisearch.NewClient.
type meiliClient interface {
	// indexExists reports whether the index uid already exists. The real impl
	// maps Meilisearch's "index_not_found" API error to (false, nil); any other
	// error propagates.
	indexExists(uid string) (bool, error)
	// createIndex creates uid with the given primary key and BLOCKS until the
	// creation task succeeds (errors if the task ends non-succeeded) — v3 awaits
	// index creation before using it.
	createIndex(ctx context.Context, uid, primaryKey string) error

	// getFilterableAttributes / getSearchableAttributes return the index's
	// current attribute lists (nil when unset), so the Indexer can skip the
	// update when they already cover what it needs (v3 parity).
	getFilterableAttributes(uid string) ([]string, error)
	getSearchableAttributes(uid string) ([]string, error)
	// setFilterableAttributes / setSearchableAttributes overwrite the lists.
	// v3 does not await these settings tasks, so neither do we.
	setFilterableAttributes(uid string, attrs []string) error
	setSearchableAttributes(uid string, attrs []string) error
	// getMaxTotalHits / setMaxTotalHits read and write the pagination cap so a
	// large result set is not silently truncated by Meilisearch's default.
	getMaxTotalHits(uid string) (int64, error)
	setMaxTotalHits(uid string, n int64) error

	// search runs a keyword query; hits are raw Meilisearch hit objects (each a
	// map[string]any) and total is the reported TotalHits. Passing the
	// *meilisearch.SearchRequest through verbatim lets the Indexer own the
	// request shaping (searchable attrs, paging, is_dir filter).
	search(uid, keywords string, req *meilisearch.SearchRequest) (hits []interface{}, total int64, err error)

	// addDocuments enqueues documents WITHOUT awaiting the task (BatchIndex is
	// fire-and-forget on the build hot path, per v3).
	addDocuments(uid string, documents interface{}) error
	// getDocumentsByParent returns the raw documents whose parent == parent.
	getDocumentsByParent(uid, parent string) ([]map[string]interface{}, error)

	// deleteDocument / deleteDocumentsByFilter enqueue a delete and return the
	// task uid so the Indexer can await it (Del is synchronous in v3).
	deleteDocument(uid, id string) (taskUID int64, err error)
	deleteDocumentsByFilter(uid, filter string) (taskUID int64, err error)
	// deleteAllDocuments clears the index; v3 does not await this, so neither do
	// we.
	deleteAllDocuments(uid string) error

	// waitForTask blocks until the task completes, returning an error if it did
	// not end in the succeeded state.
	waitForTask(ctx context.Context, taskUID int64) error
}

// Indexer implements ext.SearchIndexer against an external Meilisearch service
// through the injected meiliClient. The index is set up lazily on first use
// (guarded by mu): a bare &Indexer{} builds its real client from conf.Meilisearch
// and ensures the index/attributes exist before the first operation.
type Indexer struct {
	mu     sync.Mutex
	client meiliClient
	ready  bool
	uid    string
}

// --- ext.Extension ---

// Manifest identifies the backend as an independent built-in of Kind
// SearchIndexer, mirroring database.go/bleve.go. The base surfaces its config
// through the shared INDEX settings group; this manifest only names the
// extension for the registry.
func (*Indexer) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ExtensionID,
		Name:  map[string]string{"en": "Search: Meilisearch", "zh": "搜索:Meilisearch"},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}
}

func (*Indexer) Init(ctx context.Context, host ext.Host) error { return nil }
func (*Indexer) Shutdown(ctx context.Context) error            { return nil }

// --- ext.SearchIndexer ---

// Config reports the backend identity and that it supports incremental
// auto-update (single-row Index/Del land in the index, awaited on delete).
func (*Indexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: SearcherName, AutoUpdate: true}
}

// Search runs a keyword query restricted to the name attribute, paginated by
// Page/PerPage, with an optional is_dir filter (Scope 1=dir, 2=file), then maps
// each raw hit back to ext.SearchNode. Ported from v3 search.go.
func (ix *Indexer) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	if err := ix.ensureReady(ctx); err != nil {
		return nil, 0, err
	}
	mReq := &meilisearch.SearchRequest{
		AttributesToSearchOn: searchableAttributes,
		Page:                 int64(req.Page),
		HitsPerPage:          int64(req.PerPage),
	}
	if req.Scope != 0 {
		mReq.Filter = fmt.Sprintf("is_dir = %v", req.Scope == 1)
	}
	hits, total, err := ix.client.search(ix.uid, req.Keywords, mReq)
	if err != nil {
		return nil, 0, err
	}
	nodes, err := utils.SliceConvert(hits, func(src interface{}) (ext.SearchNode, error) {
		return nodeFromMap(src.(map[string]interface{})), nil
	})
	if err != nil {
		return nil, 0, err
	}
	return nodes, total, nil
}

// Index adds a single node (delegates to BatchIndex, as v3 does).
func (ix *Indexer) Index(ctx context.Context, node ext.SearchNode) error {
	return ix.BatchIndex(ctx, []ext.SearchNode{node})
}

// BatchIndex bulk-adds many nodes, each wrapped in a searchDocument under a
// fresh uuid primary key. It does NOT await the task (v3 fire-and-forget).
func (ix *Indexer) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	if err := ix.ensureReady(ctx); err != nil {
		return err
	}
	documents, _ := utils.SliceConvert(nodes, func(src ext.SearchNode) (*searchDocument, error) {
		return &searchDocument{
			ID:         uuid.NewString(),
			SearchNode: src,
		}, nil
	})
	return ix.client.addDocuments(ix.uid, documents)
}

// Get returns the direct children indexed under parent.
func (ix *Indexer) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	if err := ix.ensureReady(ctx); err != nil {
		return nil, err
	}
	raw, err := ix.client.getDocumentsByParent(ix.uid, parent)
	if err != nil {
		return nil, err
	}
	docs := docsFromMaps(raw)
	out := make([]ext.SearchNode, len(docs))
	for i, d := range docs {
		out[i] = d.SearchNode
	}
	return out, nil
}

// Del removes the node at prefix (and, when it is a directory, its whole
// subtree). Ported verbatim from v3: resolve the doc by parent+name; if it is a
// dir (or cannot be resolved — defensive), delete its children by a recursive
// parent-IN filter first, then delete the doc itself; every delete is awaited.
func (ix *Indexer) Del(ctx context.Context, prefix string) error {
	if err := ix.ensureReady(ctx); err != nil {
		return err
	}
	prefix = utils.FixAndCleanPath(prefix)
	dir, name := path.Split(prefix)
	raw, err := ix.client.getDocumentsByParent(ix.uid, dir[:len(dir)-1])
	if err != nil {
		return err
	}
	var document *searchDocument
	for _, v := range docsFromMaps(raw) {
		if v.Name == name {
			document = v
			break
		}
	}
	if document == nil {
		// Defensive (v3): the resolved doc may be a folder whose own row is
		// absent; try deleting its children by prefix anyway.
		return ix.delDirChild(ctx, prefix)
	}
	if document.IsDir {
		if err := ix.delDirChild(ctx, prefix); err != nil {
			return err
		}
	}
	taskUID, err := ix.client.deleteDocument(ix.uid, document.ID)
	if err != nil {
		return err
	}
	return ix.client.waitForTask(ctx, taskUID)
}

// Clear drops every document in the index (v3 does not await this).
func (ix *Indexer) Clear(ctx context.Context) error {
	if err := ix.ensureReady(ctx); err != nil {
		return err
	}
	return ix.client.deleteAllDocuments(ix.uid)
}

// --- internals ---

// delDirChild deletes an entire subtree: it DFS-collects every descendant parent
// path (getParentsByPrefix), quotes/escapes them, and issues one
// DeleteDocumentsByFilter("parent IN [...]"), awaiting the task. Ported from v3
// DelDirChild.
func (ix *Indexer) delDirChild(ctx context.Context, prefix string) error {
	dfs, err := ix.getParentsByPrefix(ctx, utils.FixAndCleanPath(prefix))
	if err != nil {
		return err
	}
	utils.SliceReplace(dfs, func(src string) string {
		return "'" + strings.ReplaceAll(src, "'", "\\'") + "'"
	})
	filter := fmt.Sprintf("parent IN [%s]", strings.Join(dfs, ","))
	taskUID, err := ix.client.deleteDocumentsByFilter(ix.uid, filter)
	if err != nil {
		return err
	}
	return ix.client.waitForTask(ctx, taskUID)
}

// getParentsByPrefix returns parent and every directory parent path beneath it,
// depth-first, honoring ctx cancellation. Ported verbatim from v3.
func (ix *Indexer) getParentsByPrefix(ctx context.Context, parent string) ([]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		parents := []string{parent}
		raw, err := ix.client.getDocumentsByParent(ix.uid, parent)
		if err != nil {
			return nil, err
		}
		for _, node := range docsFromMaps(raw) {
			if node.IsDir {
				arr, err := ix.getParentsByPrefix(ctx, path.Join(node.Parent, node.Name))
				if err != nil {
					return nil, err
				}
				parents = append(parents, arr...)
			}
		}
		return parents, nil
	}
}

// ensureReady performs v3's factory setup lazily on first use, under mu: build
// the real client if none was injected, resolve the index uid from
// conf.Meilisearch, create the index (awaited) when absent, then ensure the
// filterable/searchable attributes and the pagination cap are in place. On
// success ready is latched so later operations skip straight through; on failure
// it is retried on the next call.
func (ix *Indexer) ensureReady(ctx context.Context) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.ready {
		return nil
	}
	if ix.client == nil {
		ix.client = newRealClient()
	}
	if ix.uid == "" {
		ix.uid = meiliSettings().IndexPrefix + "alist"
	}
	uid := ix.uid

	exists, err := ix.client.indexExists(uid)
	if err != nil {
		return err
	}
	if !exists {
		if err := ix.client.createIndex(ctx, uid, "id"); err != nil {
			return err
		}
	}

	fa, err := ix.client.getFilterableAttributes(uid)
	if err != nil {
		return err
	}
	if fa == nil || !utils.SliceAllContains(fa, filterableAttributes...) {
		if err := ix.client.setFilterableAttributes(uid, filterableAttributes); err != nil {
			return err
		}
	}

	sa, err := ix.client.getSearchableAttributes(uid)
	if err != nil {
		return err
	}
	if sa == nil || !utils.SliceAllContains(sa, searchableAttributes...) {
		if err := ix.client.setSearchableAttributes(uid, searchableAttributes); err != nil {
			return err
		}
	}

	mth, err := ix.client.getMaxTotalHits(uid)
	if err != nil {
		return err
	}
	if mth != int64(model.MaxInt) {
		if err := ix.client.setMaxTotalHits(uid, int64(model.MaxInt)); err != nil {
			return err
		}
	}

	ix.ready = true
	return nil
}

// nodeFromMap maps a raw Meilisearch document/hit to an ext.SearchNode. JSON
// numbers decode as float64, so size is asserted through float64 (v3 parity).
func nodeFromMap(m map[string]interface{}) ext.SearchNode {
	return ext.SearchNode{
		Parent: m["parent"].(string),
		Name:   m["name"].(string),
		IsDir:  m["is_dir"].(bool),
		Size:   int64(m["size"].(float64)),
	}
}

// docsFromMaps maps raw documents (which carry the primary-key "id") to
// searchDocuments so the delete path can address them by id.
func docsFromMaps(raw []map[string]interface{}) []*searchDocument {
	out := make([]*searchDocument, len(raw))
	for i, m := range raw {
		out[i] = &searchDocument{
			ID:         m["id"].(string),
			SearchNode: nodeFromMap(m),
		}
	}
	return out
}

// --- real client ---

// realMeiliClient is the production meiliClient: a thin wrapper over the
// Meilisearch Go SDK. Each method is a single SDK call, ported directly from v3.
type realMeiliClient struct {
	c *meilisearch.Client
}

// meiliConnConfig is the resolved Meilisearch connection config. Since 子项目 C
// wave C4 it is read from DB settings (meilisearch_host / meilisearch_api_key /
// meilisearch_index_prefix), which are authoritative over the config-file
// conf.Conf.Meilisearch fields (kept only as back-compat defaults).
type meiliConnConfig struct {
	Host        string
	APIKey      string
	IndexPrefix string
}

// meiliSettings reads the Meilisearch connection config from DB settings at
// call time. The tool (re)builds its client and resolves its index uid lazily on
// first use / after a reset, so reading here is enough for a settings change to
// take effect on the next reconnect — no restart needed. The host default
// mirrors conf.DefaultConfig().Meilisearch.Host.
func meiliSettings() meiliConnConfig {
	return meiliConnConfig{
		Host:        setting.GetStr(conf.MeilisearchHost, "http://localhost:7700"),
		APIKey:      setting.GetStr(conf.MeilisearchAPIKey, ""),
		IndexPrefix: setting.GetStr(conf.MeilisearchIndexPrefix, ""),
	}
}

// newRealClient builds a realMeiliClient from the Meilisearch settings (read
// lazily at first use, so bootstrap can register the trusted instance before the
// settings store is populated).
func newRealClient() meiliClient {
	cfg := meiliSettings()
	return &realMeiliClient{
		c: meilisearch.NewClient(meilisearch.ClientConfig{
			Host:   cfg.Host,
			APIKey: cfg.APIKey,
		}),
	}
}

func (r *realMeiliClient) indexExists(uid string) (bool, error) {
	_, err := r.c.GetIndex(uid)
	if err != nil {
		var mErr *meilisearch.Error
		if errors.As(err, &mErr) && mErr.MeilisearchApiError.Code == "index_not_found" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *realMeiliClient) createIndex(ctx context.Context, uid, primaryKey string) error {
	task, err := r.c.CreateIndex(&meilisearch.IndexConfig{Uid: uid, PrimaryKey: primaryKey})
	if err != nil {
		return err
	}
	return r.waitForTask(ctx, task.TaskUID)
}

func (r *realMeiliClient) getFilterableAttributes(uid string) ([]string, error) {
	attrs, err := r.c.Index(uid).GetFilterableAttributes()
	if err != nil {
		return nil, err
	}
	if attrs == nil {
		return nil, nil
	}
	return *attrs, nil
}

func (r *realMeiliClient) setFilterableAttributes(uid string, attrs []string) error {
	_, err := r.c.Index(uid).UpdateFilterableAttributes(&attrs)
	return err
}

func (r *realMeiliClient) getSearchableAttributes(uid string) ([]string, error) {
	attrs, err := r.c.Index(uid).GetSearchableAttributes()
	if err != nil {
		return nil, err
	}
	if attrs == nil {
		return nil, nil
	}
	return *attrs, nil
}

func (r *realMeiliClient) setSearchableAttributes(uid string, attrs []string) error {
	_, err := r.c.Index(uid).UpdateSearchableAttributes(&attrs)
	return err
}

func (r *realMeiliClient) getMaxTotalHits(uid string) (int64, error) {
	p, err := r.c.Index(uid).GetPagination()
	if err != nil {
		return 0, err
	}
	return p.MaxTotalHits, nil
}

func (r *realMeiliClient) setMaxTotalHits(uid string, n int64) error {
	_, err := r.c.Index(uid).UpdatePagination(&meilisearch.Pagination{MaxTotalHits: n})
	return err
}

func (r *realMeiliClient) search(uid, keywords string, req *meilisearch.SearchRequest) ([]interface{}, int64, error) {
	resp, err := r.c.Index(uid).Search(keywords, req)
	if err != nil {
		return nil, 0, err
	}
	return resp.Hits, resp.TotalHits, nil
}

func (r *realMeiliClient) addDocuments(uid string, documents interface{}) error {
	_, err := r.c.Index(uid).AddDocuments(documents)
	return err
}

func (r *realMeiliClient) getDocumentsByParent(uid, parent string) ([]map[string]interface{}, error) {
	var result meilisearch.DocumentsResult
	err := r.c.Index(uid).GetDocuments(&meilisearch.DocumentsQuery{
		Filter: fmt.Sprintf("parent = '%s'", strings.ReplaceAll(parent, "'", "\\'")),
		Limit:  int64(model.MaxInt),
	}, &result)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (r *realMeiliClient) deleteDocument(uid, id string) (int64, error) {
	task, err := r.c.Index(uid).DeleteDocument(id)
	if err != nil {
		return 0, err
	}
	return task.TaskUID, nil
}

func (r *realMeiliClient) deleteDocumentsByFilter(uid, filter string) (int64, error) {
	task, err := r.c.Index(uid).DeleteDocumentsByFilter(filter)
	if err != nil {
		return 0, err
	}
	return task.TaskUID, nil
}

func (r *realMeiliClient) deleteAllDocuments(uid string) error {
	_, err := r.c.Index(uid).DeleteAllDocuments()
	return err
}

func (r *realMeiliClient) waitForTask(ctx context.Context, taskUID int64) error {
	task, err := r.c.WaitForTask(taskUID, meilisearch.WaitParams{
		Context:  ctx,
		Interval: time.Second,
	})
	if err != nil {
		return err
	}
	if task.Status != meilisearch.TaskStatusSucceeded {
		return fmt.Errorf("meilisearch task %d ended with status %s", taskUID, task.Status)
	}
	return nil
}

var _ ext.SearchIndexer = (*Indexer)(nil)
