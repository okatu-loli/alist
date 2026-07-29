package migration

import (
	"errors"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Frozen local shape of the nodes table (v4 permission stage 1, W1). The bare
// identifier "node" pluralizes to "nodes" through NamingStrategy (same trick
// as 0002), so no explicit Tabler that would bypass TablePrefix. The composite
// UNIQUE(storage_id, parent_id, name) is the identity index the node registry
// relies on for concurrency-safe find-or-create.
type node struct {
	ID             uint   `gorm:"primaryKey"`
	StorageID      uint   `gorm:"uniqueIndex:idx_node_identity,priority:1"`
	ParentID       uint   `gorm:"uniqueIndex:idx_node_identity,priority:2"`
	Name           string `gorm:"size:255;uniqueIndex:idx_node_identity,priority:3"`
	DriverObjectID string
}

// permissionNodeID / metaNodeID add the nullable node_id column (v4 stage 1)
// to the existing permissions/metas tables. Like 0008, the base identifiers
// are already taken, so route through NamingStrategy via TableName(namer) so
// AutoMigrate lands on the prefixed existing tables and only adds the column.
type permissionNodeID struct {
	ID     uint `gorm:"primaryKey"`
	NodeID uint `gorm:"index"`
}

func (permissionNodeID) TableName(namer schema.Namer) string {
	return namer.TableName("permission")
}

type metaNodeID struct {
	ID     uint `gorm:"primaryKey"`
	NodeID uint `gorm:"index"`
}

func (metaNodeID) TableName(namer schema.Namer) string {
	return namer.TableName("meta")
}

// permissionPostDrop / metaPostDrop describe the surviving indexed shape of the
// two tables AFTER the anchor triple is gone. sqlite's DropColumn rebuilds the
// whole table and does not carry the secondary indexes across, so an
// AutoMigrate against these shapes re-creates the ones the drop wiped (path /
// resource_type / node_id on permissions, path / node_id on metas) under their
// canonical auto-generated names — matching the live models exactly. On dialects
// whose DropColumn keeps indexes (MySQL) this AutoMigrate is a no-op.
//
// meta.path is a plain (non-unique) index: the v3 unique(path) key is retired in
// v4 because a node-anchored meta keeps a stale stored Path after a move, and a
// unique key would block reclaiming that path (dropAnchorTriple drops the old
// UNIQUE constraint). Meta uniqueness is enforced in op on the live derived path.
type permissionPostDrop struct {
	ID           uint   `gorm:"primaryKey"`
	ResourceType string `gorm:"size:16;index;default:fs"`
	Path         string `gorm:"index"`
	NodeID       uint   `gorm:"column:node_id;index"`
}

func (permissionPostDrop) TableName(namer schema.Namer) string {
	return namer.TableName("permission")
}

type metaPostDrop struct {
	ID     uint   `gorm:"primaryKey"`
	Path   string `gorm:"index"`
	NodeID uint   `gorm:"column:node_id;index"`
}

func (metaPostDrop) TableName(namer schema.Namer) string {
	return namer.TableName("meta")
}

func init() {
	register(&gormigrate.Migration{
		ID: "0009_nodes",
		Migrate: func(tx *gorm.DB) error {
			// Build the nodes registry, adopt existing relpath-anchored rows
			// into it (node_id backfill), then drop the now-redundant anchor
			// triple. v4 is a clean release with no compatibility window, so
			// stage 1 lands in one migration rather than a build-up wave and a
			// later teardown wave (the wave split was only to keep intermediate
			// builds green; the final schema carries no double truth).
			if err := tx.AutoMigrate(&node{}, &permissionNodeID{}, &metaNodeID{}); err != nil {
				return err
			}
			if err := backfillNodeIDs(tx); err != nil {
				return err
			}
			return dropAnchorTriple(tx)
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			for _, tbl := range []interface{}{&permissionNodeID{}, &metaNodeID{}} {
				if m.HasColumn(tbl, "node_id") {
					if err := m.DropColumn(tbl, "node_id"); err != nil {
						return err
					}
				}
			}
			if m.HasTable(&node{}) {
				if err := m.DropTable(&node{}); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

// dropAnchorTriple removes the storage_id/anchor/anchor_kind columns (and the
// composite anchor index) from permissions and metas once every row that needs
// one carries a node_id. Directory identity now lives entirely in the nodes
// table; the triple was the pre-node anchor and is dead. The composite index is
// dropped first so the sqlite table rebuild DropColumn performs is not left
// referencing a gone column.
func dropAnchorTriple(tx *gorm.DB) error {
	m := tx.Migrator()
	targets := []struct {
		tbl   interface{}
		index string
	}{
		{&permissionNodeID{}, "idx_permission_anchor"},
		{&metaNodeID{}, "idx_meta_anchor"},
	}
	for _, t := range targets {
		if m.HasIndex(t.tbl, t.index) {
			if err := m.DropIndex(t.tbl, t.index); err != nil {
				return err
			}
		}
		for _, col := range []string{"storage_id", "anchor", "anchor_kind"} {
			if m.HasColumn(t.tbl, col) {
				if err := m.DropColumn(t.tbl, col); err != nil {
					return err
				}
			}
		}
	}
	// Retire the v3 unique(path) key on metas: node-anchored rows keep a stale
	// stored Path after a move, so a unique key would block reclaiming that path.
	// Uniqueness now lives in op on the live derived path. The constraint is a
	// table-level UNIQUE (not a droppable named index), so DropConstraint rebuilds
	// the meta table without it (a no-op where already gone).
	metaTable := tx.NamingStrategy.TableName("meta")
	uniPath := tx.NamingStrategy.UniqueName(metaTable, "path")
	if m.HasConstraint(&metaPostDrop{}, uniPath) {
		if err := m.DropConstraint(&metaPostDrop{}, uniPath); err != nil {
			return err
		}
	}
	// Restore the secondary indexes the sqlite table rebuilds dropped.
	return tx.AutoMigrate(&permissionPostDrop{}, &metaPostDrop{})
}

// backfillNodeIDs adopts existing relpath-anchored permission/meta rows into
// the nodes table: for every row with storage_id≠0 and anchor_kind=relpath(0)
// still lacking a node, build its node chain and record the leaf ID. Scope
// permission rows are skipped (their truth is the role's PermissionScopes, not
// a node); all meta rows and manual permission rows are adopted. Idempotent:
// rows already carrying a node_id are left alone.
func backfillNodeIDs(tx *gorm.DB) error {
	permTable := tx.NamingStrategy.TableName("permission")
	metaTable := tx.NamingStrategy.TableName("meta")

	type row struct {
		ID        uint
		StorageID uint
		Anchor    string
	}

	adopt := func(table, extraWhere string, args ...interface{}) error {
		var rows []row
		q := tx.Table(table).
			Where("storage_id <> 0 AND anchor_kind = 0 AND (node_id = 0 OR node_id IS NULL)")
		if extraWhere != "" {
			q = q.Where(extraWhere, args...)
		}
		if err := q.Select("id", "storage_id", "anchor").Scan(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			nid, err := ensureNodeChainTx(tx, r.StorageID, r.Anchor)
			if err != nil {
				return err
			}
			if err := tx.Table(table).Where("id = ?", r.ID).Update("node_id", nid).Error; err != nil {
				return err
			}
		}
		return nil
	}

	// Scope permission rows stay unanchored (origin = 'scope').
	if err := adopt(permTable, "origin <> ?", "scope"); err != nil {
		return err
	}
	return adopt(metaTable, "")
}

// ensureNodeChainTx is the migration-local, single-connection equivalent of
// db.EnsureNodeChain: find-or-create the storage root plus one node per path
// segment and return the leaf ID. Anchors are already FixAndCleanPath'd when
// stored, so the split here is a plain "/" tokenization.
func ensureNodeChainTx(tx *gorm.DB, storageID uint, anchor string) (uint, error) {
	parentID, err := ensureNodeTx(tx, storageID, 0, "")
	if err != nil {
		return 0, err
	}
	anchor = strings.Trim(anchor, "/")
	if anchor == "" {
		return parentID, nil
	}
	for _, seg := range strings.Split(anchor, "/") {
		parentID, err = ensureNodeTx(tx, storageID, parentID, seg)
		if err != nil {
			return 0, err
		}
	}
	return parentID, nil
}

func ensureNodeTx(tx *gorm.DB, storageID, parentID uint, name string) (uint, error) {
	var existing node
	err := tx.Where("storage_id = ? AND parent_id = ? AND name = ?", storageID, parentID, name).
		First(&existing).Error
	if err == nil {
		return existing.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	n := node{StorageID: storageID, ParentID: parentID, Name: name}
	if err := tx.Create(&n).Error; err != nil {
		return 0, err
	}
	return n.ID, nil
}
