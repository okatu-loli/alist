package db

import (
	"strings"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// splitRel splits a storage-internal relative path into its clean segments.
// "/" (the storage root) yields no segments. The path is FixAndCleanPath'd
// first so callers may pass any shape.
func splitRel(relPath string) []string {
	relPath = utils.FixAndCleanPath(relPath)
	if relPath == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(relPath, "/"), "/")
}

// ensureNode finds or creates the single node (storageID, parentID, name),
// concurrency-safe via the UNIQUE(storage_id, parent_id, name) index: two
// racing callers both attempt the insert, the loser's INSERT is turned into a
// no-op by OnConflict DoNothing, and the row is then re-read so both observe
// the same ID. On a fresh insert the ID is populated directly.
func ensureNode(storageID, parentID uint, name string) (uint, error) {
	n := model.Node{StorageID: storageID, ParentID: parentID, Name: name}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&n).Error; err != nil {
		return 0, errors.WithStack(err)
	}
	if n.ID != 0 {
		return n.ID, nil
	}
	// Conflict (row already existed): OnConflict DoNothing left ID unset, so
	// re-read the existing row.
	var existing model.Node
	if err := db.Where("storage_id = ? AND parent_id = ? AND name = ?", storageID, parentID, name).
		First(&existing).Error; err != nil {
		return 0, errors.WithStack(err)
	}
	return existing.ID, nil
}

// EnsureNodeChain finds or creates every node on relPath within storageID —
// the storage root (storage_id, 0, "") plus one row per segment — and returns
// the leaf node's ID. relPath is FixAndCleanPath'd; "/" returns the root node.
// Concurrency-safe: each segment goes through ensureNode.
func EnsureNodeChain(storageID uint, relPath string) (uint, error) {
	parentID, err := ensureNode(storageID, 0, "")
	if err != nil {
		return 0, err
	}
	for _, seg := range splitRel(relPath) {
		parentID, err = ensureNode(storageID, parentID, seg)
		if err != nil {
			return 0, err
		}
	}
	return parentID, nil
}

// GetNodeByChain resolves relPath to its node without creating anything.
// found is false (nil error) when any segment is missing.
func GetNodeByChain(storageID uint, relPath string) (nodeID uint, found bool, err error) {
	parentID, found, err := findNode(storageID, 0, "")
	if err != nil || !found {
		return 0, found, err
	}
	for _, seg := range splitRel(relPath) {
		parentID, found, err = findNode(storageID, parentID, seg)
		if err != nil || !found {
			return 0, found, err
		}
	}
	return parentID, true, nil
}

func findNode(storageID, parentID uint, name string) (uint, bool, error) {
	var n model.Node
	err := db.Where("storage_id = ? AND parent_id = ? AND name = ?", storageID, parentID, name).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errors.WithStack(err)
	}
	return n.ID, true, nil
}

// GetNode loads a node by ID.
func GetNode(id uint) (*model.Node, error) {
	var n model.Node
	if err := db.First(&n, id).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return &n, nil
}

// UpdateNodeParentName repoints a node at a new parent and/or renames it — the
// one-row edit behind move/rename relocation (W2): the whole subtree follows
// for free because children reference this node by parent_id. Uses an explicit
// column map so a zero newParentID (reparenting to a storage root) is written
// rather than skipped as a gorm zero value.
func UpdateNodeParentName(id, newParentID uint, newName string) error {
	return errors.WithStack(db.Model(&model.Node{}).Where("id = ?", id).
		Updates(map[string]interface{}{"parent_id": newParentID, "name": newName}).Error)
}

// GetChildNodes returns the direct children of parentID. Callers pass a real
// node ID (subtree descent); parentID 0 would collect every storage root
// across all storages and is not a meaningful query.
func GetChildNodes(parentID uint) ([]model.Node, error) {
	var nodes []model.Node
	if err := db.Where("parent_id = ?", parentID).Find(&nodes).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return nodes, nil
}
