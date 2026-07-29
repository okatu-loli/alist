package handles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AlistGo/alist-agent/proto"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Agent observability (design SP-G1): admin-only reads of a paired agent's
// recent LOGS and its dispatched-task PROGRESS, both over the EXISTING
// authenticated reverse data channel (logs) or the existing task store
// (progress) — no new port, no new trust boundary beyond the RequireAdmin gate
// the /agents group already applies.
//
// FAIL CLOSED: the log endpoint pulls over the live reverse channel; if the
// agent has no channel up right now it returns a clear "not connected" error
// (errAgentChannelNotConnected) rather than any partial or fabricated result,
// so the G2 frontend can distinguish "connected, no logs" from "通道未连接".

// errAgentChannelNotConnected is returned by AgentLogs when the target agent has
// no live reverse channel to pull logs over.
var errAgentChannelNotConnected = errors.New("agent is not connected over a reverse channel")

// agentLogTailTimeout bounds one server->agent log-tail round trip so a wedged
// or slow agent can never hang the admin request indefinitely.
const agentLogTailTimeout = 10 * time.Second

// AgentLogsResp is the wire body of GET /admin/agents/:id/logs — the G2
// contract. Entries are LogEntry values (internal/agent/logring.go: {seq, ts,
// level, msg}) newer than the requested ?since= cursor, oldest first; Next is
// the cursor to pass as ?since= on the following poll.
type AgentLogsResp struct {
	Entries []proto.LogEntry `json:"entries"`
	Next    int64            `json:"next"`
}

// AgentLogs pulls one page of a connected reverse agent's recent logs over the
// authenticated channel (design SP-G1). Admin only (router gate). ?since= is
// the incremental cursor (default 0 = from the oldest buffered entry).
// @Summary Tail a connected agent's recent logs
// @Description Pulls recent log lines from a reverse-connected agent over the authenticated channel. Admin only. Fails closed if the agent is not connected.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param id path int true "agent id"
// @Param since query int false "return entries with seq greater than this cursor"
// @Success 200 {object} common.Resp[AgentLogsResp]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/agents/{id}/logs [get]
func AgentLogs(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	a, err := db.GetAgent(uint(id))
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		common.ErrorResp(c, err, code)
		return
	}

	// Fail closed: only a live, authenticated reverse channel can serve logs.
	ch := reverseChannelFor(a.AgentID)
	if ch == nil {
		common.ErrorResp(c, errAgentChannelNotConnected, 503)
		return
	}

	since, _ := strconv.ParseInt(c.Query("since"), 10, 64)
	reqPayload, err := json.Marshal(proto.ChannelLogTailReq{Since: since})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), agentLogTailTimeout)
	defer cancel()
	raw, err := ch.Send(ctx, proto.MethodAgentLogTail, reqPayload)
	if err != nil {
		// Channel-level failure (dropped mid-call, timeout, agent error frame):
		// surface it, don't pretend there were no logs.
		common.ErrorResp(c, fmt.Errorf("agent log tail over channel: %w", err), 502)
		return
	}

	var resp proto.ChannelLogTailResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		common.ErrorResp(c, fmt.Errorf("decode agent log tail response: %w", err), 502)
		return
	}
	if resp.Entries == nil {
		resp.Entries = []proto.LogEntry{}
	}
	// Never trust the far end's self-limit: re-clamp the page, adjusting Next so
	// the client re-polls anything we dropped rather than skipping it.
	if len(resp.Entries) > proto.MaxLogTailEntries {
		resp.Entries = resp.Entries[:proto.MaxLogTailEntries]
		resp.Next = resp.Entries[len(resp.Entries)-1].Seq
	}
	// Also cap per-entry message length: the untrusted agent could return
	// arbitrarily large msgs (up to the channel frame limit) every poll to bloat
	// the admin's DOM. Count is clamped above; clamp size here too. Rune-safe so
	// a multi-byte char is never split into invalid UTF-8.
	const maxLogEntryRunes = 2000
	for i := range resp.Entries {
		if r := []rune(resp.Entries[i].Msg); len(r) > maxLogEntryRunes {
			resp.Entries[i].Msg = string(r[:maxLogEntryRunes]) + "…"
		}
	}

	common.SuccessResp(c, AgentLogsResp{Entries: resp.Entries, Next: resp.Next})
}

// AgentTaskProgressDTO is the wire form of one dispatched task's progress for
// GET /admin/agents/:id/tasks — the G2 contract. It is sourced ENTIRELY from
// the existing task store (db.ListAgentTasks), which the progress/status cap
// ops already keep current (server/handles/agent_task_core.go): there is no
// parallel progress store.
type AgentTaskProgressDTO struct {
	TaskID string `json:"task_id"`
	Type   string `json:"type"`
	// Src doubles as the task's display name (its source path).
	Src         string    `json:"src"`
	Dst         string    `json:"dst"`
	Status      string    `json:"status"`       // queued | running | done | failed
	ProgressPct int       `json:"progress_pct"` // 0..100
	Error       string    `json:"error,omitempty"`
	Attempts    int       `json:"attempts"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Rapid is true only when the task is done AND completed via hash-based
	// rapid-upload ("秒传", design SP-H): the agent reports the shared
	// proto.RapidUploadDetail marker as the done-status detail. We surface
	// only this boolean, never the raw detail string, so a done detail that
	// ever carried some other value can't leak through this field.
	Rapid bool `json:"rapid"`
}

// agentTaskDisplayStatus collapses the internal state machine
// (pending/claimed/running/done/failed) onto the four states the observability
// UI shows: pending and claimed both mean "not yet moving bytes" and surface as
// "queued"; the rest map one-to-one.
func agentTaskDisplayStatus(s string) string {
	switch s {
	case model.AgentTaskPending, model.AgentTaskClaimed:
		return "queued"
	case model.AgentTaskRunning:
		return model.AgentTaskRunning
	case model.AgentTaskDone:
		return model.AgentTaskDone
	case model.AgentTaskFailed:
		return model.AgentTaskFailed
	default:
		return s
	}
}

func toAgentTaskProgressDTO(t model.AgentTask) AgentTaskProgressDTO {
	dto := AgentTaskProgressDTO{
		TaskID:      t.TaskID,
		Type:        t.Type,
		Src:         t.SrcPath,
		Dst:         t.DstDir,
		Status:      agentTaskDisplayStatus(t.Status),
		ProgressPct: t.Progress,
		Attempts:    t.Attempts,
		UpdatedAt:   t.UpdatedAt,
	}
	// Detail holds the failure reason on a failed task (the status cap op stores
	// it there). Surface it as `error` ONLY then, so a running task's progress
	// detail is never mistaken for an error.
	if t.Status == model.AgentTaskFailed {
		dto.Error = t.Detail
	}
	// A done task whose stored detail is the shared rapid-upload marker completed
	// via 秒传 (design SP-H3). Gate on done so a running task's transient detail
	// can never be mistaken for a rapid completion, and compare against the exact
	// shared const the agent writes so the two ends can't drift.
	if t.Status == model.AgentTaskDone && t.Detail == proto.RapidUploadDetail {
		dto.Rapid = true
	}
	return dto
}

// AgentTasks returns the tasks dispatched to one agent with their live progress
// (design SP-G1). Admin only (router gate). ?limit=/?offset= page it
// (db.ListAgentTasks applies its own default/max bound). Newest first.
// @Summary List an agent's dispatched tasks with progress
// @Description Lists the delegated tasks for one agent with status/progress, newest first. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param id path int true "agent id"
// @Param limit query int false "page size (default 50, max 200)"
// @Param offset query int false "page offset"
// @Success 200 {object} common.Resp[[]AgentTaskProgressDTO]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/agents/{id}/tasks [get]
func AgentTasks(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	a, err := db.GetAgent(uint(id))
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		common.ErrorResp(c, err, code)
		return
	}

	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	tasks, err := db.ListAgentTasks(a.AgentID, limit, offset)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dtos := make([]AgentTaskProgressDTO, 0, len(tasks))
	for _, t := range tasks {
		dtos = append(dtos, toAgentTaskProgressDTO(t))
	}
	common.SuccessResp(c, dtos)
}
