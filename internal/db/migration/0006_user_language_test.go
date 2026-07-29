package migration

import "testing"

// 0006 adds the language column to the users table, backfilling pre-existing
// rows to the empty string ("follow Accept-Language").
func TestUserLanguageMigrationBackfill(t *testing.T) {
	d := openMem(t, "userlang_mig")
	mig := New(d, testPrefix)
	// Bring the DB to the pre-0006 state so the seeded row is genuinely
	// backfilled by the migration (not defaulted at insert time).
	if err := mig.UpTo("0005_resource_type"); err != nil {
		t.Fatalf("up to 0005: %v", err)
	}

	if err := d.Exec("INSERT INTO " + testPrefix + "users (username) VALUES ('alice')").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if err := mig.Up(); err != nil {
		t.Fatalf("up to 0006: %v", err)
	}

	var nonEmpty int64
	d.Table(testPrefix+"users").Where("language IS NULL OR language <> ?", "").Count(&nonEmpty)
	if nonEmpty != 0 {
		t.Errorf("pre-existing user not backfilled to empty language: %d rows non-empty/NULL", nonEmpty)
	}
}
