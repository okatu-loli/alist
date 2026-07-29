package bleve

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// init stands up an in-memory settings DB so indexPath's setting.GetStr lookup
// works (C4 migrated the bleve index path from the config file to a DB setting).
// The setting is left unseeded/empty by default, so indexPath falls back to
// conf.Conf.BleveDir exactly as before — the existing index tests are unaffected.
func init() {
	dB, err := gorm.Open(sqlite.Open("file:bleve_backend_test?mode=memory&cache=shared"),
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

// seedBleveSetting writes key=value to the settings store for the duration of the
// test, clearing it back to empty on cleanup.
func seedBleveSetting(t *testing.T, key, value string) {
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

// TestIndexPathFromSettings pins the C4 migration: indexPath resolves the bleve
// index directory from the admin-editable bleve_index_path DB setting when set,
// and falls back to the config-file default (conf.Conf.BleveDir) when it is blank.
func TestIndexPathFromSettings(t *testing.T) {
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig()
	conf.Conf.BleveDir = filepath.Join(t.TempDir(), "default-bleve")
	t.Cleanup(func() { conf.Conf = oldConf })

	// Blank setting → config-file default fallback.
	if got := indexPath(); got != conf.Conf.BleveDir {
		t.Fatalf("blank setting: want fallback %q, got %q", conf.Conf.BleveDir, got)
	}

	// Setting present → it wins over the config-file default.
	custom := filepath.Join(t.TempDir(), "custom-index")
	seedBleveSetting(t, conf.BleveIndexPath, custom)
	if got := indexPath(); got != custom {
		t.Fatalf("with setting: want %q, got %q", custom, got)
	}
}

// compile-time assertion: Indexer satisfies the full ext.SearchIndexer contract
// (which embeds ext.Extension).
var _ ext.SearchIndexer = (*Indexer)(nil)

// newIndexer points conf.Conf.BleveDir at a fresh temp dir for the duration of
// the test and returns a ready Indexer. The index is closed and conf restored on
// cleanup.
func newIndexer(t *testing.T) *Indexer {
	t.Helper()
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig()
	// Point at a not-yet-existing subpath (mirrors production, where BleveDir
	// defaults to <data>/bleve and is created on first open); an already-existing
	// empty dir would fail bleve.Open with "metadata missing".
	conf.Conf.BleveDir = filepath.Join(t.TempDir(), "bleve")
	ix := &Indexer{}
	t.Cleanup(func() {
		_ = ix.Shutdown(context.Background())
		conf.Conf = oldConf
	})
	return ix
}

func TestIndexerConfig(t *testing.T) {
	var ix Indexer
	cfg := ix.Config()
	if cfg.Name != SearcherName {
		t.Fatalf("Config().Name: want %q, got %q", SearcherName, cfg.Name)
	}
	if cfg.AutoUpdate {
		t.Fatal("Config().AutoUpdate: want false (bleve cannot per-path delete)")
	}
	if id := ix.Manifest().ID; id != ExtensionID {
		t.Fatalf("Manifest().ID: want %q, got %q", ExtensionID, id)
	}
	if k := ix.Manifest().Kinds; len(k) != 1 || k[0] != ext.KindSearchIndexer {
		t.Fatalf("Manifest().Kinds: want [SearchIndexer], got %v", k)
	}
}

func TestIndexAndSearch(t *testing.T) {
	ix := newIndexer(t)
	ctx := context.Background()

	// A file and a directory both named "report", plus an unrelated file.
	if err := ix.Index(ctx, ext.SearchNode{Parent: "/docs", Name: "report", IsDir: false, Size: 10}); err != nil {
		t.Fatalf("index file: %v", err)
	}
	if err := ix.Index(ctx, ext.SearchNode{Parent: "/docs", Name: "report", IsDir: true, Size: 20}); err != nil {
		t.Fatalf("index dir: %v", err)
	}
	if err := ix.Index(ctx, ext.SearchNode{Parent: "/docs", Name: "invoice", IsDir: false, Size: 30}); err != nil {
		t.Fatalf("index other: %v", err)
	}

	// Scope 0: both "report" nodes match, "invoice" does not.
	got, total, err := ix.Search(ctx, ext.SearchReq{Keywords: "report", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("search report scope 0: want 2, got total=%d rows=%d (%+v)", total, len(got), got)
	}
	for _, n := range got {
		if n.Name != "report" || n.Parent != "/docs" {
			t.Fatalf("hit mapped wrong: %+v", n)
		}
	}

	// Scope 1: dir only.
	got, total, err = ix.Search(ctx, ext.SearchReq{Keywords: "report", Scope: 1, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search dir scope: %v", err)
	}
	if total != 1 || len(got) != 1 || !got[0].IsDir {
		t.Fatalf("scope 1 (dir): want 1 dir, got total=%d (%+v)", total, got)
	}
	if got[0].Size != 20 {
		t.Fatalf("dir size mapped wrong: want 20, got %d", got[0].Size)
	}

	// Scope 2: file only.
	got, total, err = ix.Search(ctx, ext.SearchReq{Keywords: "report", Scope: 2, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search file scope: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].IsDir {
		t.Fatalf("scope 2 (file): want 1 file, got total=%d (%+v)", total, got)
	}
	if got[0].Size != 10 {
		t.Fatalf("file size mapped wrong: want 10, got %d", got[0].Size)
	}
}

func TestBatchIndexAndPaging(t *testing.T) {
	ix := newIndexer(t)
	ctx := context.Background()

	nodes := make([]ext.SearchNode, 5)
	for i := range nodes {
		nodes[i] = ext.SearchNode{Parent: "/photos", Name: "photo", IsDir: false, Size: int64(i + 1)}
	}
	if err := ix.BatchIndex(ctx, nodes); err != nil {
		t.Fatalf("batch: %v", err)
	}

	// Full result set.
	_, total, err := ix.Search(ctx, ext.SearchReq{Keywords: "photo", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if total != 5 {
		t.Fatalf("batch total: want 5, got %d", total)
	}

	// Paging: From/Size derived from Page/PerPage.
	p1, total, err := ix.Search(ctx, ext.SearchReq{Keywords: "photo", Scope: 0, Page: 1, PerPage: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if total != 5 || len(p1) != 2 {
		t.Fatalf("page 1: want total=5 rows=2, got total=%d rows=%d", total, len(p1))
	}
	p3, total, err := ix.Search(ctx, ext.SearchReq{Keywords: "photo", Scope: 0, Page: 3, PerPage: 2})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if total != 5 || len(p3) != 1 {
		t.Fatalf("page 3: want total=5 rows=1, got total=%d rows=%d", total, len(p3))
	}
}

func TestGetAndDelNotSupported(t *testing.T) {
	ix := newIndexer(t)
	ctx := context.Background()

	if _, err := ix.Get(ctx, "/docs"); !errors.Is(err, errs.NotSupport) {
		t.Fatalf("Get: want errs.NotSupport, got %v", err)
	}
	if err := ix.Del(ctx, "/docs"); !errors.Is(err, errs.NotSupport) {
		t.Fatalf("Del: want errs.NotSupport, got %v", err)
	}
}

func TestClearEmptiesIndex(t *testing.T) {
	ix := newIndexer(t)
	ctx := context.Background()

	if err := ix.Index(ctx, ext.SearchNode{Parent: "/z", Name: "keeper", Size: 1}); err != nil {
		t.Fatalf("index: %v", err)
	}
	_, total, err := ix.Search(ctx, ext.SearchReq{Keywords: "keeper", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search before clear: %v", err)
	}
	if total != 1 {
		t.Fatalf("before clear: want 1, got %d", total)
	}

	if err := ix.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}

	_, total, err = ix.Search(ctx, ext.SearchReq{Keywords: "keeper", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search after clear: %v", err)
	}
	if total != 0 {
		t.Fatalf("after clear: want 0, got %d", total)
	}

	// The index is still usable after Clear.
	if err := ix.Index(ctx, ext.SearchNode{Parent: "/z", Name: "fresh", Size: 2}); err != nil {
		t.Fatalf("index after clear: %v", err)
	}
	_, total, err = ix.Search(ctx, ext.SearchReq{Keywords: "fresh", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search fresh: %v", err)
	}
	if total != 1 {
		t.Fatalf("reindex after clear: want 1, got %d", total)
	}
}
