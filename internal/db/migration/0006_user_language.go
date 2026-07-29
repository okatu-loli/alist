package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Frozen local shape for the column added by this migration. Like 0005, the
// identifier "user" is already taken by the baseline (which creates the users
// table from a bare User struct), so a struct named userLanguage would
// pluralize to user_languages and miss the target. TablerWithNamer routes the
// base name "user" through NamingStrategy so pluralization + TablePrefix still
// apply and this AutoMigrate lands on the existing users (x_users) table.
type userLanguage struct {
	ID       uint   `gorm:"primaryKey"`
	Language string `gorm:"default:''"`
}

func (userLanguage) TableName(namer schema.Namer) string {
	return namer.TableName("user")
}

func init() {
	register(&gormigrate.Migration{
		ID: "0006_user_language",
		Migrate: func(tx *gorm.DB) error {
			// AutoMigrate only adds the missing language column to the existing
			// users table; other columns are left untouched.
			if err := tx.AutoMigrate(&userLanguage{}); err != nil {
				return err
			}
			// Backfill: empty preference means "follow Accept-Language". The
			// column default handles fresh inserts; explicitly level any row
			// still holding NULL to the empty string.
			return tx.Model(&userLanguage{}).
				Where("language IS NULL").
				Update("language", "").Error
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasColumn(&userLanguage{}, "language") {
				return m.DropColumn(&userLanguage{}, "language")
			}
			return nil
		},
	})
}
