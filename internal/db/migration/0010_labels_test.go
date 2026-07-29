package migration

import (
	"testing"
)

// 0010 builds the two labels tables. This runs the full migration set and checks
// that both tables exist with the identity, name-uniqueness and lookup indexes
// the models declare. Cross-table drift (models vs migrations) is covered by
// TestSchemaDriftGuard once the models are in its list; this test pins the
// migration's own output.
func TestLabelsMigrationBuildsTables(t *testing.T) {
	d := openMem(t, "labels_mig")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	m := d.Migrator()

	if !m.HasTable(testPrefix + "labels") {
		t.Fatalf("labels table not created")
	}
	if !m.HasTable(testPrefix + "label_bindings") {
		t.Fatalf("label_bindings table not created")
	}

	// labels: name is uniquely indexed.
	if !m.HasIndex(&label{}, "idx_x_labels_name") {
		t.Errorf("labels.name unique index missing")
	}

	// label_bindings: composite identity uniqueness + per-column lookup indexes.
	for _, idx := range []string{
		"idx_label_binding_identity",
		"idx_x_label_bindings_node_id",
		"idx_x_label_bindings_label_id",
	} {
		if !m.HasIndex(&labelBinding{}, idx) {
			t.Errorf("label_bindings index %q missing", idx)
		}
	}
}

// Rollback drops both tables again.
func TestLabelsMigrationRollback(t *testing.T) {
	d := openMem(t, "labels_rollback")
	mig := New(d, testPrefix)
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// Down() rolls back one migration at a time. Newer migrations sit on top of
	// 0010, so roll back until 0010 itself is undone (each later migration's
	// rollback is a no-op for the labels tables).
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
		if last == "0010_labels" {
			break
		}
	}
	m := d.Migrator()
	if m.HasTable(testPrefix + "labels") {
		t.Errorf("labels table survived rollback")
	}
	if m.HasTable(testPrefix + "label_bindings") {
		t.Errorf("label_bindings table survived rollback")
	}
}
