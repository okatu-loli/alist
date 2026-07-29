package migration

import "testing"

// TestAgentReverseModeMigrationBackfill pins that a pre-existing agent row
// (paired before reverse mode existed) is backfilled to mode="outbound"
// rather than left with an empty/NULL mode.
func TestAgentReverseModeMigrationBackfill(t *testing.T) {
	d := openMem(t, "agentreversemode_mig")
	mig := New(d, testPrefix)
	if err := mig.UpTo("0017_agent_pairing_confirm"); err != nil {
		t.Fatalf("up to 0017: %v", err)
	}

	if err := d.Exec("INSERT INTO " + testPrefix + "agents (agent_id, name, enabled, storage_whitelist) VALUES ('legacy-agent', 'legacy', 1, '[]')").Error; err != nil {
		t.Fatalf("seed legacy agent: %v", err)
	}

	if err := mig.Up(); err != nil {
		t.Fatalf("up to 0018: %v", err)
	}

	var notOutbound int64
	d.Table(testPrefix+"agents").Where("mode IS NULL OR mode <> ?", "outbound").Count(&notOutbound)
	if notOutbound != 0 {
		t.Errorf("pre-existing agent not backfilled to mode=outbound: %d rows off-default", notOutbound)
	}
}

// TestAgentReverseModePersist pins that mode + reverse_addr round-trip through
// the new columns.
func TestAgentReverseModePersist(t *testing.T) {
	d := openMem(t, "agentreversemode_persist")
	mig := New(d, testPrefix)
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := d.Exec("INSERT INTO "+testPrefix+"agents (agent_id, name, enabled, storage_whitelist, mode, reverse_addr) VALUES ('rev-agent', 'rev', 1, '[]', ?, ?)",
		"reverse", "wss://vps.example:5343/agentchannel").Error; err != nil {
		t.Fatalf("insert reverse agent: %v", err)
	}
	var mode, addr string
	row := d.Table(testPrefix+"agents").Where("agent_id = ?", "rev-agent").Select("mode", "reverse_addr").Row()
	if err := row.Scan(&mode, &addr); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if mode != "reverse" || addr != "wss://vps.example:5343/agentchannel" {
		t.Fatalf("mode/reverse_addr did not persist: mode=%q addr=%q", mode, addr)
	}
}

// TestAgentReverseModeMigrationRollback pins that rollback drops just the two
// new columns, leaving the agents table (and its rows) intact.
func TestAgentReverseModeMigrationRollback(t *testing.T) {
	d := openMem(t, "agentreversemode_rollback")
	mig := New(d, testPrefix)
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !d.Migrator().HasColumn(&agentReverseMode{}, "mode") {
		t.Fatalf("mode column not created")
	}
	if !d.Migrator().HasColumn(&agentReverseMode{}, "reverse_addr") {
		t.Fatalf("reverse_addr column not created")
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
		if last == "0018_agent_reverse_mode" {
			break
		}
	}
	if d.Migrator().HasColumn(&agentReverseMode{}, "mode") {
		t.Errorf("mode column survived rollback")
	}
	if d.Migrator().HasColumn(&agentReverseMode{}, "reverse_addr") {
		t.Errorf("reverse_addr column survived rollback")
	}
	if !d.Migrator().HasTable(testPrefix + "agents") {
		t.Errorf("agents table must survive a columns-only rollback")
	}
}
