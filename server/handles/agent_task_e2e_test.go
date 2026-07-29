package handles

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	agentpkg "github.com/AlistGo/alist-agent/agent"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// TestAgentDelegatedCopyEndToEnd drives the WHOLE remote-agent copy
// pipeline over a real loopback HTTP server, using nothing but the same
// public entry points a real operator/agent would: an admin starts a
// pairing session and a caller creates a task (both driven directly through
// their handlers, as CreateAgentTask/agent-admin flows elsewhere in this
// package do), the agent completes pairing and heartbeats for real HTTP
// calls, the heartbeat dispatches the pending task with a capability token,
// and internal/agent.RunCopyTask (the exact code cmd/agent.go's `agent run`
// calls) executes it — src-link, download, upload, progress, status — all
// against the REAL handlers (design W12-3a).
//
// The fixture is fakeRemoteDriver (agent_task_remote_test.go), not plain
// Local: CreateAgentTask now categorically rejects Local as either src or
// dst (errs.AgentLocalDriverUnsupported — see
// TestCreateAgentTaskSrcIsLocalRejected / …DstIsLocalRejected), so a
// Local-to-Local fixture can no longer reach step 3 below at all.
// fakeRemoteDriver is disk-backed exactly like Local (so the on-disk
// assertion at the end is unchanged) but is registered under a different
// driver name and exposes a genuine, agent-fetchable download URL (over an
// in-process httptest.Server) instead of Local's forced server-proxy
// fallback — i.e. it stands in for what a real, supported (non-Local)
// cloud-drive source/destination looks like. The destination file on disk
// is asserted to hold the exact source bytes: proof the whole chain — pair
// -> task create -> heartbeat dispatch -> agent-side execute -> server-side
// write — actually works together, not just that each half independently
// parses its own fixtures.
func TestAgentDelegatedCopyEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRemoteAgentTaskFixture(t)

	r := gin.New()
	r.POST("/api/v4/agent/pair/complete", PairAgentComplete)
	r.POST("/api/v4/agent/heartbeat", AgentHeartbeat)
	r.POST("/api/v4/agent/task/:id/src-link", AgentTaskSrcLink)
	r.PUT("/api/v4/agent/task/:id/content", AgentTaskPutContent)
	r.POST("/api/v4/agent/task/:id/progress", AgentTaskProgress)
	r.POST("/api/v4/agent/task/:id/status", AgentTaskStatus)
	// fakeRemoteDriver's src-link is a genuine direct URL (its own
	// httptest.Server, started in agent_task_remote_test.go's Init) — this
	// router mount is (unlike the Local fixture this test used to run) not
	// actually hit by the download itself, only by anything else that might
	// still resolve a signed /d link; kept for parity with the production
	// router (server/router.go) and as a safety net, minus the rate
	// limiters, which are operational concerns orthogonal to what this test
	// pins.
	r.GET("/d/*path", middlewares.Down(), Down)
	srv := httptest.NewServer(r)
	defer srv.Close()

	// 1. An admin starts a pairing session and the agent completes it for
	// real, over HTTP — the exact sequence `alist agent join` drives (see
	// server/handles/agent_e2e_test.go's join/pair/heartbeat test, which
	// this test extends one step further into task execution).
	code, _ := agentsrv.Start()
	dataDir := t.TempDir()
	identity, err := agentpkg.LoadOrCreateIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	pairResp, err := agentpkg.PairComplete(t.Context(), srv.URL, agentpkg.PairJoinParams{
		Code: code, Identity: identity, Name: "e2e-copy-agent", Platform: "linux/amd64", Version: "vtest",
	})
	if err != nil {
		t.Fatalf("PairComplete: %v", err)
	}

	// 2. A freshly paired agent is disabled/unconfirmed until an admin
	// SAS-confirms it (design W12-2b — see server/handles/agent_e2e_test.go
	// for the dedicated pairing+SAS+confirm end-to-end test) AND has an
	// EMPTY whitelist by design (PairAgentComplete's own doc) until an admin
	// grants one. Both are exercised here as direct DB updates (the admin
	// confirm/PATCH endpoints themselves are out of THIS test's scope, which
	// is task delegation/execution; setupAgentTaskFixture's own
	// directly-created agent does the same).
	agentRow, err := db.GetAgentByAgentID(pairResp.AgentID)
	if err != nil {
		t.Fatalf("agent row must exist after pairing: %v", err)
	}
	agentRow.Enabled = true
	agentRow.Confirmed = true
	agentRow.SetPendingSAS(nil)
	agentRow.SetWhitelist([]string{fx.mount})
	if err := db.UpdateAgent(agentRow); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(agentRow.ID) })

	// 3. The task's creator delegates a cross-drive copy to the now-
	// whitelisted agent, through the real (authed) CreateAgentTask handler.
	body := `{"agent_id":"` + pairResp.AgentID + `","src_path":"` + fx.srcPath + `","dst_dir":"` + fx.dstDir + `"}`
	w := driveAuthedPost(fx.creator, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("CreateAgentTask: want 200, got %d: %s", got, w.Body.String())
	}
	createdTask := parseAgentTaskDTO(t, w)
	if createdTask.Status != model.AgentTaskPending {
		t.Fatalf("new task status = %q, want pending", createdTask.Status)
	}
	t.Cleanup(func() {
		if row, err := db.GetAgentTaskByTaskID(createdTask.TaskID); err == nil {
			_ = db.DeleteAgentTask(row.ID)
		}
	})

	// 4. The agent heartbeats for real — the SAME call `agent run`'s loop
	// makes — and the response must carry exactly this task, claimed, with
	// a fresh, verifiable capability token (design W12-3a's dispatch step).
	hbResp, err := agentpkg.Heartbeat(t.Context(), srv.URL, identity, pairResp.AgentID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if hbResp.Task == nil {
		t.Fatal("expected the heartbeat to dispatch the pending task")
	}
	if hbResp.Task.TaskID != createdTask.TaskID || hbResp.Task.SrcPath != fx.srcPath || hbResp.Task.DstDir != fx.dstDir {
		t.Fatalf("unexpected dispatched task: %+v", hbResp.Task)
	}
	if hbResp.Task.CapToken == "" {
		t.Fatal("expected a non-empty capability token")
	}

	// 5. The agent executes the task — download src, upload to dst, report
	// progress/status — via EXACTLY the function cmd/agent.go's `agent run`
	// calls.
	task := agentpkg.Task{
		TaskID:  hbResp.Task.TaskID,
		Type:    hbResp.Task.Type,
		SrcPath: hbResp.Task.SrcPath,
		DstDir:  hbResp.Task.DstDir,
	}
	if err := agentpkg.RunCopyTask(t.Context(), srv.URL, task, agentpkg.StaticToken(hbResp.Task.CapToken)); err != nil {
		t.Fatalf("RunCopyTask: %v", err)
	}

	// 6. The destination must hold EXACTLY the source's bytes, verified
	// directly on disk (fx.diskDir is the Local storage's own
	// root_folder_path — see setupAgentTaskFixture).
	dstPath := filepath.Join(fx.diskDir, "dst", "file.bin")
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("destination file must exist after RunCopyTask: %v", err)
	}
	if string(got) != "agent task payload" {
		t.Fatalf("destination content = %q, want %q", got, "agent task payload")
	}

	// 7. The task row itself must reflect the final, server-recorded
	// outcome: done, 100%.
	finalTask, err := db.GetAgentTaskByTaskID(createdTask.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if finalTask.Status != model.AgentTaskDone || finalTask.Progress != 100 {
		t.Fatalf("final task state = %+v, want status=done progress=100", finalTask)
	}
}
