package migration

import "testing"

// TestAgentPairingConfirmMigrationBackfill pins that a pre-existing agent
// row (paired before this wave existed, under the old "created Enabled=true
// with no confirm step" flow) is backfilled to Confirmed=true rather than
// retroactively becoming "awaiting its first SAS confirmation".
func TestAgentPairingConfirmMigrationBackfill(t *testing.T) {
	d := openMem(t, "agentpairingconfirm_mig")
	mig := New(d, testPrefix)
	if err := mig.UpTo("0016_agent_task_attempts"); err != nil {
		t.Fatalf("up to 0016: %v", err)
	}

	if err := d.Exec("INSERT INTO " + testPrefix + "agents (agent_id, name, enabled, storage_whitelist) VALUES ('legacy-agent', 'legacy', 1, '[]')").Error; err != nil {
		t.Fatalf("seed legacy agent: %v", err)
	}

	if err := mig.Up(); err != nil {
		t.Fatalf("up to 0017: %v", err)
	}

	var notConfirmed int64
	d.Table(testPrefix+"agents").Where("confirmed IS NULL OR confirmed = ?", false).Count(&notConfirmed)
	if notConfirmed != 0 {
		t.Errorf("pre-existing agent not backfilled to confirmed=true: %d rows unconfirmed", notConfirmed)
	}
}

// TestAgentPairingConfirmMigrationRollback pins that rollback drops just
// the two new columns, leaving the agents table (and its rows) intact.
func TestAgentPairingConfirmMigrationRollback(t *testing.T) {
	d := openMem(t, "agentpairingconfirm_rollback")
	mig := New(d, testPrefix)
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !d.Migrator().HasColumn(&agentPairingConfirm{}, "confirmed") {
		t.Fatalf("confirmed column not created")
	}
	if !d.Migrator().HasColumn(&agentPairingConfirm{}, "pending_sas") {
		t.Fatalf("pending_sas column not created")
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
		if last == "0017_agent_pairing_confirm" {
			break
		}
	}
	if d.Migrator().HasColumn(&agentPairingConfirm{}, "confirmed") {
		t.Errorf("confirmed column survived rollback")
	}
	if d.Migrator().HasColumn(&agentPairingConfirm{}, "pending_sas") {
		t.Errorf("pending_sas column survived rollback")
	}
	if !d.Migrator().HasTable(testPrefix + "agents") {
		t.Errorf("agents table must survive a columns-only rollback")
	}
}
