package migration

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the recovery_codes table (Phase C: internal TOTP
// second factor). The bare identifier "recoveryCode" pluralizes through
// NamingStrategy — "recoveryCode" -> "recovery_codes" — so TablePrefix is
// honored without an explicit Tabler (same trick as 0013_app_passwords). The
// gorm tags mirror the live model (internal/model/recovery_code.go) exactly —
// especially the UserID lookup index — so AutoMigrate lands on identical
// index names; the schema-drift guard enforces they stay in sync.
type recoveryCode struct {
	ID        uint `gorm:"primaryKey"`
	UserID    uint `gorm:"index"`
	Hash      string
	Salt      string
	CreatedAt time.Time
	UsedAt    *time.Time
}

func init() {
	register(&gormigrate.Migration{
		ID: "0020_totp_recovery_codes",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&recoveryCode{})
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasTable(&recoveryCode{}) {
				return m.DropTable(&recoveryCode{})
			}
			return nil
		},
	})
}
