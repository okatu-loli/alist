package migration

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the x_agents table (v4 remote-agent feature, design
// W12, wave W12-1: data-layer + admin-endpoint foundation only — no channel/
// SAS/task machinery yet). The bare identifier "agent" pluralizes through
// NamingStrategy -- "agent" -> "agents" -- so TablePrefix is honored without
// an explicit Tabler (same trick as 0010_labels / 0012_shares / 0013_app_
// passwords). The gorm tags mirror the live model (internal/model/agent.go)
// exactly -- especially the AgentID unique index -- so AutoMigrate lands on
// identical index names; the schema-drift guard enforces they stay in sync.
type agent struct {
	ID               uint   `gorm:"primaryKey"`
	AgentID          string `gorm:"uniqueIndex"`
	Name             string
	PubKey           string
	Enabled          bool
	StorageWhitelist string `gorm:"type:text"`
	LastSeen         *time.Time
	CreatedAt        time.Time
}

func init() {
	register(&gormigrate.Migration{
		ID: "0014_agents",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&agent{})
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasTable(&agent{}) {
				return m.DropTable(&agent{})
			}
			return nil
		},
	})
}
