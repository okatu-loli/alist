package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Frozen local shapes for the columns added by this migration. Unlike 0002
// (which creates the permissions/roles tables via default pluralization of a
// bare identifier), here we must ADD columns to those ALREADY-EXISTING tables.
// The identifiers permission/role are taken by 0002, and a plain TableName()
// would return the name verbatim and bypass NamingStrategy's TablePrefix
// (breaking the "x_"-prefixed migration tests). TablerWithNamer routes the
// base name through the namer, so pluralization + prefix still apply and these
// structs land on permissions/roles (x_permissions/x_roles under the tests).
type permissionResType struct {
	ID           uint   `gorm:"primaryKey"`
	ResourceType string `gorm:"size:16;index;default:fs"`
}

func (permissionResType) TableName(namer schema.Namer) string {
	return namer.TableName("permission")
}

type roleProtected struct {
	ID        uint `gorm:"primaryKey"`
	Protected bool `gorm:"default:false"`
}

func (roleProtected) TableName(namer schema.Namer) string {
	return namer.TableName("role")
}

func init() {
	register(&gormigrate.Migration{
		ID: "0005_resource_type",
		Migrate: func(tx *gorm.DB) error {
			// AutoMigrate only adds the missing columns/index to the existing
			// tables; other columns are left untouched.
			if err := tx.AutoMigrate(&permissionResType{}, &roleProtected{}); err != nil {
				return err
			}
			// Backfill: every pre-existing permission row is a filesystem row.
			// The column default handles fresh inserts; explicitly backfill any
			// row still holding the zero value or NULL.
			if err := tx.Model(&permissionResType{}).
				Where("resource_type = ? OR resource_type IS NULL", "").
				Update("resource_type", "fs").Error; err != nil {
				return err
			}
			// admin/guest are built-in roles: mark them protected.
			return tx.Model(&roleProtected{}).
				Where("name IN ?", []string{"admin", "guest"}).
				Update("protected", true).Error
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasColumn(&permissionResType{}, "resource_type") {
				if err := m.DropColumn(&permissionResType{}, "resource_type"); err != nil {
					return err
				}
			}
			if m.HasColumn(&roleProtected{}, "protected") {
				return m.DropColumn(&roleProtected{}, "protected")
			}
			return nil
		},
	})
}
