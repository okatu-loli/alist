package db

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTasksTestDB(t *testing.T) {
	t.Helper()
	dsn := "file:tasks_test_" + t.Name() + "?mode=memory&cache=shared"
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.TaskItem{}); err != nil {
		t.Fatal(err)
	}
	old := db
	db = dB
	t.Cleanup(func() { db = old })
}

// TestGetTaskDataFunc_NoRowYetReturnsEmptyArray guards against the bug where
// GetTaskDataFunc returned a nil function whenever the TaskItem row didn't
// exist yet (fresh install, or a task type InitialTasks() never seeded).
// tache.NewManager only wires up persistence when *both* the read and write
// functions are non-nil, so a nil read function silently disabled
// persistence forever, even with TaskPersistant: true.
func TestGetTaskDataFunc_NoRowYetReturnsEmptyArray(t *testing.T) {
	setupTasksTestDB(t)

	readFunc := GetTaskDataFunc("copy", true)
	if readFunc == nil {
		t.Fatal("GetTaskDataFunc(enabled=true) must never return nil, even with no row yet")
	}
	data, err := readFunc()
	if err != nil {
		t.Fatalf("unexpected error reading unseeded task data: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("expected empty task list %q, got %q", "[]", data)
	}
}

// TestGetTaskDataFunc_Disabled checks the disabled path still returns nil,
// which tache.NewManager relies on to skip persistence entirely.
func TestGetTaskDataFunc_Disabled(t *testing.T) {
	setupTasksTestDB(t)
	if f := GetTaskDataFunc("copy", false); f != nil {
		t.Error("GetTaskDataFunc(enabled=false) must return nil")
	}
}

// TestUpdateTaskData_CreatesRowWhenMissing guards the other half of the same
// bug: UpdateTaskData used to do a bare `UPDATE ... WHERE key = ?`, which
// silently touches zero rows (and persists nothing) the first time a task
// type is ever written, since InitialTasks() doesn't seed every task type
// (e.g. "move" and "upload" aren't in it).
func TestUpdateTaskData_CreatesRowWhenMissing(t *testing.T) {
	setupTasksTestDB(t)

	if err := UpdateTaskData(&model.TaskItem{Key: "move", PersistData: `[{"id":"1"}]`}); err != nil {
		t.Fatalf("UpdateTaskData on missing row: %v", err)
	}
	got, err := GetTaskDataByType("move")
	if err != nil {
		t.Fatalf("GetTaskDataByType: %v", err)
	}
	if got.PersistData != `[{"id":"1"}]` {
		t.Errorf("PersistData = %q, want the freshly-created row's data", got.PersistData)
	}

	// second write should update the existing row, not create a duplicate.
	if err := UpdateTaskData(&model.TaskItem{Key: "move", PersistData: `[{"id":"2"}]`}); err != nil {
		t.Fatalf("UpdateTaskData on existing row: %v", err)
	}
	got, err = GetTaskDataByType("move")
	if err != nil {
		t.Fatalf("GetTaskDataByType after update: %v", err)
	}
	if got.PersistData != `[{"id":"2"}]` {
		t.Errorf("PersistData = %q, want updated data", got.PersistData)
	}
}

// TestUpdateTaskDataFunc_RoundTripsThroughGetTaskDataFunc exercises the
// exact write-then-read cycle tache performs: persist() calls the write
// function, and a fresh manager's recover() calls the read function.
func TestUpdateTaskDataFunc_RoundTripsThroughGetTaskDataFunc(t *testing.T) {
	setupTasksTestDB(t)

	writeFunc := UpdateTaskDataFunc("upload", true)
	if writeFunc == nil {
		t.Fatal("UpdateTaskDataFunc(enabled=true) must not be nil")
	}
	if err := writeFunc([]byte(`[{"id":"abc"}]`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	readFunc := GetTaskDataFunc("upload", true)
	data, err := readFunc()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != `[{"id":"abc"}]` {
		t.Errorf("read back %q, want the written data", data)
	}
}
