package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// user is a frozen, minimal shape naming the users table. The bare identifier
// "user" pluralizes through NamingStrategy to the prefixed "users" table (same
// trick as 0010's label structs — no explicit Tabler), so this migration stays
// a leaf: it imports neither internal/model nor internal/db. It exists only to
// resolve the table name for the base_path column drop; the column itself is
// addressed by raw name (v4's live model.User no longer declares it).
type user struct {
	ID uint `gorm:"primaryKey"`
}

// 0011 drops the retired users.base_path column. v4 removed model.User.BasePath
// (RBAC scope is now the only file boundary). The 0000 baseline still creates
// the column (frozen history) and live deployments already carry it, so this
// dedicated drop migration converges both the frozen baseline chain and existing
// databases onto the current model. On SQLite, DropColumn performs a table
// rebuild (handled by GORM).
func init() {
	register(&gormigrate.Migration{
		ID: "0011_drop_user_base_path",
		Migrate: func(tx *gorm.DB) error {
			if tx.Migrator().HasColumn(&user{}, "base_path") {
				return tx.Migrator().DropColumn(&user{}, "base_path")
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error { return nil },
	})
}
