package db

import (
	"fmt"
	stdpath "path"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// This is the persistence layer of the builtin.search.database backend (子项目
// A, wave A2), ported from v3 internal/db/searchnode.go. It stores each indexed
// object once (keyed by parent + name) and answers SQL-LIKE subtree/keyword
// queries. The index is built as admin over the full path space; per-hit
// access control happens one layer up in the search base — nothing here makes
// an access decision.

// whereInParent builds the subtree predicate for a search rooted at parent:
// every node whose Parent is parent itself OR sits below parent/. Root "/"
// matches everything (no filter). Mirrors v3.
func whereInParent(parent string) *gorm.DB {
	if parent == "/" {
		return db.Where("1 = 1")
	}
	return db.Where(fmt.Sprintf("%s LIKE ?", columnName("parent")),
		fmt.Sprintf("%s/%%", parent)).
		Or(fmt.Sprintf("%s = ?", columnName("parent")), parent)
}

// CreateSearchNode inserts a single indexed node.
func CreateSearchNode(node *model.SearchNode) error {
	return db.Create(node).Error
}

// BatchCreateSearchNodes bulk-inserts nodes in batches of 1000 (the build path
// fans out through this to amortize per-write cost).
func BatchCreateSearchNodes(nodes *[]model.SearchNode) error {
	return db.CreateInBatches(nodes, 1000).Error
}

// DeleteSearchNodesByParent removes the subtree at path: first every descendant
// (the parent-prefix predicate), then the node itself (matched by its own
// parent=dir(path) AND name=base(path)). Mirrors v3.
func DeleteSearchNodesByParent(path string) error {
	path = utils.FixAndCleanPath(path)
	err := db.Where(whereInParent(path)).Delete(&model.SearchNode{}).Error
	if err != nil {
		return err
	}
	dir, name := stdpath.Split(path)
	// stdpath.Split keeps a trailing slash on dir ("/diff/b" -> "/diff/"), but
	// parents are stored slash-free ("/diff"), so the node-itself match must
	// strip it — otherwise the leaf survives for every non-root path. The root
	// dir "/" is preserved (parents there are stored as "/").
	if dir != "/" {
		dir = strings.TrimSuffix(dir, "/")
	}
	return db.Where(fmt.Sprintf("%s = ? AND %s = ?",
		columnName("parent"), columnName("name")),
		dir, name).Delete(&model.SearchNode{}).Error
}

// ClearSearchNodes drops every indexed node.
func ClearSearchNodes() error {
	return db.Where("1 = 1").Delete(&model.SearchNode{}).Error
}

// GetSearchNodesByParent returns the direct children indexed under parent.
func GetSearchNodesByParent(parent string) ([]model.SearchNode, error) {
	var nodes []model.SearchNode
	if err := db.Where(fmt.Sprintf("%s = ?",
		columnName("parent")), parent).Find(&nodes).Error; err != nil {
		return nil, err
	}
	return nodes, nil
}

// SearchNode runs one index query: subtree filter (whereInParent), per-keyword
// substring AND on name (whitespace-split), optional dir/file scope, name-asc
// ordering, and Page/PerPage paging, plus a COUNT for the total. useFullText is
// carried for the future `database` FTS backend; A2's database_non_full_text
// always passes false, so only the SQL-LIKE branch is exercised here.
func SearchNode(req ext.SearchReq, useFullText bool) ([]model.SearchNode, int64, error) {
	keywordsClause := db.Where("1 = 1")
	for _, keyword := range strings.Fields(req.Keywords) {
		keywordsClause = keywordsClause.Where(fmt.Sprintf("%s LIKE ?", columnName("name")),
			fmt.Sprintf("%%%s%%", keyword))
	}
	searchDB := db.Model(&model.SearchNode{}).Where(whereInParent(req.Parent)).Where(keywordsClause)

	if req.Scope != 0 {
		isDir := req.Scope == 1
		searchDB = searchDB.Where(fmt.Sprintf("%s = ?", columnName("is_dir")), isDir)
	}

	var count int64
	if err := searchDB.Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get search items count")
	}
	var files []model.SearchNode
	if err := searchDB.Order(fmt.Sprintf("%s asc", columnName("name"))).
		Offset((req.Page - 1) * req.PerPage).Limit(req.PerPage).
		Find(&files).Error; err != nil {
		return nil, 0, err
	}
	return files, count, nil
}
