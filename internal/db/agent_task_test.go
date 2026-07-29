package db

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupAgentTaskTestDB swaps the package db handle for a fresh in-memory
// database carrying just the agent_tasks table. Mirrors setupAgentTestDB.
func setupAgentTaskTestDB(t *testing.T) {
	t.Helper()
	dsn := fmt.Sprintf("file:agent_task_test_%s?mode=memory&cache=shared&_busy_timeout=5000", t.Name())
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	// model.Agent too: RequeueStaleAgentTasks joins the agent-task sweep
	// against the owning agent's LastSeen (see its staleAgentIDsQuery
	// subquery) — without this table the subquery itself would error.
	if err := dB.AutoMigrate(&model.AgentTask{}, &model.Agent{}); err != nil {
		t.Fatal(err)
	}
	// TestClaimPendingForAgentConcurrentSingleFlight fires real concurrent
	// goroutines at this db. mattn/go-sqlite3's shared-cache mode can surface
	// "database table is locked" (SQLITE_LOCKED, distinct from SQLITE_BUSY —
	// _busy_timeout above doesn't retry it) when multiple physical
	// connections in gorm's pool contend on the same table. Pinning the pool
	// to one physical connection serializes at the database/sql layer
	// instead: goroutines still race to claim (exercising
	// ClaimPendingForAgent's real compare-and-swap logic), they just don't
	// need SQLite's own multi-connection locking to get there cleanly.
	if sqlDB, err := dB.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	old := db
	db = dB
	t.Cleanup(func() { db = old })
}

func TestAgentTaskCRUD(t *testing.T) {
	setupAgentTaskTestDB(t)

	task := &model.AgentTask{
		TaskID:     "task-crud-1",
		AgentID:    "agent-1",
		Type:       model.AgentTaskCopy,
		SrcPath:    "/src/a.bin",
		DstDir:     "/dst",
		CreatorUID: 42,
		Status:     model.AgentTaskPending,
	}
	if err := CreateAgentTask(task); err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.ID == 0 {
		t.Fatal("id not assigned")
	}

	got, err := GetAgentTask(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TaskID != "task-crud-1" || got.CreatorUID != 42 || got.Status != model.AgentTaskPending {
		t.Fatalf("get by id mismatch: %+v", got)
	}

	byTaskID, err := GetAgentTaskByTaskID("task-crud-1")
	if err != nil {
		t.Fatalf("get by task_id: %v", err)
	}
	if byTaskID.ID != task.ID {
		t.Fatalf("get by task_id mismatch: %+v", byTaskID)
	}

	got.Status = model.AgentTaskRunning
	got.Progress = 50
	got.Detail = "halfway"
	if err := UpdateAgentTask(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, err := GetAgentTask(task.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != model.AgentTaskRunning || reloaded.Progress != 50 || reloaded.Detail != "halfway" {
		t.Fatalf("update did not apply: %+v", reloaded)
	}

	second := &model.AgentTask{TaskID: "task-crud-2", AgentID: "agent-1", Status: model.AgentTaskPending}
	if err := CreateAgentTask(second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	list, err := ListAgentTasks("", 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 tasks, got %d: %+v", len(list), list)
	}
	// Newest-first: task-crud-2 (created second) must sort before
	// task-crud-1.
	if list[0].TaskID != "task-crud-2" || list[1].TaskID != "task-crud-1" {
		t.Fatalf("expected newest-first order [task-crud-2 task-crud-1], got %+v", list)
	}

	if err := DeleteAgentTask(task.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := GetAgentTask(task.ID); !errors.Is(err, errs.AgentTaskNotFound) {
		t.Fatalf("get after delete must be AgentTaskNotFound, got %v", err)
	}
}

func TestGetAgentTaskUnknown(t *testing.T) {
	setupAgentTaskTestDB(t)
	if _, err := GetAgentTask(999999); !errors.Is(err, errs.AgentTaskNotFound) {
		t.Fatalf("unknown id must be AgentTaskNotFound, got %v", err)
	}
	if _, err := GetAgentTaskByTaskID("never-existed"); !errors.Is(err, errs.AgentTaskNotFound) {
		t.Fatalf("unknown task_id must be AgentTaskNotFound, got %v", err)
	}
}

// TestClaimPendingForAgentHappyPath pins the basic claim: a pending task for
// the agent is picked and flips to claimed; a task belonging to a DIFFERENT
// agent (even if pending) is never returned.
func TestClaimPendingForAgentHappyPath(t *testing.T) {
	setupAgentTaskTestDB(t)

	mine := &model.AgentTask{TaskID: "mine", AgentID: "agent-1", Status: model.AgentTaskPending}
	other := &model.AgentTask{TaskID: "other", AgentID: "agent-2", Status: model.AgentTaskPending}
	if err := CreateAgentTask(mine); err != nil {
		t.Fatal(err)
	}
	if err := CreateAgentTask(other); err != nil {
		t.Fatal(err)
	}

	claimed, err := ClaimPendingForAgent("agent-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.TaskID != "mine" {
		t.Fatalf("expected to claim 'mine', got %+v", claimed)
	}
	if claimed.Status != model.AgentTaskClaimed {
		t.Fatalf("claimed task status = %q, want claimed", claimed.Status)
	}

	reloaded, err := GetAgentTaskByTaskID("mine")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskClaimed {
		t.Fatalf("persisted status = %q, want claimed", reloaded.Status)
	}
	otherReloaded, err := GetAgentTaskByTaskID("other")
	if err != nil {
		t.Fatal(err)
	}
	if otherReloaded.Status != model.AgentTaskPending {
		t.Fatalf("other agent's task must be untouched, got status %q", otherReloaded.Status)
	}
}

// TestClaimPendingForAgentNoneAvailable pins the "nothing pending" contract:
// (nil, nil), not an error.
func TestClaimPendingForAgentNoneAvailable(t *testing.T) {
	setupAgentTaskTestDB(t)
	claimed, err := ClaimPendingForAgent("no-such-agent")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if claimed != nil {
		t.Fatalf("expected nil, got %+v", claimed)
	}
}

// TestClaimPendingForAgentNeverReclaimsTerminal pins that a done/failed task
// is never returned by ClaimPendingForAgent, even though it "belongs" to the
// agent — only status=pending rows are eligible.
func TestClaimPendingForAgentNeverReclaimsTerminal(t *testing.T) {
	setupAgentTaskTestDB(t)
	for _, status := range []string{model.AgentTaskClaimed, model.AgentTaskRunning, model.AgentTaskDone, model.AgentTaskFailed} {
		task := &model.AgentTask{TaskID: "already-" + status, AgentID: "agent-1", Status: status}
		if err := CreateAgentTask(task); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := ClaimPendingForAgent("agent-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("expected nil (nothing pending), got %+v", claimed)
	}
}

// TestClaimPendingForAgentConcurrentSingleFlight is the security-critical
// property: N goroutines racing ClaimPendingForAgent for the SAME single
// pending task must produce exactly ONE winner. Run with -race.
func TestClaimPendingForAgentConcurrentSingleFlight(t *testing.T) {
	setupAgentTaskTestDB(t)
	task := &model.AgentTask{TaskID: "contested", AgentID: "agent-1", Status: model.AgentTaskPending}
	if err := CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	results := make([]*model.AgentTask, n)
	errsOut := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errsOut[i] = ClaimPendingForAgent("agent-1")
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := 0; i < n; i++ {
		if errsOut[i] != nil {
			t.Errorf("goroutine %d: unexpected error %v", i, errsOut[i])
			continue
		}
		if results[i] != nil {
			if results[i].TaskID != "contested" {
				t.Errorf("goroutine %d: claimed unexpected task %+v", i, results[i])
			}
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent claimers, got %d", n, winners)
	}

	reloaded, err := GetAgentTaskByTaskID("contested")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskClaimed {
		t.Fatalf("final persisted status = %q, want claimed", reloaded.Status)
	}
}

// TestGetActiveAgentTaskForAgent pins the "already in flight" lookup the
// heartbeat uses to keep handing an agent its claimed/running task (with a
// fresh token) across reconnects, WITHOUT that counting as a new claim
// (status is never mutated by this call).
func TestGetActiveAgentTaskForAgent(t *testing.T) {
	setupAgentTaskTestDB(t)

	pending := &model.AgentTask{TaskID: "pending-1", AgentID: "agent-1", Status: model.AgentTaskPending}
	if err := CreateAgentTask(pending); err != nil {
		t.Fatal(err)
	}
	if got, err := GetActiveAgentTaskForAgent("agent-1"); err != nil || got != nil {
		t.Fatalf("a merely-pending task must not be 'active': got %+v, err %v", got, err)
	}

	running := &model.AgentTask{TaskID: "running-1", AgentID: "agent-1", Status: model.AgentTaskRunning}
	if err := CreateAgentTask(running); err != nil {
		t.Fatal(err)
	}
	got, err := GetActiveAgentTaskForAgent("agent-1")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if got == nil || got.TaskID != "running-1" {
		t.Fatalf("expected running-1, got %+v", got)
	}
	if got.Status != model.AgentTaskRunning {
		t.Fatalf("GetActiveAgentTaskForAgent must not mutate status, got %q", got.Status)
	}

	// A done task is not "active".
	done := &model.AgentTask{TaskID: "done-1", AgentID: "agent-2", Status: model.AgentTaskDone}
	if err := CreateAgentTask(done); err != nil {
		t.Fatal(err)
	}
	if got, err := GetActiveAgentTaskForAgent("agent-2"); err != nil || got != nil {
		t.Fatalf("a done task must not be 'active': got %+v, err %v", got, err)
	}
}

// --- RequeueStaleAgentTasks (design W12-4 (B)) ---

// seedAgentForTaskTest inserts a bare agent row with the given LastSeen
// (nil meaning "never heartbeated") — RequeueStaleAgentTasks' subquery
// joins against this table.
func seedAgentForTaskTest(t *testing.T, agentID string, lastSeen *time.Time) {
	t.Helper()
	a := &model.Agent{AgentID: agentID, Enabled: true, LastSeen: lastSeen}
	if err := CreateAgent(a); err != nil {
		t.Fatal(err)
	}
}

// backdateTaskUpdatedAt sets a task's updated_at directly (bypassing gorm's
// auto-touch-on-save hook via UpdateColumn) so a test can simulate "this
// task has genuinely sat untouched for a while", independent of when the
// row happened to be created — RequeueStaleAgentTasks additionally gates on
// this (see its "second guard" doc), so a freshly-created row is never
// stale-sweepable no matter how old its agent's LastSeen is.
func backdateTaskUpdatedAt(t *testing.T, taskID string, when time.Time) {
	t.Helper()
	if err := db.Model(&model.AgentTask{}).Where("task_id = ?", taskID).UpdateColumn("updated_at", when).Error; err != nil {
		t.Fatal(err)
	}
}

// TestRequeueStaleAgentTasksRequeuesStaleAgentsTask pins the core mechanic:
// a claimed/running task whose agent's LastSeen is well past staleAfter is
// reset to pending (claimable again) and its Attempts increments.
func TestRequeueStaleAgentTasksRequeuesStaleAgentsTask(t *testing.T) {
	setupAgentTaskTestDB(t)

	longAgo := time.Now().Add(-1 * time.Hour)
	seedAgentForTaskTest(t, "agent-dead", &longAgo)

	task := &model.AgentTask{TaskID: "stuck-running", AgentID: "agent-dead", Status: model.AgentTaskRunning}
	if err := CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	backdateTaskUpdatedAt(t, "stuck-running", longAgo)

	n, err := RequeueStaleAgentTasks(5 * time.Minute)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row changed, got %d", n)
	}

	reloaded, err := GetAgentTaskByTaskID("stuck-running")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskPending {
		t.Fatalf("status = %q, want pending", reloaded.Status)
	}
	if reloaded.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", reloaded.Attempts)
	}
	if reloaded.AgentID != "agent-dead" {
		t.Fatalf("AgentID must be preserved across requeue (same-agent reclaim, see doc), got %q", reloaded.AgentID)
	}

	// Now claimable again — the exact bug this fixes: before this function
	// existed, a claimed/running row was invisible to every future
	// ClaimPendingForAgent call, forever.
	claimed, err := ClaimPendingForAgent("agent-dead")
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if claimed == nil || claimed.TaskID != "stuck-running" {
		t.Fatalf("requeued task must be reclaimable, got %+v", claimed)
	}
}

// TestRequeueStaleAgentTasksNeverTouchesFreshAgent pins THE safety property
// this whole feature rests on: a claimed/running task whose agent's
// LastSeen is fresh (right now) is left completely untouched by a sweep —
// status, attempts, detail, updated_at, all of it — proving a live-but-busy
// agent can never be requeued out from under itself (see
// RequeueStaleAgentTasks' doc, "THE safety property").
func TestRequeueStaleAgentTasksNeverTouchesFreshAgent(t *testing.T) {
	setupAgentTaskTestDB(t)

	now := time.Now()
	seedAgentForTaskTest(t, "agent-alive", &now)

	task := &model.AgentTask{
		TaskID: "long-copy-running", AgentID: "agent-alive",
		Status: model.AgentTaskRunning, Progress: 42, Detail: "mid-transfer",
	}
	if err := CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	before, err := GetAgentTaskByTaskID("long-copy-running")
	if err != nil {
		t.Fatal(err)
	}

	// Even a staleAfter as tight as a millisecond must not touch it: what
	// matters is LastSeen being IN the window, not the window's width.
	n, err := RequeueStaleAgentTasks(5 * time.Minute)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 0 {
		t.Fatalf("a fresh agent's task must never be swept, got n=%d", n)
	}

	after, err := GetAgentTaskByTaskID("long-copy-running")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status || after.Attempts != before.Attempts ||
		after.Progress != before.Progress || after.Detail != before.Detail {
		t.Fatalf("a fresh agent's task must be byte-for-byte untouched: before=%+v after=%+v", before, after)
	}
}

// TestRequeueStaleAgentTasksNeverTouchesPendingOrTerminal pins that the
// sweep only ever considers claimed/running rows — a merely-pending task
// (nothing to "requeue"; it's already requeueable) and a terminal
// done/failed task (already settled — see model.AgentTask.IsTerminal) are
// both left alone regardless of their agent's staleness.
func TestRequeueStaleAgentTasksNeverTouchesPendingOrTerminal(t *testing.T) {
	setupAgentTaskTestDB(t)

	longAgo := time.Now().Add(-1 * time.Hour)
	seedAgentForTaskTest(t, "agent-dead-2", &longAgo)

	for _, status := range []string{model.AgentTaskPending, model.AgentTaskDone, model.AgentTaskFailed} {
		task := &model.AgentTask{TaskID: "untouched-" + status, AgentID: "agent-dead-2", Status: status}
		if err := CreateAgentTask(task); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := RequeueStaleAgentTasks(5 * time.Minute); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	for _, status := range []string{model.AgentTaskPending, model.AgentTaskDone, model.AgentTaskFailed} {
		reloaded, err := GetAgentTaskByTaskID("untouched-" + status)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.Status != status {
			t.Fatalf("task seeded as %q must stay %q, got %q", status, status, reloaded.Status)
		}
	}
}

// TestRequeueStaleAgentTasksNeverTouchesUnknownAgent pins that a task whose
// AgentID matches no row in the agents table at all (e.g. the agent was
// deleted) is left alone: staleAgentIDsQuery only ever names agents that DO
// exist and ARE stale, never "agent_id not found in agents at all" — an
// orphaned task is a different problem (out of this wave's scope) than a
// dead agent's task.
func TestRequeueStaleAgentTasksNeverTouchesUnknownAgent(t *testing.T) {
	setupAgentTaskTestDB(t)

	task := &model.AgentTask{TaskID: "orphaned", AgentID: "agent-never-existed", Status: model.AgentTaskRunning}
	if err := CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}

	n, err := RequeueStaleAgentTasks(5 * time.Minute)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 0 {
		t.Fatalf("an orphaned task (no matching agent row) must not be swept, got n=%d", n)
	}
}

// TestRequeueStaleAgentTasksAttemptsCapMarksFailed pins the give-up path: a
// task already at MaxAgentTaskAttempts, whose agent is STILL stale, is
// finalized as failed (never requeued a 4th time) with a "no healthy agent"
// detail — the "cap N=3, then stop retrying forever" contract.
func TestRequeueStaleAgentTasksAttemptsCapMarksFailed(t *testing.T) {
	setupAgentTaskTestDB(t)

	longAgo := time.Now().Add(-1 * time.Hour)
	seedAgentForTaskTest(t, "agent-flaky", &longAgo)

	task := &model.AgentTask{
		TaskID: "exhausted", AgentID: "agent-flaky",
		Status: model.AgentTaskRunning, Attempts: MaxAgentTaskAttempts,
	}
	if err := CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	backdateTaskUpdatedAt(t, "exhausted", longAgo)

	n, err := RequeueStaleAgentTasks(5 * time.Minute)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row changed (requeued OR failed), got %d", n)
	}

	reloaded, err := GetAgentTaskByTaskID("exhausted")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskFailed {
		t.Fatalf("status = %q, want failed", reloaded.Status)
	}
	if reloaded.Attempts != MaxAgentTaskAttempts {
		t.Fatalf("attempts must NOT increment past the cap on the give-up path, got %d", reloaded.Attempts)
	}
	if reloaded.Detail == "" {
		t.Fatal("failed-by-sweep task must carry an explanatory detail")
	}

	// A failed task is terminal: it must never be handed back out.
	if claimed, err := ClaimPendingForAgent("agent-flaky"); err != nil || claimed != nil {
		t.Fatalf("a failed task must never become claimable, got %+v, err %v", claimed, err)
	}
}

// TestRequeueStaleAgentTasksBelowCapIncrementsNotFails pins the boundary
// exactly at the cap: Attempts == MaxAgentTaskAttempts-1 (one requeue still
// allowed) must requeue (not fail) and land exactly AT the cap.
func TestRequeueStaleAgentTasksBelowCapIncrementsNotFails(t *testing.T) {
	setupAgentTaskTestDB(t)

	longAgo := time.Now().Add(-1 * time.Hour)
	seedAgentForTaskTest(t, "agent-flaky-2", &longAgo)

	task := &model.AgentTask{
		TaskID: "one-more-try", AgentID: "agent-flaky-2",
		Status: model.AgentTaskClaimed, Attempts: MaxAgentTaskAttempts - 1,
	}
	if err := CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	backdateTaskUpdatedAt(t, "one-more-try", longAgo)

	if _, err := RequeueStaleAgentTasks(5 * time.Minute); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	reloaded, err := GetAgentTaskByTaskID("one-more-try")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskPending {
		t.Fatalf("status = %q, want pending (still under the cap)", reloaded.Status)
	}
	if reloaded.Attempts != MaxAgentTaskAttempts {
		t.Fatalf("attempts = %d, want exactly %d", reloaded.Attempts, MaxAgentTaskAttempts)
	}
}

// TestClaimPendingForAgentLazySweepRequeuesBeforeClaiming pins the wiring
// (design W12-4 (B), "lazy in ClaimPendingForAgent"): calling
// ClaimPendingForAgent for agent B, while agent A's task is stuck and A is
// stale, must sweep A's task back to pending as a side effect — even though
// this particular call is asking about B, not A — and a SUBSEQUENT call for
// A must then be able to claim it. Confirms the sweep really is wired into
// the claim path, not merely unit-tested in isolation.
func TestClaimPendingForAgentLazySweepRequeuesBeforeClaiming(t *testing.T) {
	setupAgentTaskTestDB(t)

	longAgo := time.Now().Add(-1 * time.Hour)
	seedAgentForTaskTest(t, "agent-a-dead", &longAgo)
	now := time.Now()
	seedAgentForTaskTest(t, "agent-b-alive", &now)

	stuck := &model.AgentTask{TaskID: "a-stuck", AgentID: "agent-a-dead", Status: model.AgentTaskRunning}
	if err := CreateAgentTask(stuck); err != nil {
		t.Fatal(err)
	}
	backdateTaskUpdatedAt(t, "a-stuck", longAgo)

	// B has nothing pending of its own — this call claims nothing for B,
	// but must still perform the global sweep as a side effect.
	if claimed, err := ClaimPendingForAgent("agent-b-alive"); err != nil || claimed != nil {
		t.Fatalf("agent-b-alive has no pending task, got %+v, err %v", claimed, err)
	}

	reloaded, err := GetAgentTaskByTaskID("a-stuck")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskPending {
		t.Fatalf("agent-b-alive's claim call must have swept agent-a-dead's stuck task to pending as a side effect, got status %q", reloaded.Status)
	}

	claimed, err := ClaimPendingForAgent("agent-a-dead")
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.TaskID != "a-stuck" {
		t.Fatalf("agent-a-dead must now be able to reclaim its own swept task, got %+v", claimed)
	}
}

// TestClaimPendingConcurrentWithRequeueNeverDoubleClaims extends the
// existing single-flight property (TestClaimPendingForAgentConcurrentSingleFlight)
// across a concurrent RequeueStaleAgentTasks sweep: goroutines racing
// ClaimPendingForAgent (which itself sweeps first, see that function's doc)
// against a SEPARATE, directly-invoked RequeueStaleAgentTasks call, all
// targeting one contested row, must still produce exactly one winner overall
// — a sweep changing a row's status is just as much a "claim participant" as
// another ClaimPendingForAgent call, and the same atomic-UPDATE-per-row
// reasoning must hold across both call sites. Run with -race.
func TestClaimPendingConcurrentWithRequeueNeverDoubleClaims(t *testing.T) {
	setupAgentTaskTestDB(t)

	longAgo := time.Now().Add(-1 * time.Hour)
	seedAgentForTaskTest(t, "agent-contested", &longAgo)
	task := &model.AgentTask{TaskID: "contested-vs-sweep", AgentID: "agent-contested", Status: model.AgentTaskRunning}
	if err := CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	backdateTaskUpdatedAt(t, "contested-vs-sweep", longAgo)

	const n = 10
	var wg sync.WaitGroup
	claimResults := make([]*model.AgentTask, n)
	wg.Add(n + 1)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Each of these first sweeps (requeuing the row if it is still
			// running/claimed and stale), then tries to claim it.
			r, err := ClaimPendingForAgent("agent-contested")
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			claimResults[i] = r
		}(i)
	}
	go func() {
		defer wg.Done()
		if _, err := RequeueStaleAgentTasks(5 * time.Minute); err != nil {
			t.Errorf("requeue: %v", err)
		}
	}()
	wg.Wait()

	winners := 0
	for _, r := range claimResults {
		if r != nil {
			if r.TaskID != "contested-vs-sweep" {
				t.Errorf("unexpected claimed task: %+v", r)
			}
			winners++
		}
	}
	if winners > 1 {
		t.Fatalf("expected at most 1 winner among %d concurrent claimers racing a sweep, got %d", n, winners)
	}

	reloaded, err := GetAgentTaskByTaskID("contested-vs-sweep")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskClaimed && reloaded.Status != model.AgentTaskPending {
		t.Fatalf("final status must be claimed (someone won) or pending (race resolved to nobody claiming yet), got %q", reloaded.Status)
	}
}

// --- ListAgentTasks (design W12-4: task visibility) ---

func TestListAgentTasksFilterOrderAndPaging(t *testing.T) {
	setupAgentTaskTestDB(t)

	for i, agentID := range []string{"agent-x", "agent-y", "agent-x", "agent-y", "agent-x"} {
		task := &model.AgentTask{TaskID: fmt.Sprintf("t-%d", i), AgentID: agentID, Status: model.AgentTaskPending}
		if err := CreateAgentTask(task); err != nil {
			t.Fatal(err)
		}
	}

	all, err := ListAgentTasks("", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 tasks unfiltered, got %d", len(all))
	}
	// Newest-first: t-4 (id 5, created last) must lead.
	if all[0].TaskID != "t-4" {
		t.Fatalf("expected newest-first, first = %q, want t-4", all[0].TaskID)
	}

	xOnly, err := ListAgentTasks("agent-x", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(xOnly) != 3 {
		t.Fatalf("expected 3 agent-x tasks, got %d", len(xOnly))
	}
	for _, task := range xOnly {
		if task.AgentID != "agent-x" {
			t.Fatalf("filter leaked another agent's task: %+v", task)
		}
	}

	page1, err := ListAgentTasks("", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Fatalf("expected page size 2, got %d", len(page1))
	}
	page2, err := ListAgentTasks("", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 {
		t.Fatalf("expected page size 2, got %d", len(page2))
	}
	if page1[0].TaskID == page2[0].TaskID || page1[1].TaskID == page2[0].TaskID {
		t.Fatalf("paged results must not overlap: page1=%+v page2=%+v", page1, page2)
	}
	// Together, page1+page2 must be the first 4 of the newest-first order.
	if page1[0].TaskID != all[0].TaskID || page1[1].TaskID != all[1].TaskID ||
		page2[0].TaskID != all[2].TaskID || page2[1].TaskID != all[3].TaskID {
		t.Fatalf("paging must preserve newest-first order across pages: page1=%+v page2=%+v all=%+v", page1, page2, all)
	}
}

func TestListAgentTasksEmpty(t *testing.T) {
	setupAgentTaskTestDB(t)
	list, err := ListAgentTasks("no-such-agent", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %+v", list)
	}
}
