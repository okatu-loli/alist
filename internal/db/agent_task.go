package db

import (
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Remote-agent task-delegation CRUD (design W12-3a: server-side foundation
// for cross-drive copy delegation). No caching layer — same reasoning as
// agent.go: every agent-facing capability call re-loads the task row fresh
// (see server/handles/agent_task.go's resolveTaskCap), so a stale cached
// read could let a de-whitelisted, mutated, or already-terminal task keep
// being served past the point it should have been cut off.

// CreateAgentTask inserts a new task row.
func CreateAgentTask(t *model.AgentTask) error {
	return errors.WithStack(db.Create(t).Error)
}

// GetAgentTask loads a task by primary key. A missing id maps to
// errs.AgentTaskNotFound.
func GetAgentTask(id uint) (*model.AgentTask, error) {
	var t model.AgentTask
	err := db.First(&t, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.AgentTaskNotFound
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &t, nil
}

// GetAgentTaskByTaskID loads a task by its public TaskID (the identifier a
// capability token's claims and the /agent/task/:id/* URLs carry — the
// numeric primary key is never exposed to an agent). A missing task_id maps
// to errs.AgentTaskNotFound.
func GetAgentTaskByTaskID(taskID string) (*model.AgentTask, error) {
	var t model.AgentTask
	err := db.Where("task_id = ?", taskID).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.AgentTaskNotFound
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &t, nil
}

// defaultAgentTaskListLimit/maxAgentTaskListLimit bound ListAgentTasks'
// paging (design W12-4: task visibility). limit<=0 falls back to the
// default page size; limit above the max is clamped down to it -- an admin
// endpoint backed by this must never be able to force an unbounded scan of
// the table via a huge or missing ?limit=.
const (
	defaultAgentTaskListLimit = 50
	maxAgentTaskListLimit     = 200
)

// ListAgentTasks lists tasks newest-first (id DESC -- ids are assigned in
// creation order, so this is equivalently created-at-descending without
// depending on clock resolution), optionally filtered to a single agentID
// (empty string means "every agent"), bounded to at most
// maxAgentTaskListLimit rows starting at offset. Backs the admin task-list
// endpoint (server/handles.AdminListAgentTasks) -- a read, not itself a
// trust boundary (the admin gate is the router middleware), so this applies
// no authorization of its own.
func ListAgentTasks(agentID string, limit, offset int) ([]model.AgentTask, error) {
	if limit <= 0 {
		limit = defaultAgentTaskListLimit
	} else if limit > maxAgentTaskListLimit {
		limit = maxAgentTaskListLimit
	}
	if offset < 0 {
		offset = 0
	}
	q := db.Model(&model.AgentTask{}).Order("id DESC")
	if agentID != "" {
		q = q.Where("agent_id = ?", agentID)
	}
	var ts []model.AgentTask
	if err := q.Limit(limit).Offset(offset).Find(&ts).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return ts, nil
}

// UpdateAgentTask persists the full row (caller already merged the partial
// update onto a freshly loaded row — same pattern as UpdateAgent).
func UpdateAgentTask(t *model.AgentTask) error {
	return errors.WithStack(db.Save(t).Error)
}

// DeleteAgentTask removes a task row by id.
func DeleteAgentTask(id uint) error {
	return errors.WithStack(db.Delete(&model.AgentTask{}, id).Error)
}

// staleAgentTaskAfter is the threshold ClaimPendingForAgent sweeps with on
// every call (design W12-4 (B): lazy sweep, see that function's doc for why
// lazy over a ticker). Comfortably larger (10x) than agentHeartbeatInterval
// (cmd/agent.go, 30s) -- THE safety property this whole feature rests on:
// because design W12-4 (A) makes the agent's heartbeat tick on its own
// schedule concurrently with any in-flight copy (never blocked by it, see
// internal/agent/runner.go), a genuinely alive agent's LastSeen is at most
// one heartbeat interval old at any moment, no matter how long its current
// copy has been running. A threshold this many multiples of that interval
// means an agent has to miss on the order of ten consecutive heartbeats --
// i.e. actually be down, not merely busy -- before RequeueStaleAgentTasks
// will ever touch one of its tasks. See that function's doc and
// TestRequeueStaleAgentTasksNeverTouchesFreshAgent for the pinned property.
const staleAgentTaskAfter = 5 * time.Minute

// ClaimPendingForAgent atomically claims AT MOST ONE pending task for the
// given agent, returning (nil, nil) if there is none to claim right now —
// callers must treat "lost the race" and "genuinely nothing pending" as the
// exact same outcome (there is no way, and no need, to tell them apart).
//
// Lazy stale-agent sweep (design W12-4 (B)): every call first runs
// RequeueStaleAgentTasks(staleAgentTaskAfter) -- a global sweep, not scoped
// to agentID -- so any task left claimed/running by a NOW genuinely-dead
// agent (anywhere in the system, not just this one) becomes pending again
// before this call's own SELECT runs. This is the "lazy" placement the
// design calls out as simplest and race-safe: it needs no extra goroutine,
// and it can never race its own claim query below, because the sweep only
// ever touches rows in status claimed/running while the claim query below
// only ever touches rows in status pending -- a single row can be in
// exactly one of those states at a time, so the two queries can never both
// match the same row in the same instant.
//
// Single-flight mechanism: a SELECT first picks a CANDIDATE row (the oldest
// still-pending task for this agent, by id) with no locking of its own —
// that's fine, because the actual compare-and-swap is the UPDATE that
// follows: `UPDATE ... SET status='claimed' WHERE id=? AND status='pending'`.
// That single statement is the atomic unit two concurrent callers race on.
// Whichever caller's UPDATE commits first flips the row's status; the
// LOSING caller's otherwise-identical UPDATE then matches zero rows (the
// WHERE status='pending' no longer holds), and RowsAffected==0 tells it so.
// This holds under any backend's normal transaction isolation because it
// never depends on the SELECT's snapshot being current by the time the
// UPDATE runs — only the UPDATE's own WHERE clause, evaluated atomically by
// the database at UPDATE time, decides the winner.
//
// A losing caller does NOT retry against a different candidate in the same
// call — it simply reports "no task this round". That is deliberate: the
// caller here is always a heartbeat handler, and the next heartbeat (from
// this agent or, if there were several pending tasks, whichever one wins
// next) will pick it up. There is no requirement that a single heartbeat
// claim more than one task.
func ClaimPendingForAgent(agentID string) (*model.AgentTask, error) {
	if _, err := RequeueStaleAgentTasks(staleAgentTaskAfter); err != nil {
		return nil, err
	}

	var candidate model.AgentTask
	err := db.Where("agent_id = ? AND status = ?", agentID, model.AgentTaskPending).
		Order("id").First(&candidate).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}

	res := db.Model(&model.AgentTask{}).
		Where("id = ? AND status = ?", candidate.ID, model.AgentTaskPending).
		Update("status", model.AgentTaskClaimed)
	if res.Error != nil {
		return nil, errors.WithStack(res.Error)
	}
	if res.RowsAffected == 0 {
		// Lost the race: some other caller's UPDATE already flipped this row
		// (or, less likely, it was terminated in the meantime). Either way,
		// this call claims nothing.
		return nil, nil
	}
	candidate.Status = model.AgentTaskClaimed
	return &candidate, nil
}

// GetActiveAgentTaskForAgent returns the oldest task currently CLAIMED or
// RUNNING for the given agent, or (nil, nil) if it has none. Unlike
// ClaimPendingForAgent this never changes any row's status — it exists so a
// heartbeat can keep handing the agent its already-in-flight task (with a
// freshly re-issued capability token) across reconnects, without that
// re-surfacing counting as a new claim.
func GetActiveAgentTaskForAgent(agentID string) (*model.AgentTask, error) {
	var t model.AgentTask
	err := db.Where("agent_id = ? AND status IN ?", agentID,
		[]string{model.AgentTaskClaimed, model.AgentTaskRunning}).
		Order("id").First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &t, nil
}

// MaxAgentTaskAttempts caps how many times a single task may be requeued by
// RequeueStaleAgentTasks after its owning agent goes stale. A task's
// Attempts starts at 0 and is incremented by exactly one on every requeue
// (never on a plain claim); once it would reach MaxAgentTaskAttempts, the
// NEXT staleness sweep marks the task failed instead of requeuing it again
// — see RequeueStaleAgentTasks' doc for exactly why and the SQL boundary.
const MaxAgentTaskAttempts = 3

// agentTaskFailedNoHealthyAgentDetail is the Detail a task is finalized
// with when RequeueStaleAgentTasks gives up on it (MaxAgentTaskAttempts
// reassignments already spent, and its agent is STILL stale) rather than
// requeuing it forever.
const agentTaskFailedNoHealthyAgentDetail = "no healthy agent: exceeded max reassignment attempts"

// staleAgentIDsQuery returns a fresh (never previously executed/reused)
// subquery selecting the AgentID of every agent whose LastSeen is NULL (has
// never sent a heartbeat) or older than cutoff. Built fresh on each call
// because a *gorm.DB query builder carries mutable session state that is
// not safe to reuse verbatim across two independent statements.
func staleAgentIDsQuery(cutoff time.Time) *gorm.DB {
	return db.Model(&model.Agent{}).Select("agent_id").
		Where("last_seen IS NULL OR last_seen < ?", cutoff)
}

// RequeueStaleAgentTasks is the reliability sweep (design W12-4 (B)): any
// task still claimed/running whose OWNING AGENT has gone stale — LastSeen
// older than staleAfter, or an agent that never once heartbeated — is either
// reset to pending (so it becomes claimable again, incrementing Attempts)
// or, if it has already been requeued MaxAgentTaskAttempts times and its
// agent is STILL stale, finalized as failed with a "no healthy agent"
// detail. Returns the total number of rows changed (requeued + failed
// combined — both are "the sweep acted on this task", the distinction is in
// Status/Detail on read).
//
// # THE safety property: a live-but-busy agent is never touched
//
// This is gap (1)'s fix, but it is only SAFE paired with design W12-4 (A)
// (internal/agent/runner.go's concurrent heartbeat): before that change,
// `agent run` blocked its heartbeat loop for the ENTIRE duration of a copy,
// so a long-running-but-perfectly-healthy transfer would let LastSeen go
// stale — and requeuing that task would hand the SAME work to a second
// claim while the first agent was still mid-write, i.e. concurrent writers
// on one destination. With (A), the heartbeat ticks on its own schedule
// (agentHeartbeatInterval, 30s in cmd/agent.go) independently of however
// long the in-flight copy takes, so a genuinely alive agent's LastSeen is
// never more than one interval old — no matter whether it has been copying
// for ten seconds or ten hours. staleAfter here (via the caller-supplied
// duration; ClaimPendingForAgent's lazy sweep uses staleAgentTaskAfter =
// 5m, ~10x that interval) is chosen to sit comfortably above that: an agent
// has to miss on the order of ten consecutive heartbeats — i.e. actually be
// unreachable/crashed, not merely busy — before this function will ever
// touch one of its tasks. TestRequeueStaleAgentTasksNeverTouchesFreshAgent
// pins this as a hard property: a running task whose agent's LastSeen is
// literally "now" is asserted UNCHANGED, every field, after a sweep call.
//
// # Two atomic UPDATEs, mutually exclusive by their own WHERE clauses
//
// Both statements below share the same base predicate — status IN
// (claimed, running), agent_id IN (the stale-agent subquery), AND the
// task's own updated_at older than cutoff too (see "second guard" below) —
// split only by Attempts, so no row can ever match both:
//
//	-- requeue (Attempts < MaxAgentTaskAttempts): give it another shot
//	UPDATE agent_tasks
//	   SET status = 'pending', attempts = attempts + 1, updated_at = ?
//	 WHERE status IN ('claimed','running')
//	   AND agent_id IN (SELECT agent_id FROM agents
//	                      WHERE last_seen IS NULL OR last_seen < ?)
//	   AND updated_at < ?
//	   AND attempts < ?
//
//	-- give up (Attempts already at the cap): stop retrying, surface it
//	UPDATE agent_tasks
//	   SET status = 'failed', detail = 'no healthy agent: ...', updated_at = ?
//	 WHERE status IN ('claimed','running')
//	   AND agent_id IN (SELECT agent_id FROM agents
//	                      WHERE last_seen IS NULL OR last_seen < ?)
//	   AND updated_at < ?
//	   AND attempts >= ?
//
// Each UPDATE is, on its own, the same atomic compare-and-set primitive
// ClaimPendingForAgent's doc describes for the claimed->claimed transition:
// the database evaluates each row's WHERE clause at UPDATE time, so two
// concurrent sweeps (or a sweep racing a claim, see ClaimPendingForAgent's
// doc) can never both act on the same row in conflicting ways.
//
// # Requeue keeps the task's AgentID (deliberate, and a documented scope
// boundary)
//
// A task is bound to ONE specific agent at creation (CreateAgentTaskReq.
// AgentID — the creator explicitly picks which paired agent does the copy,
// storage-whitelist and all); nothing in this codebase lets a task move to
// a DIFFERENT agent's identity automatically. Requeuing therefore resets
// Status to pending but leaves AgentID untouched: the task becomes
// claimable again the next time THAT SAME agent (identified by its
// long-term ed25519 identity, not by "whichever agent happens to poll
// next") sends a heartbeat and calls ClaimPendingForAgent with its own
// AgentID — e.g. after that agent process restarts and would otherwise have
// no way to ever recover a task it had claimed/running in a previous life
// (ClaimPendingForAgent only ever claims status=pending rows: before this
// function existed, a claimed/running row was invisible to every future
// heartbeat, including the SAME agent's). Automatic reassignment to a
// DIFFERENT agent identity — e.g. if the original is permanently gone — is
// an explicit non-goal of this wave; an operator can always create a fresh
// task against a different agent by hand. See the design doc's own
// "idempotency" note: a re-copy on reassignment always overwrites the
// destination from scratch, which is what makes reclaiming (by the same or,
// in a future wave, a different agent) safe without resumable/offset
// transfer.
//
// # Second guard: the task's OWN updated_at, not just the agent's LastSeen
//
// Gating purely on the agent's LastSeen has a tight but real race: the
// instant ClaimPendingForAgent flips a row pending->claimed, that row's
// AgentID is (by construction, still) an agent whose LastSeen has not
// necessarily been refreshed yet (e.g. a concurrent duplicate/retried
// heartbeat racing the one that just claimed it, or this function being
// invoked directly and concurrently with a claim, as
// TestClaimPendingConcurrentWithRequeueNeverDoubleClaims exercises) — a
// SECOND, independent sweep call, evaluated a moment later, would see that
// SAME just-claimed row (status='claimed' is itself in the swept-status
// set) against a still-stale LastSeen and requeue it right back to pending,
// letting a THIRD caller claim it again: two "winners" for one task inside
// one race window, even though only ever one agent identity could complete
// either claim. Both UPDATEs below additionally require
// `updated_at < cutoff` (the SAME cutoff as the LastSeen check) — and every
// successful claim/requeue/status update touches updated_at to "now" as a
// side effect — so a row that was touched inside the staleness window can
// never be swept again until it has ALSO sat untouched for a full
// staleAfter on its own account. This is strictly additive (it only ever
// narrows what the LastSeen check alone would sweep), so it changes nothing
// about the primary contract ("stale agent's abandoned task becomes
// claimable again") — it only closes the reclaim-vs-resweep race above.
func RequeueStaleAgentTasks(staleAfter time.Duration) (int, error) {
	cutoff := time.Now().Add(-staleAfter)
	now := time.Now()
	activeStatuses := []string{model.AgentTaskClaimed, model.AgentTaskRunning}

	requeued := db.Model(&model.AgentTask{}).
		Where("status IN ?", activeStatuses).
		Where("agent_id IN (?)", staleAgentIDsQuery(cutoff)).
		Where("updated_at < ?", cutoff).
		Where("attempts < ?", MaxAgentTaskAttempts).
		Updates(map[string]any{
			"status":     model.AgentTaskPending,
			"attempts":   gorm.Expr("attempts + 1"),
			"updated_at": now,
		})
	if requeued.Error != nil {
		return 0, errors.WithStack(requeued.Error)
	}

	failed := db.Model(&model.AgentTask{}).
		Where("status IN ?", activeStatuses).
		Where("agent_id IN (?)", staleAgentIDsQuery(cutoff)).
		Where("updated_at < ?", cutoff).
		Where("attempts >= ?", MaxAgentTaskAttempts).
		Updates(map[string]any{
			"status":     model.AgentTaskFailed,
			"detail":     agentTaskFailedNoHealthyAgentDetail,
			"updated_at": now,
		})
	if failed.Error != nil {
		return 0, errors.WithStack(failed.Error)
	}

	return int(requeued.RowsAffected + failed.RowsAffected), nil
}
