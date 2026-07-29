package db

import (
	"fmt"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupSearchTestDB swaps the package db handle for a fresh in-memory database
// carrying only the search_nodes table. Mirrors setupLabelTestDB.
func setupSearchTestDB(t *testing.T) {
	t.Helper()
	dsn := fmt.Sprintf("file:search_test_%s?mode=memory&cache=shared", t.Name())
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
	old := db
	db = dB
	t.Cleanup(func() {
		db = old
		conf.Conf = oldConf
	})
}

func seed(t *testing.T) {
	t.Helper()
	nodes := []model.SearchNode{
		{Parent: "/a", Name: "report final.txt", IsDir: false, Size: 10},
		{Parent: "/a/b", Name: "report draft.txt", IsDir: false, Size: 20},
		{Parent: "/a/b", Name: "photos", IsDir: true, Size: 0},
		{Parent: "/c", Name: "report other.txt", IsDir: false, Size: 30},
	}
	if err := BatchCreateSearchNodes(&nodes); err != nil {
		t.Fatalf("batch create: %v", err)
	}
}

func req(parent, keywords string, scope int) ext.SearchReq {
	return ext.SearchReq{Parent: parent, Keywords: keywords, Scope: scope, Page: 1, PerPage: 100}
}

func TestCreateAndSearchSearchNode(t *testing.T) {
	setupSearchTestDB(t)
	if err := CreateSearchNode(&model.SearchNode{Parent: "/x", Name: "hello.txt", Size: 5}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, total, err := SearchNode(req("/", "hello", 0), false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].Name != "hello.txt" {
		t.Fatalf("want 1 hello.txt, got total=%d rows=%+v", total, got)
	}
}

func TestSearchSubtreeParentFilter(t *testing.T) {
	setupSearchTestDB(t)
	seed(t)
	// A node under /a/b matches a search rooted at "/a" (subtree) ...
	got, total, err := SearchNode(req("/a", "report", 0), false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// /a/report final.txt + /a/b/report draft.txt, but NOT /c/report other.txt.
	if total != 2 {
		t.Fatalf("subtree /a: want 2, got %d (%+v)", total, got)
	}
	for _, n := range got {
		if n.Name == "report other.txt" {
			t.Fatal("/c node leaked into /a subtree search")
		}
	}
	// ... but the same node under /a does NOT match a search rooted at "/c".
	_, total, err = SearchNode(req("/c", "report", 0), false)
	if err != nil {
		t.Fatalf("search /c: %v", err)
	}
	if total != 1 {
		t.Fatalf("subtree /c: want 1 (only /c/report other.txt), got %d", total)
	}
}

func TestSearchKeywordAND(t *testing.T) {
	setupSearchTestDB(t)
	seed(t)
	// Both keywords must be substrings of name: "report" AND "draft" -> only the draft.
	got, total, err := SearchNode(req("/", "report draft", 0), false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].Name != "report draft.txt" {
		t.Fatalf("keyword AND: want 1 draft, got total=%d %+v", total, got)
	}
}

func TestSearchScopeDirFile(t *testing.T) {
	setupSearchTestDB(t)
	seed(t)
	// Scope 1 = dir only.
	got, total, err := SearchNode(req("/", "", 1), false)
	if err != nil {
		t.Fatalf("search dir: %v", err)
	}
	if total != 1 || got[0].Name != "photos" || !got[0].IsDir {
		t.Fatalf("scope dir: want photos, got total=%d %+v", total, got)
	}
	// Scope 2 = file only.
	_, total, err = SearchNode(req("/", "", 2), false)
	if err != nil {
		t.Fatalf("search file: %v", err)
	}
	if total != 3 {
		t.Fatalf("scope file: want 3 files, got %d", total)
	}
}

func TestSearchPaging(t *testing.T) {
	setupSearchTestDB(t)
	seed(t)
	// 3 files matching "report", ordered by name asc, 2 per page.
	p1 := ext.SearchReq{Parent: "/", Keywords: "report", Scope: 0, Page: 1, PerPage: 2}
	got, total, err := SearchNode(p1, false)
	if err != nil {
		t.Fatalf("search p1: %v", err)
	}
	if total != 3 || len(got) != 2 {
		t.Fatalf("page1: want total=3 rows=2, got total=%d rows=%d", total, len(got))
	}
	// Name-asc order: "report draft.txt" < "report final.txt" < "report other.txt".
	if got[0].Name != "report draft.txt" || got[1].Name != "report final.txt" {
		t.Fatalf("page1 order: got %q, %q", got[0].Name, got[1].Name)
	}
	p2 := ext.SearchReq{Parent: "/", Keywords: "report", Scope: 0, Page: 2, PerPage: 2}
	got, _, err = SearchNode(p2, false)
	if err != nil {
		t.Fatalf("search p2: %v", err)
	}
	if len(got) != 1 || got[0].Name != "report other.txt" {
		t.Fatalf("page2: want 1 (report other.txt), got %+v", got)
	}
}

func TestDeleteSearchNodesByParent(t *testing.T) {
	setupSearchTestDB(t)
	// Build /a (dir node itself) + a descendant under /a/b.
	nodes := []model.SearchNode{
		{Parent: "/", Name: "a", IsDir: true},         // the /a node itself
		{Parent: "/a", Name: "keep-sibling", Size: 1}, // subtree
		{Parent: "/a/b", Name: "deep.txt", Size: 2},   // subtree
		{Parent: "/", Name: "other", IsDir: true},     // untouched
	}
	if err := BatchCreateSearchNodes(&nodes); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := DeleteSearchNodesByParent("/a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Subtree (/a/*, /a/b/*) AND the /a node itself (parent "/", name "a") are gone.
	var all []model.SearchNode
	if err := db.Find(&all).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(all) != 1 || all[0].Name != "other" {
		t.Fatalf("after delete /a: want only 'other', got %+v", all)
	}
}

func TestGetSearchNodesByParent(t *testing.T) {
	setupSearchTestDB(t)
	seed(t)
	got, err := GetSearchNodesByParent("/a/b")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("direct children of /a/b: want 2, got %d (%+v)", len(got), got)
	}
}

func TestClearSearchNodes(t *testing.T) {
	setupSearchTestDB(t)
	seed(t)
	if err := ClearSearchNodes(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	var count int64
	if err := db.Model(&model.SearchNode{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("after clear: want 0, got %d", count)
	}
}
