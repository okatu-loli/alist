package handles

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdpath "path"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Transport-agnostic capability cores (design R3).
//
// # Why this file exists: one authorization, two transports
//
// The remote-agent cap operations (src-link, put-content, progress, status)
// are reachable over TWO transports now: the original anonymous HTTP
// endpoints (agent_task.go's gin handlers, outbound mode) AND — as of R3 —
// the mutual-authed, AEAD-encrypted reverse *channel.Channel (agent_channel.go,
// reverse mode). The R2 review's non-negotiable property is that the channel
// is TRANSPORT, NOT AUTHORIZATION: a frame arriving over a live, handshake-
// authenticated channel must be re-authorized per-op EXACTLY as the HTTP path
// is — the channel's own identity is defense-in-depth, never a substitute for
// the capability token, the storage whitelist, or op.Authorize under the
// creator's LIVE identity.
//
// The only way to GUARANTEE "identical authz on both transports" is to not
// have two authorizations at all. So every cap op's authorization + fs
// operation lives in ONE transport-agnostic core here:
//
//	resolveTaskCapCore  — the single re-auth choke point (token verify + live
//	                      task/agent reload + Enabled + creator load). Both
//	                      transports funnel through it, unchanged.
//	agentTaskSrcLinkCore
//	agentTaskPutContentCore
//	agentTaskProgressCore
//	agentTaskStatusCore
//
// The gin handlers in agent_task.go became THIN wrappers: parse the HTTP
// request, call the core, render the gin response. The channel handlers in
// agent_channel.go are the OTHER thin wrapper: decode the frame, call the
// SAME core, encode the frame. There is no forked authz path — a change to
// the whitelist check, the op.Authorize call, or the terminal-task guard
// changes BOTH transports at once because there is only one of each.
//
// Each core takes a plain context.Context (a gin.Context satisfies it, so the
// HTTP path threads its request-scoped ctx — and thus the audit IP/protocol —
// straight through; the channel path threads a channel-scoped ctx) and audits
// itself, so the audit trail is identical regardless of which transport drove
// the call.

// capOpError is a transport-neutral failure from a cap core: an HTTP-status-
// shaped code plus the underlying error, and a flag for the op.Authorize
// case (which the HTTP path must render via common.AuthzErrorResp so the
// frontend's 403/500 mapping is preserved byte-for-byte). The channel path
// maps HTTPCode to a FrameError code.
type capOpError struct {
	HTTPCode int
	Err      error
	// authz marks an op.Authorize denial: the HTTP wrapper renders it with
	// common.AuthzErrorResp(Err, authzMsg) exactly as the pre-refactor
	// handlers did (policy denial -> 403 with authzMsg, engine error -> 500),
	// rather than a plain ErrorResp.
	authz    bool
	authzMsg string
}

// writeGin renders the failure onto a gin context identically to how the
// pre-R3 handlers wrote it — this is what keeps the HTTP behavior unchanged
// under the refactor (the existing agent_task_test.go cases assert the exact
// codes below).
func (e *capOpError) writeGin(c *gin.Context) {
	if e.authz {
		common.AuthzErrorResp(c, e.Err, e.authzMsg)
		return
	}
	common.ErrorResp(c, e.Err, e.HTTPCode)
}

// resolveTaskCapCore is the transport-agnostic re-authorization choke point —
// the R3 extraction of the former gin-bound resolveTaskCap, unchanged in what
// it enforces (see agent_task.go's top-of-file doc, "capability is not a
// bypass"): verify the token, bind it to the path task id, reload the LIVE
// task row and require its agent/src/dst to still match the token's claims,
// reload the agent and require it to still exist and be Enabled, and load the
// task's creator fresh. It establishes ONLY "this token is genuinely,
// currently, for this task, issued to this still-enabled agent" — the
// per-call whitelist + op.Authorize checks are each caller's own
// responsibility, immediately before the fs op they gate.
//
// boundAgentID is R3 defense-in-depth ONLY: when non-empty (the channel path
// passes the agent id the mutual-auth handshake already proved for this
// channel), a token whose claims name a DIFFERENT agent is refused up front —
// a cap op may only travel over the channel of the very agent it was minted
// for. The HTTP path passes "" (there is no channel identity to bind to), so
// this check is a pure no-op there and HTTP behavior is unchanged. It is
// STRICTLY additive: it can only ever deny; the token, whitelist, and
// op.Authorize checks below still all run regardless. Channel identity is
// never a SUBSTITUTE for the cap token — both must hold.
//
// It audits every failure itself (identical actor/action/target to the
// pre-R3 handler) and returns a capOpError instead of writing any response.
func resolveTaskCapCore(ctx context.Context, boundAgentID, pathTaskID, capToken, auditAction string) (claims agentsrv.TaskCapClaims, task *model.AgentTask, a *model.Agent, creator *model.User, ce *capOpError) {
	claims, err := agentsrv.VerifyTaskCap(capToken)
	if err != nil || claims.TaskID == "" || claims.TaskID != pathTaskID {
		audit.Event(ctx, 0, pathTaskID, auditAction, "task:"+pathTaskID, false)
		return agentsrv.TaskCapClaims{}, nil, nil, nil, &capOpError{HTTPCode: 401, Err: errs.InvalidTaskCap}
	}

	// Defense in depth (channel only): the token must be presented over the
	// channel of the agent it names. This never REPLACES any check below — a
	// forged/stale token still fails token verification and the live-state
	// reloads all the same; this only additionally rejects a valid token
	// replayed onto the wrong agent's channel.
	if boundAgentID != "" && claims.AgentID != boundAgentID {
		audit.Event(ctx, 0, claims.AgentID, auditAction, "task:"+claims.TaskID, false)
		return agentsrv.TaskCapClaims{}, nil, nil, nil, &capOpError{HTTPCode: 401, Err: errs.InvalidTaskCap}
	}

	task, err = db.GetAgentTaskByTaskID(claims.TaskID)
	if err != nil {
		audit.Event(ctx, 0, claims.AgentID, auditAction, "task:"+claims.TaskID, false)
		code := 500
		if errors.Is(err, errs.AgentTaskNotFound) {
			code = 404
		}
		return agentsrv.TaskCapClaims{}, nil, nil, nil, &capOpError{HTTPCode: code, Err: err}
	}
	// The live task row is the authority on agent/src/dst. A token whose
	// claims no longer match it — mutated task, or a token naming a different
	// agent than the row actually belongs to — is rejected exactly like a
	// forged token: same error, no oracle distinguishing "stale" from
	// "tampered".
	if task.AgentID != claims.AgentID || task.SrcPath != claims.Src || task.DstDir != claims.DstDir {
		audit.Event(ctx, 0, claims.AgentID, auditAction, "task:"+claims.TaskID, false)
		return agentsrv.TaskCapClaims{}, nil, nil, nil, &capOpError{HTTPCode: 401, Err: errs.InvalidTaskCap}
	}

	a, err = db.GetAgentByAgentID(claims.AgentID)
	if err != nil || !a.Enabled {
		audit.Event(ctx, 0, claims.AgentID, auditAction, "task:"+claims.TaskID, false)
		if err == nil {
			err = errs.AgentDisabled
		}
		code := 403
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		return agentsrv.TaskCapClaims{}, nil, nil, nil, &capOpError{HTTPCode: code, Err: err}
	}

	creator, err = op.GetUserById(task.CreatorUID)
	if err != nil {
		audit.Event(ctx, 0, claims.AgentID, auditAction, "task:"+claims.TaskID, false)
		return agentsrv.TaskCapClaims{}, nil, nil, nil, &capOpError{HTTPCode: 500, Err: err}
	}

	return claims, task, a, creator, nil
}

// agentTaskSrcLinkCore is the transport-agnostic body of the src-link op:
// resolveTaskCapCore -> terminal guard -> whitelist(src) -> op.Authorize(read,
// creator) -> fs.Link, then build the response (a genuine direct URL, or this
// server's signed /d fallback bounded by the cap token's own remaining life).
// apiURL is the base for the proxied fallback — the HTTP path passes
// common.GetApiUrl(c.Request); the channel path passes common.GetApiUrl(nil)
// (i.e. the configured SiteURL), since a reverse dial has no inbound request
// to derive scheme/host from.
func agentTaskSrcLinkCore(ctx context.Context, boundAgentID, apiURL, taskID, capToken string) (AgentTaskSrcLinkResp, *capOpError) {
	claims, task, a, creator, ce := resolveTaskCapCore(ctx, boundAgentID, taskID, capToken, "agent.task.src_link")
	if ce != nil {
		return AgentTaskSrcLinkResp{}, ce
	}
	if task.IsTerminal() {
		audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
		return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 409, Err: errors.New("task already finished")}
	}

	if wl, err := storageWhitelisted(a, claims.Src); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
		return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 400, Err: err}
	} else if !wl {
		audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
		return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 403, Err: errs.StorageNotWhitelisted}
	}
	if err := op.Authorize(ctx, creator, model.ActRead, op.AuthzRequest{Path: claims.Src}); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
		return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 403, Err: err, authz: true, authzMsg: "permission denied"}
	}

	link, obj, err := fs.Link(ctx, claims.Src, model.LinkArgs{})
	if err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
		return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 500, Err: err}
	}

	proxied := true
	resp := AgentTaskSrcLinkResp{Size: obj.GetSize()}
	if storage, sErr := fs.GetStorage(claims.Src, &fs.GetStoragesArgs{}); sErr == nil {
		mustProxy := storage.Config().MustProxy() || storage.GetStorage().WebProxy
		if !mustProxy && link.URL != "" {
			proxied = false
			resp.URL = link.URL
			resp.Headers = link.Header
		}
	}
	if proxied {
		// FIX (root-cause: relative-URL fallback with no scheme): apiURL is
		// this server's own API base for the signed /d fallback below — the
		// HTTP transport always derives an absolute one from the incoming
		// request's Host (see common.GetApiUrl), but the reverse-channel
		// transport has no inbound request to derive scheme/host from and
		// falls back to conf.Conf.SiteURL verbatim (RegisterAgentChannelHandlers
		// passing common.GetApiUrl(nil)). When SiteURL is unset that yields "",
		// so the fallback URL below would come out as a bare "/d/..." path with
		// NO scheme — the agent's HTTP client then fails opaquely
		// ("unsupported protocol scheme \"\""). Fail closed with an actionable
		// error instead of ever handing out a schemeless link: an admin running
		// a source storage that requires this proxied fallback over a
		// reverse-mode agent MUST configure site_url so the agent can resolve
		// it. (Local storages never reach here at all — CreateAgentTask rejects
		// Local as either src or dst outright; this covers every OTHER
		// no-direct-URL/MustProxy/WebProxy driver.)
		if !strings.HasPrefix(apiURL, "http://") && !strings.HasPrefix(apiURL, "https://") {
			audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
			return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 500, Err: errors.New("source storage requires a proxied link but site_url is not configured; set it in settings so the agent can reach this server")}
		}

		// FIX (W12-3a review, "signed-/d fallback over-grant"): sign this
		// fallback with a TTL bounded by the cap token's OWN remaining life,
		// never the implicit global conf.LinkExpiration. remaining is
		// recomputed from claims.Exp so the link can never outlive the
		// SPECIFIC token that authorized it.
		remaining := time.Until(time.Unix(claims.Exp, 0))
		if remaining <= 0 {
			audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
			return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 401, Err: errs.InvalidTaskCap}
		}
		if remaining < minFallbackSignTTL {
			remaining = minFallbackSignTTL
		}
		resp.URL = fmt.Sprintf("%s/d%s?sign=%s",
			apiURL, utils.EncodePath(claims.Src, true), common.SignPathWithDuration(claims.Src, remaining))
	}
	resp.Proxied = proxied

	// Defense in depth: never hand the agent a link with no URL at all. In
	// practice this is unreachable today (the direct branch above only takes
	// resp.URL from a non-empty link.URL, and the proxied branch always
	// formats a non-empty fallback string), but it future-proofs against any
	// OTHER "no directly reachable URL" driver whose Link still slips past
	// the direct-branch's own emptiness check some other way — such a driver
	// gets a clear, actionable error here instead of a link the agent can
	// never fetch (the same class of confusing mid-task failure
	// CreateAgentTask's Local rejection exists to avoid at creation time).
	if resp.URL == "" {
		audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, false)
		return AgentTaskSrcLinkResp{}, &capOpError{HTTPCode: 500, Err: errors.New("source storage does not expose a downloadable URL for the agent")}
	}

	audit.Event(ctx, 0, a.AgentID, "agent.task.src_link", "task:"+task.TaskID, true)
	return resp, nil
}

// agentTaskPutContentCore is the transport-agnostic body of the content-write
// op: resolveTaskCapCore -> terminal guard -> whitelist(dst) -> op.Authorize
// (write, creator) -> fs.PutDirectly. It NEVER buffers the whole body — body
// streams straight through to op.Put, so an arbitrarily large copy uses
// bounded memory (the HTTP path passes the request body; the channel path
// passes the read end of an io.Pipe fed chunk-by-chunk off the wire).
//
// onAuthorized, if non-nil, is invoked exactly once immediately AFTER
// authorization has fully passed and immediately BEFORE fs.PutDirectly begins
// reading body. The channel's streaming begin uses it to learn — synchronously
// — that authorization was granted before it accepts a single byte of stream,
// so a denied write is refused up front and no chunk is ever fed to op.Put.
// The HTTP path passes nil. It is pure instrumentation: it changes nothing
// about the authorization itself, which is identical on both transports.
//
// The destination FILENAME is derived from the token's own Src (stdpath.Base),
// never from caller input — the token scopes a DIRECTORY, so letting the
// caller pick the filename would widen what the token narrows to.
func agentTaskPutContentCore(ctx context.Context, boundAgentID, taskID, capToken string, body io.Reader, size int64, onAuthorized func()) *capOpError {
	claims, task, a, creator, ce := resolveTaskCapCore(ctx, boundAgentID, taskID, capToken, "agent.task.put")
	if ce != nil {
		return ce
	}
	if task.IsTerminal() {
		audit.Event(ctx, 0, a.AgentID, "agent.task.put", "task:"+task.TaskID, false)
		return &capOpError{HTTPCode: 409, Err: errors.New("task already finished")}
	}

	if wl, err := storageWhitelisted(a, claims.DstDir); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.put", "task:"+task.TaskID, false)
		return &capOpError{HTTPCode: 400, Err: err}
	} else if !wl {
		audit.Event(ctx, 0, a.AgentID, "agent.task.put", "task:"+task.TaskID, false)
		return &capOpError{HTTPCode: 403, Err: errs.StorageNotWhitelisted}
	}
	if err := op.Authorize(ctx, creator, model.ActWrite, op.AuthzRequest{Path: claims.DstDir}); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.put", "task:"+task.TaskID, false)
		return &capOpError{HTTPCode: 403, Err: err, authz: true, authzMsg: "permission denied"}
	}

	if size < 0 {
		return &capOpError{HTTPCode: 400, Err: errors.New("Content-Length required")}
	}
	if onAuthorized != nil {
		onAuthorized()
	}
	name := stdpath.Base(claims.Src)
	s := &stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     size,
			Modified: time.Now(),
		},
		Reader:   body,
		Mimetype: utils.GetMimeType(name),
	}
	if err := fs.PutDirectly(ctx, claims.DstDir, s, true); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.put", "task:"+task.TaskID, false)
		return &capOpError{HTTPCode: 500, Err: err}
	}

	audit.Event(ctx, 0, a.AgentID, "agent.task.put", "task:"+task.TaskID, true)
	return nil
}

// agentTaskTryRapidCore is the transport-agnostic body of the rapid-upload
// ("秒传") op (design SP-H / H1): resolveTaskCapCore -> terminal guard -> BOTH
// authorization gates (whitelist+ActRead on the SOURCE, because reading the src
// hash is a src read — identical to src-link; AND whitelist+ActWrite on the
// DESTINATION dir, identical to put-content, because a hit registers a file
// there) -> resolve the source obj's already-exposed hashes -> op.PutRapid on
// the destination storage.
//
// It reuses the EXACT same choke point and gate code the other cap ops use, so
// there is no new authorization path: a disabled agent, cleared whitelist, or
// de-authorized creator denies this op just as it denies the others. The new op
// can never do anything the creator could not do themselves.
//
// The destination FILENAME is stdpath.Base(claims.Src) — never caller input
// (the wire request carries only the task id + cap token; there is no filename
// field a caller could supply), matching the put-content core.
//
// It NEVER fails the task on a rapid miss/error: an empty source hash, a
// destination driver without PutRapid (errs.NotImplement), unusable hash types
// (errs.NotSupport), or ANY provider error all return (false, nil) so the agent
// falls back to the existing download+stream path. Only an AUTHORIZATION or
// live-state failure returns a capOpError.
func agentTaskTryRapidCore(ctx context.Context, boundAgentID, taskID, capToken string) (bool, *capOpError) {
	claims, task, a, creator, ce := resolveTaskCapCore(ctx, boundAgentID, taskID, capToken, "agent.task.rapid")
	if ce != nil {
		return false, ce
	}
	if task.IsTerminal() {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 409, Err: errors.New("task already finished")}
	}

	// Gate 1: reading the source hash is a SOURCE read (identical to src-link).
	if wl, err := storageWhitelisted(a, claims.Src); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 400, Err: err}
	} else if !wl {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 403, Err: errs.StorageNotWhitelisted}
	}
	if err := op.Authorize(ctx, creator, model.ActRead, op.AuthzRequest{Path: claims.Src}); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 403, Err: err, authz: true, authzMsg: "permission denied"}
	}

	// Gate 2: a rapid hit registers a file into the DESTINATION dir (identical
	// to put-content) — its own whitelist + write authorization, independently.
	if wl, err := storageWhitelisted(a, claims.DstDir); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 400, Err: err}
	} else if !wl {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 403, Err: errs.StorageNotWhitelisted}
	}
	if err := op.Authorize(ctx, creator, model.ActWrite, op.AuthzRequest{Path: claims.DstDir}); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 403, Err: err, authz: true, authzMsg: "permission denied"}
	}

	// Resolve the source object to read the hashes it ALREADY exposes (never
	// spool/compute — that defeats the point). No usable hash => fall back.
	srcObj, err := fs.Get(ctx, claims.Src, &fs.GetArgs{})
	if err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 500, Err: err}
	}
	hashes := srcObj.GetHash()
	if len(hashes.Export()) == 0 {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, true)
		return false, nil
	}

	// Resolve the destination storage + its actual path for op.PutRapid.
	dstStorage, dstActualPath, err := op.GetStorageAndActualPath(claims.DstDir)
	if err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, false)
		return false, &capOpError{HTTPCode: 500, Err: err}
	}

	// FILENAME from the token's Src, never caller input (matches put-content).
	name := stdpath.Base(claims.Src)
	hit, err := op.PutRapid(ctx, dstStorage, dstActualPath, name, srcObj.GetSize(), hashes)
	if err != nil {
		// NotImplement (dst driver has no PutRapid) / NotSupport (hashes
		// unusable) / any provider error => fall back to streaming, DO NOT fail
		// the task.
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, true)
		return false, nil
	}
	if !hit {
		audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, true)
		return false, nil
	}
	audit.Event(ctx, 0, a.AgentID, "agent.task.rapid", "task:"+task.TaskID, true)
	return true, nil
}

// agentTaskProgressCore is the transport-agnostic body of the progress op:
// resolveTaskCapCore -> (terminal task: frozen no-op, returns current state)
// -> clamp -> claimed->running transition -> persist.
func agentTaskProgressCore(ctx context.Context, boundAgentID, taskID, capToken string, progress int, detail string) (AgentTaskDTO, *capOpError) {
	_, task, a, _, ce := resolveTaskCapCore(ctx, boundAgentID, taskID, capToken, "agent.task.progress")
	if ce != nil {
		return AgentTaskDTO{}, ce
	}
	if task.IsTerminal() {
		audit.Event(ctx, 0, a.AgentID, "agent.task.progress", "task:"+task.TaskID, true)
		return toAgentTaskDTO(*task), nil
	}

	if progress < 0 {
		progress = 0
	} else if progress > 100 {
		progress = 100
	}
	task.Progress = progress
	task.Detail = detail
	if task.Status == model.AgentTaskClaimed {
		task.Status = model.AgentTaskRunning
	}
	if err := db.UpdateAgentTask(task); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.progress", "task:"+task.TaskID, false)
		return AgentTaskDTO{}, &capOpError{HTTPCode: 500, Err: err}
	}
	audit.Event(ctx, 0, a.AgentID, "agent.task.progress", "task:"+task.TaskID, true)
	return toAgentTaskDTO(*task), nil
}

// agentTaskStatusCore is the transport-agnostic body of the status op: the
// status-value validation, resolveTaskCapCore, terminal-idempotency, and the
// status/progress persist. Terminal tasks are frozen: a retried "done" or a
// late "failed" arriving after a "done" already landed can never flip an
// already-settled outcome.
func agentTaskStatusCore(ctx context.Context, boundAgentID, taskID, capToken, status, detail string) (AgentTaskDTO, *capOpError) {
	if status != model.AgentTaskRunning && status != model.AgentTaskDone && status != model.AgentTaskFailed {
		return AgentTaskDTO{}, &capOpError{HTTPCode: 400, Err: errors.New("status must be one of running|done|failed")}
	}

	_, task, a, _, ce := resolveTaskCapCore(ctx, boundAgentID, taskID, capToken, "agent.task.status")
	if ce != nil {
		return AgentTaskDTO{}, ce
	}
	if task.IsTerminal() {
		audit.Event(ctx, 0, a.AgentID, "agent.task.status", "task:"+task.TaskID, true)
		return toAgentTaskDTO(*task), nil
	}

	task.Status = status
	task.Detail = detail
	if status == model.AgentTaskDone {
		task.Progress = 100
	}
	if err := db.UpdateAgentTask(task); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.status", "task:"+task.TaskID, false)
		return AgentTaskDTO{}, &capOpError{HTTPCode: 500, Err: err}
	}
	audit.Event(ctx, 0, a.AgentID, "agent.task.status", "task:"+task.TaskID, true)
	return toAgentTaskDTO(*task), nil
}

// maxAgentTaskLifetime bounds how long a single agent task may keep renewing
// its capability token, counted from db.AgentTask.CreatedAt (review fix,
// confirmed: "续期链无总寿命上限"). Without this, a task that never reaches a
// terminal state — stuck mid-copy, or an agent deliberately holding one open
// — could renew forever as long as the agent itself stays enabled and the
// creator stays authorized; the only backstop would have been agent-liveness
// detection, which is a DIFFERENT axis (agent reachable vs. task legitimately
// still running) and must not be the sole gate on capability lifetime.
//
// 24h is a deliberately generous, but FINITE, ceiling: it comfortably covers
// even a very large cross-drive copy over a slow link, while still bounding
// worst case exposure to a single day rather than indefinitely. A var (not a
// const) purely so a test can inject a task whose CreatedAt is already past
// it without needing to actually wait — production code never mutates this.
//
// This gate applies ONLY to renewal (agentTaskRenewCapCore), never to the
// initial dispatch/heartbeat token issuance or any OTHER cap op
// (src-link/put-content/progress/status all keep working on whatever token
// they were already holding): a task past this age simply stops being able
// to extend its capability's life beyond whatever the CURRENT token's own
// remaining ≤MaxTaskCapTTL window still gives it — the exact same "eventually
// 401s" shape an agent already faces today once its last-issued token
// expires, just with an actionable error instead of a bare 401 at the moment
// renewal itself is attempted.
var maxAgentTaskLifetime = 24 * time.Hour

// agentTaskRenewCapCore is the transport-agnostic body of the cap-renewal op
// (design cap-renewal): resolveTaskCapCore -> terminal guard -> BOTH
// authorization gates (whitelist+ActRead on the SOURCE, whitelist+ActWrite on
// the DESTINATION — the exact same two gates agentTaskTryRapidCore runs,
// because a renewed token grants exactly the same read+write authority the
// original one did, no more) -> mint a fresh token scoped to the SAME (agent,
// task, src, dst) tuple for a fresh agentsrv.MaxTaskCapTTL.
//
// Renewal is deliberately NOT "extend the presented token's own life" — every
// live-state and authorization check re-runs from scratch, exactly as every
// other cap op's does (see this file's top-of-file doc and
// server/handles/agent_task.go's "capability is not a bypass"): a task that
// finished, an agent that was disabled, a whitelist that was narrowed, or a
// creator who lost authorization all deny the very next renewal attempt just
// as they deny src-link/put-content, even though the OLD token presented to
// ask for renewal still itself verifies (VerifyTaskCap ran inside
// resolveTaskCapCore above and passed — that is what got this call this far
// at all, never what decides whether it succeeds).
//
// It reuses the EXACT same choke point and gate code the other cap ops use —
// there is no new authorization path here, and no way for a renewed token to
// authorize anything the presented one did not already.
func agentTaskRenewCapCore(ctx context.Context, boundAgentID, taskID, capToken string) (newToken string, expiresAt int64, ce *capOpError) {
	claims, task, a, creator, rce := resolveTaskCapCore(ctx, boundAgentID, taskID, capToken, "agent.task.renew_cap")
	if rce != nil {
		return "", 0, rce
	}
	if task.IsTerminal() {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 409, Err: errors.New("task already finished")}
	}
	if time.Since(task.CreatedAt) > maxAgentTaskLifetime {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 403, Err: fmt.Errorf("task exceeds the maximum agent task lifetime (%s); recreate the task to continue", maxAgentTaskLifetime)}
	}

	// Gate 1: the renewed token still authorizes reading the source.
	if wl, err := storageWhitelisted(a, claims.Src); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 400, Err: err}
	} else if !wl {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 403, Err: errs.StorageNotWhitelisted}
	}
	if err := op.Authorize(ctx, creator, model.ActRead, op.AuthzRequest{Path: claims.Src}); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 403, Err: err, authz: true, authzMsg: "permission denied"}
	}

	// Gate 2: the renewed token still authorizes writing the destination.
	if wl, err := storageWhitelisted(a, claims.DstDir); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 400, Err: err}
	} else if !wl {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 403, Err: errs.StorageNotWhitelisted}
	}
	if err := op.Authorize(ctx, creator, model.ActWrite, op.AuthzRequest{Path: claims.DstDir}); err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 403, Err: err, authz: true, authzMsg: "permission denied"}
	}

	tok, err := agentsrv.IssueTaskCap(a.AgentID, task.TaskID, task.SrcPath, task.DstDir, agentsrv.MaxTaskCapTTL)
	if err != nil {
		audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, false)
		return "", 0, &capOpError{HTTPCode: 500, Err: err}
	}
	audit.Event(ctx, 0, a.AgentID, "agent.task.renew_cap", "task:"+task.TaskID, true)
	return tok, time.Now().Add(agentsrv.MaxTaskCapTTL).Unix(), nil
}
