package migration

import (
	"testing"
)

// 0013 builds the app_passwords table. This runs the full migration set and
// checks the table exists with the user-id lookup index the model declares.
// Cross-table drift (models vs migrations) is covered by TestSchemaDriftGuard;
// this test pins the migration's own output.
func TestAppPasswordsMigrationBuildsTable(t *testing.T) {
	d := openMem(t, "app_passwords_mig")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	m := d.Migrator()

	if !m.HasTable(testPrefix + "app_passwords") {
		t.Fatalf("app_passwords table not created")
	}
	if !m.HasIndex(&appPassword{}, "idx_x_app_passwords_user_id") {
		t.Errorf("app_passwords index idx_x_app_passwords_user_id missing")
	}
}

// Rollback drops the app_passwords table again.
func TestAppPasswordsMigrationRollback(t *testing.T) {
	d := openMem(t, "app_passwords_rollback")
	mig := New(d, testPrefix)
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	for i := 0; i < len(registry); i++ {
		applied, _, err := mig.Status()
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if len(applied) == 0 || applied[len(applied)-1] == "0000_baseline" {
			break
		}
		last := applied[len(applied)-1]
		if err := mig.Down(); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
		if last == "0013_app_passwords" {
			break
		}
	}
	if d.Migrator().HasTable(testPrefix + "app_passwords") {
		t.Errorf("app_passwords table survived rollback")
	}
}
