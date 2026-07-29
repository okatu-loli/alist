package migration

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the shares table (v4 share feature, W1). The bare
// identifier "share" pluralizes through NamingStrategy — "share" -> "shares" —
// so TablePrefix is honored without an explicit Tabler (same trick as
// 0010_labels). The gorm tags mirror the live model (internal/model/share.go)
// exactly — especially the Token uniqueIndex and the NodeID/CreatorID lookup
// indexes — so AutoMigrate lands on identical index names; the schema-drift
// guard enforces they stay in sync.
type share struct {
	ID           uint   `gorm:"primaryKey"`
	Token        string `gorm:"size:64;uniqueIndex;not null"`
	NodeID       uint   `gorm:"index"`
	CreatorID    uint   `gorm:"index"`
	PasswordHash string `gorm:"size:128"`
	ExpiresAt    *time.Time
	Disabled     bool
	CreatedAt    time.Time
}

func init() {
	register(&gormigrate.Migration{
		ID: "0012_shares",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&share{})
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasTable(&share{}) {
				return m.DropTable(&share{})
			}
			return nil
		},
	})
}
