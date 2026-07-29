package database

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// compile-time assertion: Indexer satisfies the full ext.SearchIndexer contract
// (which embeds ext.Extension).
var _ ext.SearchIndexer = (*Indexer)(nil)

func setupIndexerDB(t *testing.T) {
	t.Helper()
	dsn := "file:indexer_test_" + t.Name() + "?mode=memory&cache=shared"
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.SearchNode{}); err != nil {
		t.Fatal(err)
	}
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	t.Cleanup(func() { conf.Conf = oldConf })
}

func TestIndexerConfig(t *testing.T) {
	var ix Indexer
	cfg := ix.Config()
	if cfg.Name != "database_non_full_text" {
		t.Fatalf("Config().Name: want database_non_full_text, got %q", cfg.Name)
	}
	if !cfg.AutoUpdate {
		t.Fatal("Config().AutoUpdate: want true")
	}
	if id := ix.Manifest().ID; id != "builtin.search.database" {
		t.Fatalf("Manifest().ID: want builtin.search.database, got %q", id)
	}
	if k := ix.Manifest().Kinds; len(k) != 1 || k[0] != ext.KindSearchIndexer {
		t.Fatalf("Manifest().Kinds: want [SearchIndexer], got %v", k)
	}
}

func TestIndexerRoundTrip(t *testing.T) {
	setupIndexerDB(t)
	ctx := context.Background()
	ix := &Indexer{}

	// BatchIndex + Index, then Search through the backend.
	if err := ix.BatchIndex(ctx, []ext.SearchNode{
		{Parent: "/docs", Name: "readme report.txt", Size: 1},
		{Parent: "/docs/sub", Name: "notes.md", IsDir: false, Size: 2},
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if err := ix.Index(ctx, ext.SearchNode{Parent: "/docs", Name: "report solo.txt", Size: 3}); err != nil {
		t.Fatalf("index: %v", err)
	}

	got, total, err := ix.Search(ctx, ext.SearchReq{Parent: "/docs", Keywords: "report", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("search report: want 2, got total=%d rows=%d (%+v)", total, len(got), got)
	}

	// Get: direct children of /docs.
	children, err := ix.Get(ctx, "/docs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("get /docs: want 2, got %d", len(children))
	}

	// Del the /docs subtree, then Search returns nothing.
	if err := ix.Del(ctx, "/docs"); err != nil {
		t.Fatalf("del: %v", err)
	}
	_, total, err = ix.Search(ctx, ext.SearchReq{Parent: "/", Keywords: "report", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search after del: %v", err)
	}
	if total != 0 {
		t.Fatalf("after del: want 0, got %d", total)
	}

	// Re-seed then Clear.
	if err := ix.Index(ctx, ext.SearchNode{Parent: "/z", Name: "x.txt"}); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if err := ix.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	_, total, err = ix.Search(ctx, ext.SearchReq{Parent: "/", Keywords: "x", Scope: 0, Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("search after clear: %v", err)
	}
	if total != 0 {
		t.Fatalf("after clear: want 0, got %d", total)
	}
}
