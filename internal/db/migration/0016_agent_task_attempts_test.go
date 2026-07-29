package migration

import "testing"

// 0016 adds the attempts column to the agent_tasks table, backfilling
// pre-existing rows to 0 (a task that was never requeued). Mirrors
// 0008_permission_display_name's backfill test.
func TestAgentTaskAttemptsMigrationBackfill(t *testing.T) {
	d := openMem(t, "agenttaskattempts_mig")
	mig := New(d, testPrefix)
	// Bring the DB to the pre-0016 state so the seeded row is genuinely
	// backfilled by the migration (not defaulted at insert time).
	if err := mig.UpTo("0015_agent_tasks"); err != nil {
		t.Fatalf("up to 0015: %v", err)
	}

	if err := d.Exec("INSERT INTO " + testPrefix + "agent_tasks (task_id, agent_id, status) VALUES ('legacy-task', 'agent-1', 'pending')").Error; err != nil {
		t.Fatalf("seed agent task: %v", err)
	}

	if err := mig.Up(); err != nil {
		t.Fatalf("up to 0016: %v", err)
	}

	var nonZero int64
	d.Table(testPrefix+"agent_tasks").Where("attempts IS NULL OR attempts <> ?", 0).Count(&nonZero)
	if nonZero != 0 {
		t.Errorf("pre-existing agent task not backfilled to 0 attempts: %d rows non-zero/NULL", nonZero)
	}
}

// Rollback drops just the attempts column, leaving the rest of the table
// (and its rows) intact.
func TestAgentTaskAttemptsMigrationRollback(t *testing.T) {
	d := openMem(t, "agenttaskattempts_rollback")
	mig := New(d, testPrefix)
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !d.Migrator().HasColumn(&agentTaskAttempts{}, "attempts") {
		t.Fatalf("attempts column not created")
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
		if last == "0016_agent_task_attempts" {
			break
		}
	}
	if d.Migrator().HasColumn(&agentTaskAttempts{}, "attempts") {
		t.Errorf("attempts column survived rollback")
	}
	if !d.Migrator().HasTable(testPrefix + "agent_tasks") {
		t.Errorf("agent_tasks table must survive an attempts-column-only rollback")
	}
}
