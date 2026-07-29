package db

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupAgentTestDB swaps the package db handle for a fresh in-memory database
// carrying just the agents table. Mirrors setupLabelTestDB.
func setupAgentTestDB(t *testing.T) {
	t.Helper()
	dsn := fmt.Sprintf("file:agent_test_%s?mode=memory&cache=shared", t.Name())
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.Agent{}); err != nil {
		t.Fatal(err)
	}
	old := db
	db = dB
	t.Cleanup(func() { db = old })
}

func TestAgentCRUD(t *testing.T) {
	setupAgentTestDB(t)

	a := &model.Agent{AgentID: "agent-crud-1", Name: "office-nas", Enabled: true}
	a.SetWhitelist([]string{"/local", "/nas"})
	if err := CreateAgent(a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ID == 0 {
		t.Fatal("id not assigned")
	}

	got, err := GetAgent(a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "office-nas" || got.AgentID != "agent-crud-1" || !got.Enabled {
		t.Fatalf("get by id mismatch: %+v", got)
	}
	if wl := got.Whitelist(); len(wl) != 2 || wl[0] != "/local" || wl[1] != "/nas" {
		t.Fatalf("whitelist round-trip mismatch: %v", wl)
	}

	got.Name = "renamed-nas"
	got.Enabled = false
	now := time.Now().Truncate(time.Second)
	got.LastSeen = &now
	if err := UpdateAgent(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, err := GetAgent(a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Name != "renamed-nas" || reloaded.Enabled {
		t.Fatalf("update did not apply: %+v", reloaded)
	}
	if reloaded.LastSeen == nil || !reloaded.LastSeen.Equal(now) {
		t.Fatalf("LastSeen did not round-trip: %+v", reloaded.LastSeen)
	}

	// A second row, so ListAgents has two to distinguish.
	b := &model.Agent{AgentID: "agent-crud-2", Name: "second"}
	if err := CreateAgent(b); err != nil {
		t.Fatalf("create second: %v", err)
	}
	list, err := ListAgents()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 agents, got %d: %+v", len(list), list)
	}

	if err := DeleteAgent(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := GetAgent(a.ID); !errors.Is(err, errs.AgentNotFound) {
		t.Fatalf("get after delete must be AgentNotFound, got %v", err)
	}
	list, err = ListAgents()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("expected only the second agent to survive, got %+v", list)
	}
}

func TestGetAgentUnknownID(t *testing.T) {
	setupAgentTestDB(t)
	if _, err := GetAgent(999999); !errors.Is(err, errs.AgentNotFound) {
		t.Fatalf("unknown id must be AgentNotFound, got %v", err)
	}
}

// TestGetAgentByAgentID pins the lookup key the pairing-complete and
// heartbeat handlers actually use (they never see the numeric primary key).
func TestGetAgentByAgentID(t *testing.T) {
	setupAgentTestDB(t)
	a := &model.Agent{AgentID: "fingerprint-abc", Name: "by-agent-id", Enabled: true}
	if err := CreateAgent(a); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := GetAgentByAgentID("fingerprint-abc")
	if err != nil {
		t.Fatalf("get by agent_id: %v", err)
	}
	if got.ID != a.ID || got.Name != "by-agent-id" {
		t.Fatalf("mismatch: %+v", got)
	}

	if _, err := GetAgentByAgentID("never-paired"); !errors.Is(err, errs.AgentNotFound) {
		t.Fatalf("unknown agent_id must be AgentNotFound, got %v", err)
	}
}

// TestAgentWhitelistJSONRoundTrip pins model.Agent's Whitelist()/SetWhitelist()
// helpers directly (no DB involved): a nil/empty slice round-trips to "[]" and
// back to an empty, non-nil slice, never a bare "" or a nil slice a caller
// would have to nil-check.
func TestAgentWhitelistJSONRoundTrip(t *testing.T) {
	var a model.Agent

	// Zero-value column ("" from a fresh row / migration default) decodes to
	// an empty, non-nil slice.
	if wl := a.Whitelist(); wl == nil || len(wl) != 0 {
		t.Fatalf("zero-value StorageWhitelist must decode to empty non-nil slice, got %#v", wl)
	}

	a.SetWhitelist(nil)
	if a.StorageWhitelist != "[]" {
		t.Fatalf("SetWhitelist(nil) must store \"[]\", got %q", a.StorageWhitelist)
	}
	if wl := a.Whitelist(); len(wl) != 0 {
		t.Fatalf("SetWhitelist(nil) round-trip must decode empty, got %v", wl)
	}

	a.SetWhitelist([]string{})
	if a.StorageWhitelist != "[]" {
		t.Fatalf("SetWhitelist([]) must store \"[]\", got %q", a.StorageWhitelist)
	}

	paths := []string{"/local", "storage:3"}
	a.SetWhitelist(paths)
	if a.StorageWhitelist != `["/local","storage:3"]` {
		t.Fatalf("unexpected encoded whitelist: %q", a.StorageWhitelist)
	}
	got := a.Whitelist()
	if len(got) != 2 || got[0] != "/local" || got[1] != "storage:3" {
		t.Fatalf("whitelist round-trip mismatch: %v", got)
	}

	// Malformed column never panics or errors out to the caller — it decodes
	// to empty, the same safe default as an unset column.
	a.StorageWhitelist = "{not json"
	if wl := a.Whitelist(); wl == nil || len(wl) != 0 {
		t.Fatalf("malformed StorageWhitelist must decode to empty non-nil slice, got %#v", wl)
	}
}
