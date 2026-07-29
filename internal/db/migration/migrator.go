package migration

import (
	"errors"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Migrator wraps gormigrate with alist's table-prefix convention and the
// baseline-protection rules from rollout Step 1.
type Migrator struct {
	db        *gorm.DB
	g         *gormigrate.Gormigrate
	tableName string
}

// New builds a Migrator. tablePrefix is the gorm NamingStrategy prefix
// (normally conf.Conf.Database.TablePrefix) — injected so this package stays
// conf-free and testable.
func New(db *gorm.DB, tablePrefix string) *Migrator {
	tableName := tablePrefix + "schema_migrations"
	opts := &gormigrate.Options{
		TableName:                 tableName,
		IDColumnName:              "id",
		IDColumnSize:              255,
		UseTransaction:            false, // MySQL DDL is auto-commit anyway
		ValidateUnknownMigrations: false, // an old binary may run against a newer DB during rollback
	}
	return &Migrator{
		db:        db,
		g:         gormigrate.New(db, opts, registry),
		tableName: tableName,
	}
}

// Up applies all pending migrations.
func (m *Migrator) Up() error {
	return m.g.Migrate()
}

// UpTo applies migrations up to and including id, stopping short of later
// ones. Exposed for tests that need to seed a pre-migration schema shape.
func (m *Migrator) UpTo(id string) error {
	return m.g.MigrateTo(id)
}

// TableName returns the fully-prefixed migration bookkeeping table name.
func (m *Migrator) TableName() string {
	return m.tableName
}

// Down rolls back the most recent migration. It refuses to cross the
// baseline: restoring a pre-baseline state means restoring a backup.
func (m *Migrator) Down() error {
	applied, _, err := m.Status()
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		return errors.New("nothing to roll back")
	}
	if applied[len(applied)-1] == "0000_baseline" {
		return ErrBaselineIrreversible
	}
	return m.g.RollbackLast()
}

// Status reports applied migration IDs (in table order) and pending IDs
// (registry order minus applied).
func (m *Migrator) Status() (applied []string, pending []string, err error) {
	if m.db.Migrator().HasTable(m.tableName) {
		if err = m.db.Table(m.tableName).Order("id").Pluck("id", &applied).Error; err != nil {
			return nil, nil, err
		}
	}
	appliedSet := make(map[string]struct{}, len(applied))
	for _, id := range applied {
		appliedSet[id] = struct{}{}
	}
	for _, mig := range registry {
		if _, ok := appliedSet[mig.ID]; !ok {
			pending = append(pending, mig.ID)
		}
	}
	return applied, pending, nil
}
