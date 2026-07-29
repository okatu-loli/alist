package migration

import "testing"

// 0005 adds resource_type to permissions and protected to roles, backfilling
// pre-existing rows: every legacy permission becomes an fs row, and the
// built-in admin/guest roles are marked protected.
func TestResourceTypeMigrationBackfill(t *testing.T) {
	d := openMem(t, "restype_mig")
	mig := New(d, testPrefix)
	// Bring the DB to the pre-0005 state so the seeded rows are genuinely
	// backfilled by the migration (not defaulted at insert time).
	if err := mig.UpTo("0004_tokens"); err != nil {
		t.Fatalf("up to 0004: %v", err)
	}

	if err := d.Exec("INSERT INTO " + testPrefix + "permissions (path, effect, recursive, origin) VALUES ('/data', 'allow', 1, 'manual')").Error; err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	for _, name := range []string{"admin", "guest", "staff"} {
		if err := d.Exec("INSERT INTO "+testPrefix+"roles (name) VALUES (?)", name).Error; err != nil {
			t.Fatalf("seed role %s: %v", name, err)
		}
	}

	if err := mig.Up(); err != nil {
		t.Fatalf("up to 0005: %v", err)
	}

	// Every pre-existing permission row is backfilled to fs.
	var nonFS int64
	d.Table(testPrefix+"permissions").Where("resource_type <> ?", "fs").Count(&nonFS)
	if nonFS != 0 {
		t.Errorf("legacy permission not backfilled to fs: %d rows still non-fs", nonFS)
	}

	// admin/guest are protected; a normal role is not.
	for _, tc := range []struct {
		name string
		want bool
	}{{"admin", true}, {"guest", true}, {"staff", false}} {
		var protected bool
		if err := d.Table(testPrefix+"roles").Select("protected").Where("name = ?", tc.name).Scan(&protected).Error; err != nil {
			t.Fatalf("read protected for %s: %v", tc.name, err)
		}
		if protected != tc.want {
			t.Errorf("role %s protected = %v, want %v", tc.name, protected, tc.want)
		}
	}
}
