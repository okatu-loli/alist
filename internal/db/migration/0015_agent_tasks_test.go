package migration

import (
	"testing"
)

// 0015 builds the x_agent_tasks table (design W12-3a). This runs the full
// migration set and checks the table exists with the TaskID unique index and
// AgentID lookup index the model declares. Cross-table drift (models vs
// migrations) is covered by TestSchemaDriftGuard; this test pins the
// migration's own output.
func TestAgentTasksMigrationBuildsTable(t *testing.T) {
	d := openMem(t, "agent_tasks_mig")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	m := d.Migrator()

	if !m.HasTable(testPrefix + "agent_tasks") {
		t.Fatalf("agent_tasks table not created")
	}
	if !m.HasIndex(&agentTask{}, "idx_x_agent_tasks_task_id") {
		t.Errorf("agent_tasks index idx_x_agent_tasks_task_id missing")
	}
	if !m.HasIndex(&agentTask{}, "idx_x_agent_tasks_agent_id") {
		t.Errorf("agent_tasks index idx_x_agent_tasks_agent_id missing")
	}
}

// Rollback drops the x_agent_tasks table again.
func TestAgentTasksMigrationRollback(t *testing.T) {
	d := openMem(t, "agent_tasks_rollback")
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
		if last == "0015_agent_tasks" {
			break
		}
	}
	if d.Migrator().HasTable(testPrefix + "agent_tasks") {
		t.Errorf("agent_tasks table survived rollback")
	}
}
