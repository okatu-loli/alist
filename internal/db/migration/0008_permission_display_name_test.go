package migration

import "testing"

// 0008 adds the display_name column to the permissions table, backfilling
// pre-existing rows to the empty string (display falls back to Name).
func TestPermissionDisplayNameMigrationBackfill(t *testing.T) {
	d := openMem(t, "permdisplay_mig")
	mig := New(d, testPrefix)
	// Bring the DB to the pre-0008 state so the seeded row is genuinely
	// backfilled by the migration (not defaulted at insert time).
	if err := mig.UpTo("0007_audit"); err != nil {
		t.Fatalf("up to 0007: %v", err)
	}

	if err := d.Exec("INSERT INTO " + testPrefix + "permissions (name, path, effect, recursive, origin) VALUES ('legacy', '/data', 'allow', 1, 'manual')").Error; err != nil {
		t.Fatalf("seed permission: %v", err)
	}

	if err := mig.Up(); err != nil {
		t.Fatalf("up to 0008: %v", err)
	}

	var nonEmpty int64
	d.Table(testPrefix+"permissions").Where("display_name IS NULL OR display_name <> ?", "").Count(&nonEmpty)
	if nonEmpty != 0 {
		t.Errorf("pre-existing permission not backfilled to empty display_name: %d rows non-empty/NULL", nonEmpty)
	}
}
