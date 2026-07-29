package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the search_nodes table (子项目 A, wave A2: the
// builtin.search.database index). The migration package must not import
// internal/model, so this snapshot carries the table definition verbatim; if
// the live model.SearchNode ever gains columns, a later migration adds them —
// this one stays pinned to the shape it created. The bare identifier
// "searchNode" pluralizes to "search_nodes" through NamingStrategy (honoring
// TablePrefix, e.g. "x_search_nodes"), so no explicit Tabler is needed — same
// convention as 0009's node. Parent is indexed because every query filters on a
// parent-prefix subtree.
type searchNode struct {
	Parent string `json:"parent" gorm:"index"`
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Size   int64  `json:"size"`
}

func init() {
	register(&gormigrate.Migration{
		ID: "0019_search_nodes",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&searchNode{})
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasTable(&searchNode{}) {
				return m.DropTable(&searchNode{})
			}
			return nil
		},
	})
}
