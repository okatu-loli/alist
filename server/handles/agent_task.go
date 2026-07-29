package handles

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Remote-agent task-delegation API (design W12-3a: server-side foundation
// for cross-drive copy delegation — direct-drive transfer, agent-side
// execution deferred to W12-3b). This file is, alongside agent_pair.go, the
// crypto/authz-critical half of the remote-agent feature.
//
// # Capability is not a bypass
//
// Every agent-facing endpoint below (SrcLink, PutContent, Progress, Status)
// is reached with NO user session — an agent authenticates a call purely by
// presenting a capability token (agentpkg.VerifyTaskCap). That token proves
// exactly one thing: "the server, at issuance time, minted a token scoping
// this agent to read <src> and write <dst_dir> for this task, and it has
// not yet expired." It is a NARROWING, never a GRANT: it can never let an
// agent (or the token's bearer, if stolen) do anything the task's CREATOR
// could not already do themselves, and it can never survive the creator's,
// the agent's, or the task's authority being revoked out from under it.
// Concretely, that means every call in this file re-derives its own
// authorization from scratch, from the LIVE database, never from the token
// alone:
//
//  1. resolveTaskCap verifies the token, binds it to the :id path param (a
//     token minted for task A is refused at task B's endpoint — cross-task
//     replay is rejected), reloads the task row and requires its live
//     AgentID/SrcPath/DstDir to still match the token's claims (a task
//     mutated after the token was issued invalidates it), and reloads the
//     agent row and requires it to still exist and be Enabled (disabling an
//     agent is a live revocation, checked on every call, not just at
//     claim time).
//  2. Each endpoint THEN re-checks the storage whitelist
//     (storageWhitelisted) for the specific path it is about to touch, and
//     re-runs op.Authorize for the specific action (ActRead for src-link,
//     ActWrite for content) against the TASK'S CREATOR — loaded fresh via
//     CreatorUID, never cached — so an admin narrowing the whitelist, or a
//     policy change that would now deny the creator, cuts the agent off on
//     the very next call, mid-task, without needing to touch the agent or
//     the token at all.
//
// No single one of these checks is trusted alone; all of them run, every
// time, on every call.
//
// One more note: PutContent's body streams straight through to op.Put via
// an io.Reader — never buffered whole in memory — so there is no
// in-memory size cap to apply the way maxAgentBodyBytes bounds the small
// JSON control-plane bodies below (that constant is deliberately NOT
// applied to the content endpoint).

// --- task creation (authed: the creator, any logged-in user) ---

// CreateAgentTaskReq is the wire body for POST /agent/tasks.
type CreateAgentTaskReq struct {
	AgentID string `json:"agent_id" binding:"required"`
	SrcPath string `json:"src_path" binding:"required"`
	DstDir  string `json:"dst_dir" binding:"required"`
}

// AgentTaskDTO is the wire form of a delegated task. ID and Attempts
// (design W12-4: task visibility + reliability) were added alongside the
// admin task-list endpoint below — every OTHER endpoint that already
// returned this DTO (CreateAgentTask, AgentTaskProgress, AgentTaskStatus)
// gains these two fields too, additively; nothing that already parsed this
// shape breaks.
type AgentTaskDTO struct {
	ID        uint      `json:"id"`
	TaskID    string    `json:"task_id"`
	AgentID   string    `json:"agent_id"`
	Type      string    `json:"type"`
	SrcPath   string    `json:"src_path"`
	DstDir    string    `json:"dst_dir"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Detail    string    `json:"detail"`
	Attempts  int       `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toAgentTaskDTO(t model.AgentTask) AgentTaskDTO {
	return AgentTaskDTO{
		ID:        t.ID,
		TaskID:    t.TaskID,
		AgentID:   t.AgentID,
		Type:      t.Type,
		SrcPath:   t.SrcPath,
		DstDir:    t.DstDir,
		Status:    t.Status,
		Progress:  t.Progress,
		Detail:    t.Detail,
		Attempts:  t.Attempts,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

// taskIDLen mirrors agentpkg's pairing-code entropy budget (see
// internal/agent/pairing.go's codeLen) — a task id is not secret (it's the
// public :id path segment; the capability token is the actual secret), but
// there is no reason to make it any easier to enumerate than it needs to be.
const taskIDLen = 32

// minFallbackSignTTL is the floor on AgentTaskSrcLink's proxied-fallback
// sign TTL (see that handler): the cap token's own remaining life bounds the
// signed link from above, but a token with only a sliver of life left would
// otherwise produce a link that is already-or-nearly dead on arrival. This
// floor keeps the link usable without reintroducing an unbounded lifetime —
// it is seconds, not the "never expires" default this fix removes.
const minFallbackSignTTL = 5 * time.Second

// storageWhitelisted resolves path to its owning storage and reports
// whether it is covered by agent's StorageWhitelist. A whitelist entry
// matches either the storage's ACTUAL (virtual) mount path (e.g. "/nas" —
// utils.GetActualMountPath, which strips a balance member's ".balanceN"
// suffix and is a no-op for a plain, non-balance mount) or its numeric id in
// "storage:<id>" form (e.g. "storage:3"). An agent with an EMPTY whitelist
// matches nothing, by construction (the loop below simply never finds a hit
// against an empty slice) — see model.Agent's own doc: empty is the safe
// default.
//
// Group-level, resolution-stable (fix for balance-group whitelist
// TOCTOU/mismatch): op.GetStorageAndActualPath resolves through
// GetBalancedStorage, which round-robins across a balance group's member
// storages on every call — including the later call this same path makes
// from fs.Link/fs.PutDirectly to actually perform the op. Comparing against
// the RAW storage.GetStorage().MountPath (which carries the ".balanceN"
// suffix for every member but the group's bare one) had two failure modes:
// (1) a whitelist entry "/nas" never matched a suffixed member, so an agent
// hitting a suffixed member on this call got an intermittent false 403 even
// though "/nas" was clearly meant to cover the mount; (2) because this
// check's resolution and the op's later resolution round-robin
// independently, the check could pass against one member while the op it
// gates actually executes against a DIFFERENT, non-whitelisted member of the
// same group.
//
// All members of one balance group share exactly one actual mount path
// (that is what makes them a group, and what op.GetActualMountPath strips
// down to), and op treats the group as a single logical mount — so keying
// the whitelist decision on the actual mount path instead of the resolved
// member's raw one makes "whitelisted" a property of the GROUP, not of
// whichever member the round-robin happened to hand back on this
// particular call. That is what makes "whitelisted ⟺ op-reachable" hold:
// the check's outcome for a given path no longer depends on which member
// resolved it, so it can never diverge from whatever member the later fs
// op independently resolves to.
//
// The id form is kept as a convenience match against the SPECIFIC member
// this call happened to resolve to, not the whole group — a balance group's
// members have distinct storage ids, so whitelisting one member's id
// whitelists only that member (and, since members are interchangeable
// within a group, effectively whitelists the group only if every member's
// id is listed). Admins whitelisting a balance group should prefer the
// path/actual-mount-path form, which is unambiguous and covers the group in
// one entry; the id form remains exact and rename-survivable for the common,
// non-balance case.
func storageWhitelisted(a *model.Agent, path string) (bool, error) {
	storage, _, err := op.GetStorageAndActualPath(path)
	if err != nil {
		return false, err
	}
	actualMP := utils.GetActualMountPath(storage.GetStorage().MountPath)
	idForm := fmt.Sprintf("storage:%d", storage.GetStorage().ID)
	for _, w := range a.Whitelist() {
		if w == actualMP || w == idForm {
			return true, nil
		}
	}
	return false, nil
}

// rejectLocalAgentStorage resolves path to its owning storage and reports
// errs.AgentLocalDriverUnsupported if that storage's driver is Local — see
// that error's doc for why Local is categorically unreachable for a remote
// agent, no matter how the whitelist/authz checks come out. Called from
// CreateAgentTask only (after whitelist+authz already passed): the agent-
// facing capability endpoints (src-link, put-content) never needed their
// own copy of this check, because a task whose src/dst can never resolve to
// Local is never created in the first place.
func rejectLocalAgentStorage(path string) error {
	storage, _, err := op.GetStorageAndActualPath(path)
	if err != nil {
		return err
	}
	if storage.GetStorage().Driver == "Local" {
		return errs.AgentLocalDriverUnsupported
	}
	return nil
}

// CreateAgentTask delegates a cross-drive copy to a paired agent. The
// caller (any logged-in user, not admin-only — a user may delegate their
// own copy) becomes the task's CreatorUID: the agent later acts within
// THEIR authority, never beyond it (see this file's top-of-file doc).
// Rejects if: the agent is unknown or disabled; the caller themselves is
// not authorized (ActRead on src_path, ActWrite on dst_dir) — a task is
// never created for a copy the creator could not already do themselves;
// or the agent's storage whitelist does not cover BOTH the source and
// destination storage.
// @Summary Create an agent task
// @Description Delegates a cross-drive copy to a paired, enabled agent whose storage whitelist covers both src and dst, and for which the caller is themselves authorized (read src, write dst). Authed (any user, not admin-only).
// @Tags agent
// @Security ApiKeyAuth
// @Param body body CreateAgentTaskReq true "task"
// @Success 200 {object} common.Resp[AgentTaskDTO]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/agent/tasks [post]
func CreateAgentTask(c *gin.Context) {
	var req CreateAgentTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)

	srcPath, err := user.JoinPath(req.SrcPath)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	dstDir, err := user.JoinPath(req.DstDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	target := "agent:" + req.AgentID

	a, err := db.GetAgentByAgentID(req.AgentID)
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, err, code)
		return
	}
	if !a.Enabled {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, errs.AgentDisabled, 403)
		return
	}

	// The creator must themselves be able to do this exact copy — a task is
	// never a way to reach further than the creator's own RBAC already
	// allows (see top-of-file doc).
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: srcPath}); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.AuthzErrorResp(c, err, "permission denied on source")
		return
	}
	// srcPath only proves the caller may read the source; the copy also
	// writes into dstDir, which needs its own authorization independently
	// (mirrors FsCopy's srcDir/dstDir split in fsmanage.go).
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: dstDir}); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.AuthzErrorResp(c, err, "permission denied on destination")
		return
	}

	if ok, err := storageWhitelisted(a, srcPath); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, err, 400)
		return
	} else if !ok {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, errs.StorageNotWhitelisted, 403)
		return
	}
	if ok, err := storageWhitelisted(a, dstDir); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, err, 400)
		return
	} else if !ok {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, errs.StorageNotWhitelisted, 403)
		return
	}

	// Neither side may resolve to Local — see errs.AgentLocalDriverUnsupported.
	// Checked last, after authz+whitelist, so an otherwise-invalid request
	// still reports ITS most specific reason first (unchanged behavior for
	// every other rejection); this is purely an additional, final gate.
	if err := rejectLocalAgentStorage(srcPath); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, err, 400)
		return
	}
	if err := rejectLocalAgentStorage(dstDir); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, err, 400)
		return
	}

	t := &model.AgentTask{
		TaskID:     random.String(taskIDLen),
		AgentID:    a.AgentID,
		Type:       model.AgentTaskCopy,
		SrcPath:    srcPath,
		DstDir:     dstDir,
		CreatorUID: user.ID,
		Status:     model.AgentTaskPending,
	}
	if err := db.CreateAgentTask(t); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.task.create", target, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "agent.task.create", "task:"+t.TaskID, true)
	common.SuccessResp(c, toAgentTaskDTO(*t))
}

// --- agent-facing capability endpoints (anonymous; cap_token-authed) ---

// resolveTaskCapCore (server/handles/agent_task_core.go) is the single,
// transport-agnostic choke point every agent-facing capability op funnels
// through before touching anything else — see its doc and this file's
// top-of-file doc for the full reasoning. As of R3 it is shared verbatim by
// the HTTP handlers below AND the reverse-channel handlers in
// agent_channel.go, so the re-authorization is provably identical on both
// transports.

// AgentTaskSrcLinkReq is the wire body for POST /agent/task/:id/src-link.
type AgentTaskSrcLinkReq struct {
	CapToken string `json:"cap_token" binding:"required"`
}

// AgentTaskSrcLinkResp is the response: either a genuine direct-from-drive
// URL (the storage driver produced one and does not require server
// proxying — Proxied=false), or a signed URL back at THIS server's existing
// /d download route (Proxied=true) for storages that have no directly
// reachable URL (e.g. Local) or that require proxying (WebProxy/MustProxy).
// A driver-specific direct presigned upload/download path beyond what op.Link
// already exposes is a future enhancement, not attempted here.
type AgentTaskSrcLinkResp struct {
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
	Size    int64       `json:"size"`
	Proxied bool        `json:"proxied"`
}

// AgentTaskSrcLink issues a download link for a task's source file. See
// resolveTaskCap and this file's top-of-file doc for the full capability
// re-verification chain; this handler additionally re-checks the storage
// whitelist and op.Authorize(ActRead) for claims.Src against the task's
// CREATOR (never the agent) before calling fs.Link.
// @Summary Get a task's source download link
// @Description Anonymous, cap_token-authed. Re-verifies the whitelist and read-authorization for the creator on every call.
// @Tags agent
// @Param id path string true "task id"
// @Param body body AgentTaskSrcLinkReq true "capability token"
// @Success 200 {object} common.Resp[AgentTaskSrcLinkResp]
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any]
// @Router /v4/agent/task/{id}/src-link [post]
func AgentTaskSrcLink(c *gin.Context) {
	taskID := c.Param("id")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req AgentTaskSrcLinkReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	// All authorization + fs.Link + response-building lives in the shared,
	// transport-agnostic core (agent_task_core.go), which the reverse-channel
	// handler calls identically — this handler is now a thin adapter: parse
	// the gin request, thread the request-scoped ctx (so the audit trail keeps
	// the caller IP) and the request-derived API base for the /d fallback,
	// render the response. boundAgentID is "" because HTTP has no channel
	// identity to bind to.
	resp, ce := agentTaskSrcLinkCore(c, "", common.GetApiUrl(c.Request), taskID, req.CapToken)
	if ce != nil {
		ce.writeGin(c)
		return
	}
	common.SuccessResp(c, resp)
}

// AgentTaskTryRapidReq is the wire body for POST /agent/task/:id/rapid. Like
// every cap op it carries only the task-scoped capability token — NO filename
// or path, so the destination filename can only ever come from the token's own
// Src (see agentTaskTryRapidCore).
type AgentTaskTryRapidReq struct {
	CapToken string `json:"cap_token" binding:"required"`
}

// AgentTaskTryRapidResp reports whether the hash-based rapid-upload hit
// (Rapid=true: the file is registered, no bytes moved) or the agent must fall
// back to streaming (Rapid=false).
type AgentTaskTryRapidResp struct {
	Rapid bool `json:"rapid"`
}

// AgentTaskTryRapid attempts a hash-based rapid-upload ("秒传") of a task's
// source into its destination BEFORE the agent downloads+streams it. All
// re-authorization (the shared choke point + BOTH the src-read and dst-write
// gates under the creator's live identity + the storage whitelist) lives in the
// transport-agnostic agentTaskTryRapidCore, which the reverse-channel handler
// calls identically — this handler is a thin adapter. On any miss/error it
// returns Rapid=false WITHOUT failing the task; only an authorization/live-state
// failure is an error. boundAgentID is "" because HTTP has no channel identity.
// @Summary Try a hash-based rapid-upload for a task
// @Description Anonymous, cap_token-authed. Re-verifies the whitelist and BOTH read(src)+write(dst) authorization for the creator on every call. Returns rapid=true on a hash hit (no bytes moved), rapid=false to fall back to streaming.
// @Tags agent
// @Param id path string true "task id"
// @Param body body AgentTaskTryRapidReq true "capability token"
// @Success 200 {object} common.Resp[AgentTaskTryRapidResp]
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any]
// @Router /v4/agent/task/{id}/rapid [post]
func AgentTaskTryRapid(c *gin.Context) {
	taskID := c.Param("id")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req AgentTaskTryRapidReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	rapid, ce := agentTaskTryRapidCore(c, "", taskID, req.CapToken)
	if ce != nil {
		ce.writeGin(c)
		return
	}
	common.SuccessResp(c, AgentTaskTryRapidResp{Rapid: rapid})
}

// AgentTaskPutContent streams the request body into claims.DstDir as
// stdpath.Base(claims.Src) — the proxied-write fallback for storages
// without a driver-specific direct/presigned upload path (a genuine
// direct-to-drive presigned PUT is a future, driver-specific enhancement;
// this wave always proxies the write through the server, token-authorized).
// The destination FILENAME is derived from the token's own Src field, never
// taken from client/query/header input: the token scopes a DIRECTORY
// (write on DstDir), not an arbitrary path inside it, so letting the caller
// pick this endpoint's filename would widen what the token narrows to.
//
// The capability token is accepted ONLY via the Agent-Cap-Token header — a
// prior ?cap_token= query fallback was removed (W12-3a review, minor
// finding): a query string is far more likely to be captured verbatim in
// access logs, proxy logs, and any Referer header a redirect off this
// response might carry, leaking a live (if short-lived, ≤MaxTaskCapTTL)
// scoped credential somewhere the header form never would.
// @Summary Upload a task's file content
// @Description Anonymous, cap_token-authed via the Agent-Cap-Token header (query-string form intentionally unsupported — see handler doc). Re-verifies the whitelist and write-authorization for the creator on every call. Streams straight to storage — never buffers the whole body in memory.
// @Tags agent
// @Param id path string true "task id"
// @Param Agent-Cap-Token header string true "capability token"
// @Success 200 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any]
// @Router /v4/agent/task/{id}/content [put]
func AgentTaskPutContent(c *gin.Context) {
	taskID := c.Param("id")
	capToken := c.GetHeader("Agent-Cap-Token")

	// The whole re-auth + streaming write is the shared core; the request
	// body streams straight through to op.Put (never buffered), exactly as
	// before. On any rejection the body is drained (discardBody) so a client
	// mid-upload doesn't hang, then the core's error is rendered.
	ce := agentTaskPutContentCore(c, "", taskID, capToken, c.Request.Body, c.Request.ContentLength, nil)
	if ce != nil {
		discardBody(c)
		ce.writeGin(c)
		return
	}
	common.SuccessResp(c)
}

// discardBody drains and closes the request body on a rejected upload, so a
// client that already started sending a large body doesn't leave the
// connection hanging (mirrors FsStream's discard-on-file-exists path).
func discardBody(c *gin.Context) {
	if c.Request.Body == nil {
		return
	}
	_, _ = utils.CopyWithBuffer(io.Discard, c.Request.Body)
	_ = c.Request.Body.Close()
}

// AgentTaskProgressReq is the wire body for POST /agent/task/:id/progress.
type AgentTaskProgressReq struct {
	CapToken string `json:"cap_token" binding:"required"`
	Progress int    `json:"progress"`
	Detail   string `json:"detail"`
}

// AgentTaskProgress records a progress update from the agent, and — the
// first time it is called for a task — flips claimed -> running (the agent
// having reported real progress is what "running" means; the heartbeat
// claim itself only ever sets "claimed"). A terminal (done/failed) task's
// progress is frozen: the call is accepted (still cap-verified, still
// audited) but is a no-op, returning the task's CURRENT persisted state —
// see this file's top-of-file doc on terminal-state idempotency.
// @Summary Report task progress
// @Description Anonymous, cap_token-authed.
// @Tags agent
// @Param id path string true "task id"
// @Param body body AgentTaskProgressReq true "progress update"
// @Success 200 {object} common.Resp[AgentTaskDTO]
// @Router /v4/agent/task/{id}/progress [post]
func AgentTaskProgress(c *gin.Context) {
	taskID := c.Param("id")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req AgentTaskProgressReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	dto, ce := agentTaskProgressCore(c, "", taskID, req.CapToken, req.Progress, req.Detail)
	if ce != nil {
		ce.writeGin(c)
		return
	}
	common.SuccessResp(c, dto)
}

// AgentTaskStatusReq is the wire body for POST /agent/task/:id/status.
type AgentTaskStatusReq struct {
	CapToken string `json:"cap_token" binding:"required"`
	Status   string `json:"status" binding:"required"`
	Detail   string `json:"detail"`
}

// AgentTaskStatus finalizes (or transitions to running) a task's status.
// Idempotent for terminal states: once a task is done or failed, further
// status calls are accepted (still cap-verified, still audited) but never
// change the persisted state — a retried "done" ack, or a late "failed"
// arriving after a "done" already landed, can never flip an already-settled
// outcome. See this file's top-of-file doc.
// @Summary Report task status
// @Description Anonymous, cap_token-authed. status must be one of running|done|failed.
// @Tags agent
// @Param id path string true "task id"
// @Param body body AgentTaskStatusReq true "status update"
// @Success 200 {object} common.Resp[AgentTaskDTO]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/agent/task/{id}/status [post]
func AgentTaskStatus(c *gin.Context) {
	taskID := c.Param("id")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req AgentTaskStatusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	dto, ce := agentTaskStatusCore(c, "", taskID, req.CapToken, req.Status, req.Detail)
	if ce != nil {
		ce.writeGin(c)
		return
	}
	common.SuccessResp(c, dto)
}

// AgentTaskRenewCapReq is the wire body for POST /agent/task/:id/renew.
type AgentTaskRenewCapReq struct {
	CapToken string `json:"cap_token" binding:"required"`
}

// AgentTaskRenewCapResp carries the freshly minted token plus its expiry
// (unix seconds) — informational only, see agentTaskRenewCapCore's doc.
type AgentTaskRenewCapResp struct {
	CapToken  string `json:"cap_token"`
	ExpiresAt int64  `json:"expires_at"`
}

// AgentTaskRenewCap re-mints a fresh capability token for a task the presented
// token already names (design cap-renewal): a copy that outlives one token's
// TTL (agentsrv.MaxTaskCapTTL) calls this proactively — see
// agent/task.go's wrapRenewingToken in the alist-agent module — instead of
// letting src-link/put-content/progress start 401ing partway through. All
// re-authorization (the shared choke point + BOTH the src-read and dst-write
// gates under the creator's live identity + the storage whitelist, identical
// to try-rapid's two gates) lives in the transport-agnostic
// agentTaskRenewCapCore, which the reverse-channel handler calls identically
// — this handler is a thin adapter.
// @Summary Renew a task's capability token
// @Description Anonymous, cap_token-authed. Re-verifies the whitelist and BOTH read(src)+write(dst) authorization for the creator on every call, exactly like try-rapid, then reissues a fresh token scoped to the same task.
// @Tags agent
// @Param id path string true "task id"
// @Param body body AgentTaskRenewCapReq true "capability token"
// @Success 200 {object} common.Resp[AgentTaskRenewCapResp]
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any]
// @Router /v4/agent/task/{id}/renew [post]
func AgentTaskRenewCap(c *gin.Context) {
	taskID := c.Param("id")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req AgentTaskRenewCapReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	tok, exp, ce := agentTaskRenewCapCore(c, "", taskID, req.CapToken)
	if ce != nil {
		ce.writeGin(c)
		return
	}
	common.SuccessResp(c, AgentTaskRenewCapResp{CapToken: tok, ExpiresAt: exp})
}

// --- admin task visibility (design W12-4: reliability wave) ---

// AdminListAgentTasks lists delegated agent tasks (id, agent, type, src,
// dst, status, progress, detail, attempts, timestamps) newest first,
// optionally filtered to a single agent via ?agent_id=, bounded/paged via
// ?limit=&?offset= (db.ListAgentTasks applies its own default/max — an
// out-of-range or missing value never produces an unbounded scan). Admin
// only (see server/router_v4.go). A read, not itself a trust boundary
// beyond the admin gate — no audit event, mirroring ListAgents.
// @Summary List agent tasks
// @Description Lists delegated remote-agent tasks, newest first. Optional ?agent_id= filter and ?limit=/?offset= paging. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param agent_id query string false "filter by agent id"
// @Param limit query int false "page size (default 50, max 200)"
// @Param offset query int false "page offset"
// @Success 200 {object} common.Resp[[]AgentTaskDTO]
// @Router /v4/admin/agent-tasks [get]
func AdminListAgentTasks(c *gin.Context) {
	agentID := c.Query("agent_id")
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))

	tasks, err := db.ListAgentTasks(agentID, limit, offset)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dtos := make([]AgentTaskDTO, 0, len(tasks))
	for _, task := range tasks {
		dtos = append(dtos, toAgentTaskDTO(task))
	}
	common.SuccessResp(c, dtos)
}
