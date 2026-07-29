package migration

import (
	"testing"
)

// 0014 builds the x_agents table (design W12-1). This runs the full
// migration set and checks the table exists with the AgentID unique index
// the model declares. Cross-table drift (models vs migrations) is covered by
// TestSchemaDriftGuard; this test pins the migration's own output.
func TestAgentsMigrationBuildsTable(t *testing.T) {
	d := openMem(t, "agents_mig")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	m := d.Migrator()

	if !m.HasTable(testPrefix + "agents") {
		t.Fatalf("agents table not created")
	}
	if !m.HasIndex(&agent{}, "idx_x_agents_agent_id") {
		t.Errorf("agents index idx_x_agents_agent_id missing")
	}
}

// Rollback drops the x_agents table again.
func TestAgentsMigrationRollback(t *testing.T) {
	d := openMem(t, "agents_rollback")
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
		if last == "0014_agents" {
			break
		}
	}
	if d.Migrator().HasTable(testPrefix + "agents") {
		t.Errorf("agents table survived rollback")
	}
}
