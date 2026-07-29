package migration

import (
	"testing"
)

// 0012 builds the shares table. This runs the full migration set and checks the
// table exists with the token-uniqueness and node/creator lookup indexes the
// model declares. Cross-table drift (models vs migrations) is covered by
// TestSchemaDriftGuard; this test pins the migration's own output.
func TestSharesMigrationBuildsTable(t *testing.T) {
	d := openMem(t, "shares_mig")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	m := d.Migrator()

	if !m.HasTable(testPrefix + "shares") {
		t.Fatalf("shares table not created")
	}
	for _, idx := range []string{
		"idx_x_shares_token",
		"idx_x_shares_node_id",
		"idx_x_shares_creator_id",
	} {
		if !m.HasIndex(&share{}, idx) {
			t.Errorf("shares index %q missing", idx)
		}
	}
}

// Rollback drops the shares table again.
func TestSharesMigrationRollback(t *testing.T) {
	d := openMem(t, "shares_rollback")
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
		if last == "0012_shares" {
			break
		}
	}
	if d.Migrator().HasTable(testPrefix + "shares") {
		t.Errorf("shares table survived rollback")
	}
}
