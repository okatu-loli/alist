package migration

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shapes of the labels tables (v4 labels feature, W1). Bare
// identifiers pluralize through NamingStrategy — "label" -> "labels",
// "labelBinding" -> "label_bindings" — so TablePrefix is honored without an
// explicit Tabler (same trick as 0002/0009). The gorm index tags mirror the live
// models (internal/model/label.go) exactly so AutoMigrate lands on identical
// index names; the schema-drift guard enforces that they stay in sync.
type label struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"size:64;uniqueIndex;not null"`
	Color     string `gorm:"size:32"`
	CreatedAt time.Time
}

// labelBinding anchors a label onto a node. UNIQUE(label_id, node_id) makes a
// bind idempotent; the standalone node_id index backs the per-node decorator
// lookup, and label_id gets its own index for the by-label ("files with this
// label") query.
type labelBinding struct {
	ID        uint `gorm:"primaryKey"`
	LabelID   uint `gorm:"uniqueIndex:idx_label_binding_identity,priority:1;index"`
	NodeID    uint `gorm:"column:node_id;uniqueIndex:idx_label_binding_identity,priority:2;index"`
	CreatedAt time.Time
}

func init() {
	register(&gormigrate.Migration{
		ID: "0010_labels",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&label{}, &labelBinding{})
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			for _, tbl := range []interface{}{&labelBinding{}, &label{}} {
				if m.HasTable(tbl) {
					if err := m.DropTable(tbl); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
}
