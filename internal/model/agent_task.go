package model

import "time"

// AgentTask status values (design W12-3a: server-side foundation for
// cross-drive copy delegation). The state machine is deliberately linear
// and never backtracks out of a terminal state:
//
//	pending -> claimed -> running -> done
//	                              \-> failed
//
// pending/claimed both mean "not yet actually moving bytes" -- claimed is
// what db.ClaimPendingForAgent sets the instant a heartbeat picks the task
// up (single-flight: exactly one heartbeat call wins that transition, see
// that function's doc); the agent itself flips claimed -> running on its
// first progress report. done/failed are terminal: AgentTask.IsTerminal
// reports true for either, and every status-mutating endpoint in
// server/handles/agent_task.go treats a terminal task as frozen -- a
// done/failed task is never re-claimed and never re-finalized into a
// different terminal state (idempotent, not "last write wins").
const (
	AgentTaskPending = "pending"
	AgentTaskClaimed = "claimed"
	AgentTaskRunning = "running"
	AgentTaskDone    = "done"
	AgentTaskFailed  = "failed"
)

// AgentTaskCopy is the only Type this wave ever assigns: a cross-storage
// copy delegated to a paired remote agent. Later waves may add more Types;
// nothing here assumes copy is the only value forever.
const AgentTaskCopy = "copy"

// AgentTask is one unit of work delegated to a paired remote agent (design
// W12-3a). CreatorUID is load-bearing for the whole security model: the
// agent that executes this task acts within the CREATOR's authority, never
// beyond it -- every agent-facing endpoint re-runs op.Authorize against the
// creator's identity (loaded fresh via CreatorUID), not the agent's, and
// not a snapshot cached at creation time. See server/handles/agent_task.go's
// top-of-file doc for the full "capability is not a bypass" reasoning.
//
// SrcPath and DstDir are full virtual paths (already base-path-joined for
// CreatorUID at creation time -- see handles.CreateAgentTask), exactly what
// a capability token minted for this task scopes: read on SrcPath, write on
// DstDir. They are re-validated (not just re-read) on every agent-facing
// call: op.FixAndCleanPath/op.Authorize re-clean and re-decide, so a token
// can never be used to reach outside the exact paths it was minted for.
type AgentTask struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	TaskID     string `json:"task_id" gorm:"uniqueIndex"`
	AgentID    string `json:"agent_id" gorm:"index"`
	Type       string `json:"type"`
	SrcPath    string `json:"src_path"`
	DstDir     string `json:"dst_dir"`
	CreatorUID uint   `json:"creator_uid"`
	Status     string `json:"status"`
	Progress   int    `json:"progress"`
	Detail     string `json:"detail" gorm:"type:text"`
	// Attempts counts how many times db.RequeueStaleAgentTasks has reset this
	// task from claimed/running back to pending because its owning agent went
	// stale (design W12-4: reliability). It never decreases and is never
	// touched by anything else -- a fresh task starts at 0. Once it reaches
	// db.MaxAgentTaskAttempts, a further staleness sweep marks the task
	// failed instead of requeuing it again (see that function's doc): re-copy
	// on reassignment is cheap but not free, and a task whose assigned agent
	// keeps going stale needs a human, not an infinite retry loop.
	Attempts  int       `json:"attempts" gorm:"default:0"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IsTerminal reports whether Status is a terminal state (done or failed) --
// see the package-level status-constants doc for what that guarantees.
func (t *AgentTask) IsTerminal() bool {
	return t.Status == AgentTaskDone || t.Status == AgentTaskFailed
}
