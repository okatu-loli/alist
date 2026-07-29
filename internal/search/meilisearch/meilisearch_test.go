package meilisearch

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/meilisearch/meilisearch-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// init stands up an in-memory settings DB so meiliSettings' setting.GetStr lookups
// work (C4 migrated the Meilisearch host/key/prefix from the config file to DB
// settings). Tests still inject a fake meiliClient, so no live Meilisearch server
// is ever contacted.
func init() {
	dB, err := gorm.Open(sqlite.Open("file:meili_backend_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

// seedMeiliSetting writes key=value to the settings store for the duration of the
// test, clearing it back to empty on cleanup.
func seedMeiliSetting(t *testing.T, key, value string) {
	t.Helper()
	item := model.SettingItem{Key: key, Value: value, Type: conf.TypeString, Group: model.INDEX}
	if err := op.SaveSettingItem(&item); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	t.Cleanup(func() {
		reset := model.SettingItem{Key: key, Value: "", Type: conf.TypeString, Group: model.INDEX}
		_ = op.SaveSettingItem(&reset)
	})
}

// compile-time assertion: Indexer satisfies the full ext.SearchIndexer contract
// (which embeds ext.Extension).
var _ ext.SearchIndexer = (*Indexer)(nil)

// fakeMeili is an in-memory meiliClient recording every call, so the whole
// backend is exercised with NO live Meilisearch server.
type fakeMeili struct {
	// setup
	exists          bool
	created         bool
	createdUID      string
	createdPK       string
	filterableAttrs []string // returned by get
	searchableAttrs []string
	maxTotalHits    int64
	setFilterable   [][]string
	setSearchable   [][]string
	setPagination   []int64

	// search
	searchHits         []interface{}
	searchTotal        int64
	lastSearchUID      string
	lastSearchKeywords string
	lastSearchReq      *meilisearch.SearchRequest

	// documents
	addedDocs    []interface{}
	docsByParent map[string][]map[string]interface{}

	// deletes
	deletedDocID    string
	deleteFilters   []string
	deleteAllCalled bool

	// tasks
	waited      []int64
	nextTaskUID int64
}

func (f *fakeMeili) indexExists(uid string) (bool, error) { return f.exists, nil }

func (f *fakeMeili) createIndex(ctx context.Context, uid, primaryKey string) error {
	f.created = true
	f.createdUID = uid
	f.createdPK = primaryKey
	return nil
}

func (f *fakeMeili) getFilterableAttributes(uid string) ([]string, error) {
	return f.filterableAttrs, nil
}
func (f *fakeMeili) getSearchableAttributes(uid string) ([]string, error) {
	return f.searchableAttrs, nil
}
func (f *fakeMeili) setFilterableAttributes(uid string, attrs []string) error {
	f.setFilterable = append(f.setFilterable, attrs)
	return nil
}
func (f *fakeMeili) setSearchableAttributes(uid string, attrs []string) error {
	f.setSearchable = append(f.setSearchable, attrs)
	return nil
}
func (f *fakeMeili) getMaxTotalHits(uid string) (int64, error) { return f.maxTotalHits, nil }
func (f *fakeMeili) setMaxTotalHits(uid string, n int64) error {
	f.setPagination = append(f.setPagination, n)
	return nil
}

func (f *fakeMeili) search(uid, keywords string, req *meilisearch.SearchRequest) ([]interface{}, int64, error) {
	f.lastSearchUID = uid
	f.lastSearchKeywords = keywords
	f.lastSearchReq = req
	return f.searchHits, f.searchTotal, nil
}

func (f *fakeMeili) addDocuments(uid string, documents interface{}) error {
	f.addedDocs = append(f.addedDocs, documents)
	return nil
}

func (f *fakeMeili) getDocumentsByParent(uid, parent string) ([]map[string]interface{}, error) {
	return f.docsByParent[parent], nil
}

func (f *fakeMeili) deleteDocument(uid, id string) (int64, error) {
	f.deletedDocID = id
	f.nextTaskUID++
	return f.nextTaskUID, nil
}

func (f *fakeMeili) deleteDocumentsByFilter(uid, filter string) (int64, error) {
	f.deleteFilters = append(f.deleteFilters, filter)
	f.nextTaskUID++
	return f.nextTaskUID, nil
}

func (f *fakeMeili) deleteAllDocuments(uid string) error {
	f.deleteAllCalled = true
	return nil
}

func (f *fakeMeili) waitForTask(ctx context.Context, taskUID int64) error {
	f.waited = append(f.waited, taskUID)
	return nil
}

// readyIndexer returns an Indexer wired to fake and pre-marked ready, so tests
// that are not about setup skip ensureReady's index/attribute plumbing.
func readyIndexer(fake *fakeMeili) *Indexer {
	return &Indexer{client: fake, uid: "alist", ready: true}
}

func doc(id, parent, name string, isDir bool, size float64) map[string]interface{} {
	return map[string]interface{}{
		"id":     id,
		"parent": parent,
		"name":   name,
		"is_dir": isDir,
		"size":   size,
	}
}

func TestConfigAndManifest(t *testing.T) {
	if SearcherName != "meilisearch" {
		t.Fatalf("SearcherName: want meilisearch, got %q", SearcherName)
	}
	if ExtensionID != "builtin.search.meilisearch" {
		t.Fatalf("ExtensionID: want builtin.search.meilisearch, got %q", ExtensionID)
	}
	var ix Indexer
	cfg := ix.Config()
	if cfg.Name != SearcherName {
		t.Fatalf("Config().Name: want %q, got %q", SearcherName, cfg.Name)
	}
	if !cfg.AutoUpdate {
		t.Fatal("Config().AutoUpdate: want true")
	}
	if id := ix.Manifest().ID; id != ExtensionID {
		t.Fatalf("Manifest().ID: want %q, got %q", ExtensionID, id)
	}
	if k := ix.Manifest().Kinds; len(k) != 1 || k[0] != ext.KindSearchIndexer {
		t.Fatalf("Manifest().Kinds: want [SearchIndexer], got %v", k)
	}
}

func TestSearchMapsHitsAndThreadsPaging(t *testing.T) {
	fake := &fakeMeili{
		searchHits: []interface{}{
			doc("a", "/docs", "report", false, 10),
			doc("b", "/docs", "report", true, 20),
		},
		searchTotal: 2,
	}
	ix := readyIndexer(fake)
	ctx := context.Background()

	nodes, total, err := ix.Search(ctx, ext.SearchReq{Keywords: "report", Scope: 0, Page: 3, PerPage: 7})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 2 || len(nodes) != 2 {
		t.Fatalf("search: want total=2 rows=2, got total=%d rows=%d", total, len(nodes))
	}
	// hit mapping
	if nodes[0] != (ext.SearchNode{Parent: "/docs", Name: "report", IsDir: false, Size: 10}) {
		t.Fatalf("hit[0] mapped wrong: %+v", nodes[0])
	}
	if nodes[1] != (ext.SearchNode{Parent: "/docs", Name: "report", IsDir: true, Size: 20}) {
		t.Fatalf("hit[1] mapped wrong: %+v", nodes[1])
	}
	// request threading
	if fake.lastSearchKeywords != "report" {
		t.Fatalf("keywords: want report, got %q", fake.lastSearchKeywords)
	}
	if fake.lastSearchReq.Page != 3 || fake.lastSearchReq.HitsPerPage != 7 {
		t.Fatalf("paging: want Page=3 HitsPerPage=7, got Page=%d HitsPerPage=%d",
			fake.lastSearchReq.Page, fake.lastSearchReq.HitsPerPage)
	}
	if len(fake.lastSearchReq.AttributesToSearchOn) != 1 || fake.lastSearchReq.AttributesToSearchOn[0] != "name" {
		t.Fatalf("AttributesToSearchOn: want [name], got %v", fake.lastSearchReq.AttributesToSearchOn)
	}
	// scope 0 => no filter
	if fake.lastSearchReq.Filter != nil {
		t.Fatalf("scope 0: want no filter, got %v", fake.lastSearchReq.Filter)
	}
}

func TestSearchScopeFilters(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		scope int
		want  string
	}{
		{1, "is_dir = true"},
		{2, "is_dir = false"},
	}
	for _, c := range cases {
		fake := &fakeMeili{}
		ix := readyIndexer(fake)
		if _, _, err := ix.Search(ctx, ext.SearchReq{Keywords: "x", Scope: c.scope, Page: 1, PerPage: 10}); err != nil {
			t.Fatalf("scope %d search: %v", c.scope, err)
		}
		got, _ := fake.lastSearchReq.Filter.(string)
		if got != c.want {
			t.Fatalf("scope %d: want filter %q, got %v", c.scope, c.want, fake.lastSearchReq.Filter)
		}
	}
}

func TestIndexAndBatchIndexWrapWithUUIDNoAwait(t *testing.T) {
	ctx := context.Background()

	// single Index
	fake := &fakeMeili{}
	ix := readyIndexer(fake)
	if err := ix.Index(ctx, ext.SearchNode{Parent: "/a", Name: "f", Size: 5}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(fake.addedDocs) != 1 {
		t.Fatalf("index: want 1 addDocuments call, got %d", len(fake.addedDocs))
	}
	docs := fake.addedDocs[0].([]*searchDocument)
	if len(docs) != 1 || docs[0].ID == "" {
		t.Fatalf("index doc: want 1 doc with non-empty id, got %+v", docs)
	}
	if docs[0].SearchNode != (ext.SearchNode{Parent: "/a", Name: "f", Size: 5}) {
		t.Fatalf("index doc payload wrong: %+v", docs[0].SearchNode)
	}
	if len(fake.waited) != 0 {
		t.Fatalf("index must NOT await: waited=%v", fake.waited)
	}

	// BatchIndex: unique non-empty ids, still no await
	fake = &fakeMeili{}
	ix = readyIndexer(fake)
	in := []ext.SearchNode{
		{Parent: "/p", Name: "1", Size: 1},
		{Parent: "/p", Name: "2", Size: 2},
		{Parent: "/p", Name: "3", Size: 3},
	}
	if err := ix.BatchIndex(ctx, in); err != nil {
		t.Fatalf("batch: %v", err)
	}
	docs = fake.addedDocs[0].([]*searchDocument)
	if len(docs) != 3 {
		t.Fatalf("batch: want 3 docs, got %d", len(docs))
	}
	seen := map[string]bool{}
	for i, d := range docs {
		if d.ID == "" {
			t.Fatalf("batch doc[%d] has empty id", i)
		}
		if seen[d.ID] {
			t.Fatalf("batch doc[%d] duplicate id %q", i, d.ID)
		}
		seen[d.ID] = true
		if d.SearchNode != in[i] {
			t.Fatalf("batch doc[%d] payload wrong: %+v", i, d.SearchNode)
		}
	}
	if len(fake.waited) != 0 {
		t.Fatalf("batch must NOT await: waited=%v", fake.waited)
	}
}

func TestDelDirRecursiveFilterAndAwait(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMeili{
		docsByParent: map[string][]map[string]interface{}{
			// parent of the target: "/a" holds the dir "sub"
			"/a": {doc("d1", "/a", "sub", true, 0)},
			// subtree of "/a/sub"
			"/a/sub":      {doc("f1", "/a/sub", "file1", false, 1), doc("d2", "/a/sub", "deep", true, 0)},
			"/a/sub/deep": {},
		},
	}
	ix := readyIndexer(fake)

	if err := ix.Del(ctx, "/a/sub"); err != nil {
		t.Fatalf("del dir: %v", err)
	}
	// recursive parent-IN filter over the dir and its descendant dir
	if len(fake.deleteFilters) != 1 {
		t.Fatalf("want 1 delete-by-filter, got %v", fake.deleteFilters)
	}
	want := "parent IN ['/a/sub','/a/sub/deep']"
	if fake.deleteFilters[0] != want {
		t.Fatalf("del filter: want %q, got %q", want, fake.deleteFilters[0])
	}
	// then the dir's own document is deleted
	if fake.deletedDocID != "d1" {
		t.Fatalf("del doc: want d1, got %q", fake.deletedDocID)
	}
	// both the filter-delete task and the doc-delete task were awaited
	if len(fake.waited) != 2 {
		t.Fatalf("del dir: want 2 awaited tasks (filter+doc), got %v", fake.waited)
	}
}

func TestDelFileSingleDocAndAwait(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMeili{
		docsByParent: map[string][]map[string]interface{}{
			"/a": {doc("x1", "/a", "file.txt", false, 9)},
		},
	}
	ix := readyIndexer(fake)

	if err := ix.Del(ctx, "/a/file.txt"); err != nil {
		t.Fatalf("del file: %v", err)
	}
	if len(fake.deleteFilters) != 0 {
		t.Fatalf("del file must not use a subtree filter, got %v", fake.deleteFilters)
	}
	if fake.deletedDocID != "x1" {
		t.Fatalf("del file: want doc x1, got %q", fake.deletedDocID)
	}
	if len(fake.waited) != 1 {
		t.Fatalf("del file: want 1 awaited task, got %v", fake.waited)
	}
}

func TestDelMissingDocFallsBackToSubtree(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMeili{
		docsByParent: map[string][]map[string]interface{}{
			"/a":       {}, // "ghost" not present under its parent
			"/a/ghost": {},
		},
	}
	ix := readyIndexer(fake)

	if err := ix.Del(ctx, "/a/ghost"); err != nil {
		t.Fatalf("del missing: %v", err)
	}
	// defensive path: still issues a subtree delete-by-filter
	if len(fake.deleteFilters) != 1 || fake.deleteFilters[0] != "parent IN ['/a/ghost']" {
		t.Fatalf("del missing: want subtree filter, got %v", fake.deleteFilters)
	}
	if fake.deletedDocID != "" {
		t.Fatalf("del missing: no single-doc delete expected, got %q", fake.deletedDocID)
	}
}

func TestGetReturnsChildren(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMeili{
		docsByParent: map[string][]map[string]interface{}{
			"/a": {doc("1", "/a", "x", false, 3), doc("2", "/a", "y", true, 0)},
		},
	}
	ix := readyIndexer(fake)
	got, err := ix.Get(ctx, "/a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 || got[0] != (ext.SearchNode{Parent: "/a", Name: "x", Size: 3}) {
		t.Fatalf("get mapped wrong: %+v", got)
	}
}

func TestClearDeletesAllDocuments(t *testing.T) {
	fake := &fakeMeili{}
	ix := readyIndexer(fake)
	if err := ix.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !fake.deleteAllCalled {
		t.Fatal("clear: DeleteAllDocuments not invoked")
	}
}

func TestSetupCreatesIndexAndSettings(t *testing.T) {
	fake := &fakeMeili{
		exists:          false, // index missing => must be created
		filterableAttrs: nil,   // attributes unset => must be set
		searchableAttrs: nil,
		maxTotalHits:    0, // != MaxInt => must be set
	}
	// ready=false so the first op triggers ensureReady.
	ix := &Indexer{client: fake, uid: "pfx_alist"}

	if err := ix.Clear(context.Background()); err != nil {
		t.Fatalf("first-use op: %v", err)
	}

	if !fake.created || fake.createdUID != "pfx_alist" || fake.createdPK != "id" {
		t.Fatalf("setup: index not created with PrimaryKey id: created=%v uid=%q pk=%q",
			fake.created, fake.createdUID, fake.createdPK)
	}
	if len(fake.setFilterable) != 1 || !equalStrs(fake.setFilterable[0], filterableAttributes) {
		t.Fatalf("setup: filterable attrs not set to %v, got %v", filterableAttributes, fake.setFilterable)
	}
	if len(fake.setSearchable) != 1 || !equalStrs(fake.setSearchable[0], searchableAttributes) {
		t.Fatalf("setup: searchable attrs not set to %v, got %v", searchableAttributes, fake.setSearchable)
	}
	if len(fake.setPagination) != 1 || fake.setPagination[0] != int64(model.MaxInt) {
		t.Fatalf("setup: pagination MaxTotalHits not set to MaxInt, got %v", fake.setPagination)
	}

	// idempotent: a second op does not re-run setup.
	fake.created = false
	fake.setFilterable = nil
	if err := ix.Clear(context.Background()); err != nil {
		t.Fatalf("second op: %v", err)
	}
	if fake.created || len(fake.setFilterable) != 0 {
		t.Fatal("setup: must not re-run once ready")
	}
}

func TestSetupSkipsWhenIndexAndSettingsPresent(t *testing.T) {
	fake := &fakeMeili{
		exists:          true,
		filterableAttrs: []string{"parent", "is_dir", "name"},
		searchableAttrs: []string{"name"},
		maxTotalHits:    int64(model.MaxInt),
	}
	ix := &Indexer{client: fake, uid: "alist"}
	if err := ix.Clear(context.Background()); err != nil {
		t.Fatalf("op: %v", err)
	}
	if fake.created {
		t.Fatal("setup: must not create an existing index")
	}
	if len(fake.setFilterable) != 0 || len(fake.setSearchable) != 0 || len(fake.setPagination) != 0 {
		t.Fatalf("setup: must not rewrite already-correct settings: f=%v s=%v p=%v",
			fake.setFilterable, fake.setSearchable, fake.setPagination)
	}
}

// TestMeiliConnConfigFromSettings pins the C4 migration: meiliSettings resolves
// the Meilisearch host/API key/index prefix from DB settings, and ensureReady
// derives the index uid from the meilisearch_index_prefix setting (so a fresh
// Indexer picks up the configured prefix on first use through the client seam).
func TestMeiliConnConfigFromSettings(t *testing.T) {
	seedMeiliSetting(t, conf.MeilisearchHost, "http://meili.internal:7700")
	seedMeiliSetting(t, conf.MeilisearchAPIKey, "s3kret")
	seedMeiliSetting(t, conf.MeilisearchIndexPrefix, "pfx_")

	cfg := meiliSettings()
	if cfg.Host != "http://meili.internal:7700" {
		t.Errorf("host: want http://meili.internal:7700, got %q", cfg.Host)
	}
	if cfg.APIKey != "s3kret" {
		t.Errorf("api key: want s3kret, got %q", cfg.APIKey)
	}
	if cfg.IndexPrefix != "pfx_" {
		t.Errorf("index prefix: want pfx_, got %q", cfg.IndexPrefix)
	}

	// A fresh Indexer (uid unset) resolves its uid from the prefix setting through
	// ensureReady on first use, observed via the injected fake client.
	fake := &fakeMeili{}
	ix := &Indexer{client: fake}
	if err := ix.Clear(context.Background()); err != nil {
		t.Fatalf("first-use op: %v", err)
	}
	if ix.uid != "pfx_alist" {
		t.Fatalf("uid: want pfx_alist (from meilisearch_index_prefix), got %q", ix.uid)
	}
	if fake.createdUID != "pfx_alist" {
		t.Fatalf("index created under wrong uid: want pfx_alist, got %q", fake.createdUID)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
