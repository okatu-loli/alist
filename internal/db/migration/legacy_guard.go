package migration

import (
	"fmt"

	"gorm.io/gorm"
)

// GuardLegacyAutoMigrate enforces the "二选一" rule from rollout Step 1: the
// legacy AutoMigrate path may only run while the schema is still expressible
// by AutoMigrate — i.e. before any post-baseline migration has been applied.
// After that point the migration history is the only source of truth and
// AutoMigrate would drift the schema outside of it.
func GuardLegacyAutoMigrate(d *gorm.DB, tablePrefix string) error {
	tableName := tablePrefix + "schema_migrations"
	if !d.Migrator().HasTable(tableName) {
		return nil
	}
	var ids []string
	if err := d.Table(tableName).Pluck("id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		if id != "0000_baseline" {
			return fmt.Errorf("refusing --migrate-mode=legacy: migration %q has been applied; the schema is owned by the migration history now (use auto/off)", id)
		}
	}
	return nil
}
