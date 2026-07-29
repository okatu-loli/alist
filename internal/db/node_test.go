package db

import (
	"fmt"
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupNodeTestDB(t *testing.T) {
	t.Helper()
	dsn := fmt.Sprintf("file:node_test_%s?mode=memory&cache=shared", t.Name())
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	// Serialize access to the shared in-memory DB so the concurrency test
	// exercises the UNIQUE conflict re-query branch without SQLITE_BUSY.
	sqlDB, err := dB.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := dB.AutoMigrate(&model.Node{}); err != nil {
		t.Fatal(err)
	}
	old := db
	db = dB
	t.Cleanup(func() { db = old })
}

func TestEnsureNodeChainBuildsAndIsIdempotent(t *testing.T) {
	setupNodeTestDB(t)
	const sid = 3

	leaf, err := EnsureNodeChain(sid, "/a/b/c")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if leaf == 0 {
		t.Fatal("leaf id 0")
	}
	// root + a + b + c
	if n := countNodes(t); n != 4 {
		t.Fatalf("expected 4 nodes, got %d", n)
	}

	// Idempotent: same chain returns the same leaf, no new rows.
	leaf2, err := EnsureNodeChain(sid, "/a/b/c")
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if leaf2 != leaf {
		t.Fatalf("leaf changed on re-ensure: %d != %d", leaf2, leaf)
	}
	if n := countNodes(t); n != 4 {
		t.Fatalf("re-ensure created rows: %d", n)
	}

	// A sibling shares the ancestor chain (only one new row).
	if _, err := EnsureNodeChain(sid, "/a/b/d"); err != nil {
		t.Fatalf("ensure sibling: %v", err)
	}
	if n := countNodes(t); n != 5 {
		t.Fatalf("expected 5 nodes after sibling, got %d", n)
	}
}

func TestEnsureNodeChainRootAndSlash(t *testing.T) {
	setupNodeTestDB(t)
	const sid = 7

	root, err := EnsureNodeChain(sid, "/")
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	if n := countNodes(t); n != 1 {
		t.Fatalf("root should be a single node, got %d", n)
	}
	node, err := GetNode(root)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if node.ParentID != 0 || node.Name != "" || node.StorageID != sid {
		t.Fatalf("bad root node: %+v", node)
	}
	// "" and "/" and unclean paths all land on the same root.
	for _, p := range []string{"", "/", "//"} {
		got, err := EnsureNodeChain(sid, p)
		if err != nil {
			t.Fatalf("ensure %q: %v", p, err)
		}
		if got != root {
			t.Fatalf("path %q did not map to root: %d != %d", p, got, root)
		}
	}
}

func TestEnsureNodeChainSeparatesStorages(t *testing.T) {
	setupNodeTestDB(t)
	a, err := EnsureNodeChain(1, "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureNodeChain(2, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("same node id across storages")
	}
	// root(1)+x(1)+root(2)+x(2)
	if n := countNodes(t); n != 4 {
		t.Fatalf("expected 4 nodes, got %d", n)
	}
}

func TestEnsureNodeChainConcurrentUnique(t *testing.T) {
	setupNodeTestDB(t)
	const sid = 5
	const workers = 12

	var wg sync.WaitGroup
	ids := make([]uint, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = EnsureNodeChain(sid, "/shared/dir")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	for i := 1; i < workers; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("worker %d got different leaf: %d != %d", i, ids[i], ids[0])
		}
	}
	// root + shared + dir, no duplicates despite the race.
	if n := countNodes(t); n != 3 {
		t.Fatalf("expected 3 nodes, got %d", n)
	}
}

func TestGetNodeByChain(t *testing.T) {
	setupNodeTestDB(t)
	const sid = 9

	if _, found, err := GetNodeByChain(sid, "/a/b"); err != nil || found {
		t.Fatalf("expected not found on empty db: found=%v err=%v", found, err)
	}

	leaf, err := EnsureNodeChain(sid, "/a/b")
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := GetNodeByChain(sid, "/a/b")
	if err != nil || !found {
		t.Fatalf("expected found: found=%v err=%v", found, err)
	}
	if got != leaf {
		t.Fatalf("chain lookup mismatch: %d != %d", got, leaf)
	}
	// A partially-existing chain still reports not found for the missing tail.
	if _, found, err := GetNodeByChain(sid, "/a/b/c"); err != nil || found {
		t.Fatalf("expected not found for missing tail: found=%v err=%v", found, err)
	}
	// Root exists once any chain is built.
	if _, found, err := GetNodeByChain(sid, "/"); err != nil || !found {
		t.Fatalf("expected root found: found=%v err=%v", found, err)
	}
}

func TestUpdateNodeParentNameAndChildren(t *testing.T) {
	setupNodeTestDB(t)
	const sid = 11

	// Build /old/child and a fresh /new parent to move it under.
	if _, err := EnsureNodeChain(sid, "/old/child"); err != nil {
		t.Fatal(err)
	}
	newParent, err := EnsureNodeChain(sid, "/new")
	if err != nil {
		t.Fatal(err)
	}
	oldParent, _, err := GetNodeByChain(sid, "/old")
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := GetNodeByChain(sid, "/old/child")
	if err != nil {
		t.Fatal(err)
	}

	kids, err := GetChildNodes(oldParent)
	if err != nil || len(kids) != 1 || kids[0].ID != child {
		t.Fatalf("children of old: %v %v", kids, err)
	}

	// Rename + reparent the child in one edit.
	if err := UpdateNodeParentName(child, newParent, "moved"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, err := GetNodeByChain(sid, "/new/moved")
	if err != nil {
		t.Fatal(err)
	}
	if got != child {
		t.Fatalf("relocated node identity changed: %d != %d", got, child)
	}
	if kids, _ := GetChildNodes(oldParent); len(kids) != 0 {
		t.Fatalf("old parent still has children: %v", kids)
	}
	if kids, _ := GetChildNodes(newParent); len(kids) != 1 || kids[0].ID != child {
		t.Fatalf("new parent missing child: %v", kids)
	}
}

func countNodes(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.Node{}).Count(&n).Error; err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	return n
}
