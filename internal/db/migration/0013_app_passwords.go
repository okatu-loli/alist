package migration

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the app_passwords table (v4 app-passwords feature,
// W3). The bare identifier "appPassword" pluralizes through NamingStrategy —
// "appPassword" -> "app_passwords" — so TablePrefix is honored without an
// explicit Tabler (same trick as 0010_labels / 0012_shares). The gorm tags
// mirror the live model (internal/model/app_password.go) exactly — especially
// the UserID lookup index — so AutoMigrate lands on identical index names; the
// schema-drift guard enforces they stay in sync.
type appPassword struct {
	ID        uint `gorm:"primaryKey"`
	UserID    uint `gorm:"index"`
	Name      string
	Hash      string
	Salt      string
	CreatedAt time.Time
	LastUsed  *time.Time
}

func init() {
	register(&gormigrate.Migration{
		ID: "0013_app_passwords",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&appPassword{})
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasTable(&appPassword{}) {
				return m.DropTable(&appPassword{})
			}
			return nil
		},
	})
}
