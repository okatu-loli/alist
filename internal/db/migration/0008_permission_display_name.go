package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Frozen local shape for the column added by this migration. Like 0005/0006,
// the identifier "permission" is already taken by 0002 (which created the
// table from a bare identifier), so TablerWithNamer routes the base name
// "permission" through NamingStrategy so pluralization + TablePrefix still
// land this AutoMigrate on the existing permissions (x_permissions) table.
type permissionDisplayName struct {
	ID          uint   `gorm:"primaryKey"`
	DisplayName string `gorm:"default:''"`
}

func (permissionDisplayName) TableName(namer schema.Namer) string {
	return namer.TableName("permission")
}

func init() {
	register(&gormigrate.Migration{
		ID: "0008_permission_display_name",
		Migrate: func(tx *gorm.DB) error {
			// AutoMigrate only adds the missing display_name column to the
			// existing permissions table; other columns are left untouched.
			if err := tx.AutoMigrate(&permissionDisplayName{}); err != nil {
				return err
			}
			// Backfill: pre-existing rows have no display name (they fall back
			// to Name for display). The column default handles fresh inserts;
			// explicitly level any row still holding NULL to the empty string.
			return tx.Model(&permissionDisplayName{}).
				Where("display_name IS NULL").
				Update("display_name", "").Error
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasColumn(&permissionDisplayName{}, "display_name") {
				return m.DropColumn(&permissionDisplayName{}, "display_name")
			}
			return nil
		},
	})
}
