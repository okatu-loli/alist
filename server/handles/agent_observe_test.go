package handles

import (
	"encoding/json"
	"github.com/AlistGo/alist-agent/proto"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/AlistGo/alist-agent/agent"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// SP-G1 admin observability tests: the log-tail endpoint pulls over the live
// reverse channel and FAILS CLOSED when the agent is not connected; the
// per-agent tasks endpoint sources progress from the existing task store,
// filtered by agent_id.

// swapReverseSupervisor installs sup as the process-wide reverse supervisor for
// the duration of a test, restoring the previous one on cleanup.
func swapReverseSupervisor(t *testing.T, sup *reverseSupervisor) {
	t.Helper()
	currentReverseSupervisorMu.Lock()
	prev := currentReverseSupervisor
	currentReverseSupervisor = sup
	currentReverseSupervisorMu.Unlock()
	t.Cleanup(func() {
		currentReverseSupervisorMu.Lock()
		currentReverseSupervisor = prev
		currentReverseSupervisorMu.Unlock()
	})
}

func parseAgentLogsResp(t *testing.T, w *httptest.ResponseRecorder) AgentLogsResp {
	t.Helper()
	var env struct {
		Code int           `json:"code"`
		Data AgentLogsResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse logs resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// TestAgentLogsOverChannelReturnsEntries drives the full server->agent log-tail
// round trip over a real loopback channel: the endpoint resolves the agent's
// live channel from the supervisor, sends agent.log.tail, and returns the
// agent's ring entries.
func TestAgentLogsOverChannelReturnsEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "logs", true, nil)

	// A live loopback channel: server end + agent end, with the server cap
	// handlers registered (harness in agent_channel_test.go).
	server, agentCh := liveReverseChannel(t, a.AgentID)

	// Agent side: a ReverseAgent with a populated ring answers log.tail.
	ring := agentpkg.NewLogRing(100)
	ring.Append(time.Now(), "info", "agent up")
	ring.Append(time.Now(), "warning", "task slow")
	ra := agentpkg.NewReverseAgent(agentCh)
	ra.LogRing = ring

	// Supervisor holding the live server-side channel for this agent.
	_, serverPriv := genEd25519(t)
	sup := newReverseSupervisor(serverPriv)
	sup.setChannel(a.AgentID, server)
	swapReverseSupervisor(t, sup)

	w := driveGetID(user, a.ID, AgentLogs)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("connected agent logs must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseAgentLogsResp(t, w)
	if len(resp.Entries) != 2 || resp.Next != 2 {
		t.Fatalf("logs = %d entries next %d, want 2, 2", len(resp.Entries), resp.Next)
	}
	if resp.Entries[0].Msg != "agent up" || resp.Entries[1].Level != "warning" {
		t.Fatalf("unexpected entries: %+v", resp.Entries)
	}
}

// TestAgentLogsClampsOversizedMsg: a malicious/compromised agent returning a
// huge per-entry msg must be size-clamped server-side (defensive — count is
// already clamped) so it can't bloat the admin's DOM.
func TestAgentLogsClampsOversizedMsg(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "logs-big", true, nil)
	server, agentCh := liveReverseChannel(t, a.AgentID)

	ring := agentpkg.NewLogRing(100)
	ring.Append(time.Now(), "info", strings.Repeat("A", 50000)) // 50k runes
	ra := agentpkg.NewReverseAgent(agentCh)
	ra.LogRing = ring

	_, serverPriv := genEd25519(t)
	sup := newReverseSupervisor(serverPriv)
	sup.setChannel(a.AgentID, server)
	swapReverseSupervisor(t, sup)

	w := driveGetID(user, a.ID, AgentLogs)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	resp := parseAgentLogsResp(t, w)
	if len(resp.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(resp.Entries))
	}
	if r := []rune(resp.Entries[0].Msg); len(r) > 2001 { // 2000 cap + the "…" marker
		t.Fatalf("oversized msg not clamped: %d runes", len(r))
	}
	if !strings.HasSuffix(resp.Entries[0].Msg, "…") {
		t.Fatalf("clamped msg should end with the truncation marker, got %q…", resp.Entries[0].Msg[:20])
	}
}

// TestAgentLogsDisconnectedFailsClosed pins the fail-closed contract: an agent
// with no live reverse channel yields a clear non-200 error, never a partial
// or empty "success".
func TestAgentLogsDisconnectedFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "disc", true, nil)

	// A supervisor exists but tracks NO channel for this agent.
	_, serverPriv := genEd25519(t)
	sup := newReverseSupervisor(serverPriv)
	swapReverseSupervisor(t, sup)

	w := driveGetID(user, a.ID, AgentLogs)
	if got := respCode(t, w); got == 200 {
		t.Fatalf("disconnected agent logs must fail closed, got 200: %s", w.Body.String())
	}
	if got := respCode(t, w); got != 503 {
		t.Fatalf("disconnected agent logs = %d, want 503 (not connected)", got)
	}
}

// TestAgentLogsNoSupervisorFailsClosed pins that with no supervisor at all
// (reverse mode unavailable this run) the endpoint still fails closed.
func TestAgentLogsNoSupervisorFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "nosup", true, nil)
	swapReverseSupervisor(t, nil)

	w := driveGetID(user, a.ID, AgentLogs)
	if got := respCode(t, w); got == 200 {
		t.Fatalf("no supervisor must fail closed, got 200: %s", w.Body.String())
	}
}

// TestAgentLogsUnknownAgent404 pins a missing agent id → 404.
func TestAgentLogsUnknownAgent404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	w := driveGetID(user, 99990001, AgentLogs)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("unknown agent logs = %d, want 404: %s", got, w.Body.String())
	}
}

func parseAgentTaskProgressList(t *testing.T, w *httptest.ResponseRecorder) []AgentTaskProgressDTO {
	t.Helper()
	var env struct {
		Data []AgentTaskProgressDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse task progress list: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// seedAgentTask persists one task row for agentID in the given status/progress.
func seedAgentTask(t *testing.T, agentID, status string, progress int, detail string) *model.AgentTask {
	t.Helper()
	task := &model.AgentTask{
		TaskID:   "obs-task-" + agentID + "-" + status + "-" + time.Now().Format("150405.000000000"),
		AgentID:  agentID,
		Type:     model.AgentTaskCopy,
		SrcPath:  "/src/" + status,
		DstDir:   "/dst",
		Status:   status,
		Progress: progress,
		Detail:   detail,
	}
	if err := db.CreateAgentTask(task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteAgentTask(task.ID) })
	return task
}

// TestAgentTasksFilteredByAgentWithProgress pins the per-agent tasks endpoint:
// it returns only THIS agent's tasks, with the display-status mapping and
// progress, and never leaks another agent's task.
func TestAgentTasksFilteredByAgentWithProgress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "tasks", true, nil)
	other := seedAgent(t, "tasks-other", true, nil)

	seedAgentTask(t, a.AgentID, model.AgentTaskPending, 0, "")
	seedAgentTask(t, a.AgentID, model.AgentTaskRunning, 42, "moving")
	seedAgentTask(t, a.AgentID, model.AgentTaskFailed, 10, "disk full")
	// A task on another agent must not appear.
	seedAgentTask(t, other.AgentID, model.AgentTaskRunning, 99, "")

	w := driveGetID(user, a.ID, AgentTasks)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("tasks must be 200, got %d: %s", got, w.Body.String())
	}
	list := parseAgentTaskProgressList(t, w)
	if len(list) != 3 {
		t.Fatalf("expected exactly this agent's 3 tasks, got %d: %+v", len(list), list)
	}

	byStatus := map[string]AgentTaskProgressDTO{}
	for _, d := range list {
		byStatus[d.Status] = d
	}
	// pending → queued display status.
	if _, ok := byStatus["queued"]; !ok {
		t.Fatalf("pending task must display as queued: %+v", list)
	}
	if run, ok := byStatus[model.AgentTaskRunning]; !ok || run.ProgressPct != 42 {
		t.Fatalf("running task must surface progress_pct 42: %+v", byStatus)
	}
	// failed → error surfaces the detail; running/queued must not.
	fail, ok := byStatus[model.AgentTaskFailed]
	if !ok || fail.Error != "disk full" {
		t.Fatalf("failed task must surface error=detail, got %+v", fail)
	}
	if byStatus[model.AgentTaskRunning].Error != "" {
		t.Fatalf("running task must not surface an error string: %+v", byStatus[model.AgentTaskRunning])
	}
}

// TestAgentTasksRapidSurfacesRapidFlag pins the SP-H3 contract: a done task whose
// stored detail is the rapid-upload marker surfaces rapid=true; a normally-
// streamed done task, and any running/failed task, surface rapid=false. The raw
// detail string is never leaked through the rapid flag.
func TestAgentTasksRapidSurfacesRapidFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "tasks-rapid", true, nil)

	seedAgentTask(t, a.AgentID, model.AgentTaskDone, 100, proto.RapidUploadDetail)   // rapid hit
	seedAgentTask(t, a.AgentID, model.AgentTaskDone, 100, "")                        // streamed done
	seedAgentTask(t, a.AgentID, model.AgentTaskRunning, 40, proto.RapidUploadDetail) // not done → false
	seedAgentTask(t, a.AgentID, model.AgentTaskFailed, 10, proto.RapidUploadDetail)  // not done → false

	w := driveGetID(user, a.ID, AgentTasks)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("tasks must be 200, got %d: %s", got, w.Body.String())
	}
	list := parseAgentTaskProgressList(t, w)
	if len(list) != 4 {
		t.Fatalf("expected 4 tasks, got %d: %+v", len(list), list)
	}

	var rapidDone, streamDone, running, failed *AgentTaskProgressDTO
	for i := range list {
		d := &list[i]
		switch {
		case d.Status == model.AgentTaskDone && d.Rapid:
			rapidDone = d
		case d.Status == model.AgentTaskDone && !d.Rapid:
			streamDone = d
		case d.Status == model.AgentTaskRunning:
			running = d
		case d.Status == model.AgentTaskFailed:
			failed = d
		}
	}
	if rapidDone == nil {
		t.Fatalf("a done task with detail=rapid-upload must surface rapid=true: %+v", list)
	}
	if streamDone == nil || streamDone.Rapid {
		t.Fatalf("a streamed done task must surface rapid=false: %+v", streamDone)
	}
	if running == nil || running.Rapid {
		t.Fatalf("a running task must never surface rapid=true: %+v", running)
	}
	if failed == nil || failed.Rapid {
		t.Fatalf("a failed task must never surface rapid=true: %+v", failed)
	}
}

// TestAgentTasksUnknownAgent404 pins a missing agent id → 404.
func TestAgentTasksUnknownAgent404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	w := driveGetID(user, 99990002, AgentTasks)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("unknown agent tasks = %d, want 404: %s", got, w.Body.String())
	}
}
