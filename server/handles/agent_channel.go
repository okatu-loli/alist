package handles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AlistGo/alist-agent/proto"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/AlistGo/alist-agent/channel"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	log "github.com/sirupsen/logrus"
)

// Reverse-channel cap-op handlers (design R3): the server-side half that
// receives the capability operations an agent sends over a live, mutual-authed,
// AEAD-encrypted reverse *channel.Channel and funnels EACH into the SAME shared
// authorization core the anonymous HTTP endpoints use
// (server/handles/agent_task_core.go). This is the crux of the R2 review's
// non-negotiable property — "the channel is TRANSPORT, not AUTHORIZATION":
//
//   - Every agent->server cap frame carries its own capability token, which is
//     re-verified per op through resolveTaskCapCore exactly as over HTTP: the
//     token is reloaded, bound to the task, the LIVE task/agent rows are
//     reloaded (Enabled re-checked), and op.Authorize runs under the task
//     CREATOR's live identity. A disabled agent, a cleared whitelist, or a
//     de-authorized creator denies the very next cap op over the channel — the
//     channel's standing handshake trust grants NOTHING on its own.
//   - The channel's handshake-proven agent identity is layered on as pure
//     defense in depth (boundAgentID): a cap token may only be redeemed over
//     the channel of the agent it names. This never substitutes for the token.
//   - Streaming writes go through the identical op.Put core: the reassembled
//     chunk stream is fed to agentTaskPutContentCore via an io.Pipe, so the
//     write is authorized and executed by the exact same code the HTTP PUT runs.
//
// Task DISPATCH is inverted vs HTTP: instead of the agent polling a heartbeat
// that returns a task, the server PUSHES task.dispatch when it has a claimed
// task for this channel's agent (DispatchAgentTaskOverChannel), using the same
// db.ClaimPendingForAgent (which filters by agent_id, so a task can only ever
// reach the agent it was created for) + agentsrv.IssueTaskCap the heartbeat
// path uses.

// maxAgentStreamBytes is the hard ceiling on the TOTAL bytes one channel
// put-content stream may carry, independent of the per-request declared size —
// the reverse-channel analogue of internal/webdav's maxWebdavPutBytes. A var
// (not a const) so a test can lower it to prove the cap engages without moving
// 100 GiB. Enforced chunk-by-chunk as bytes arrive, before they are handed to
// op.Put, so an over-cap stream is torn down early rather than written.
var maxAgentStreamBytes int64 = 100 << 30 // 100 GiB

// putStream is one in-flight channel put-content upload's server-side state.
// The bytes never accumulate here: each chunk is written straight into pw,
// whose read end (pr) op.Put drains inside the core goroutine — so pw.Write
// blocks until op.Put has consumed the chunk, giving natural backpressure and a
// transient footprint of at most one chunk.
type putStream struct {
	pw     *io.PipeWriter
	cancel context.CancelFunc // cancels the core goroutine's ctx (op.Put)
	size   int64              // the agent-declared total; received bytes may not exceed it

	authzOK chan struct{} // closed by the core's onAuthorized hook once authz passed

	resultCh chan struct{} // closed once result is set
	result   *capOpError   // the core's outcome (read only after resultCh closed)

	mu      sync.Mutex
	written int64
	nextSeq uint64
}

func (s *putStream) setResult(ce *capOpError) {
	s.result = ce
	close(s.resultCh)
}

// waitResult blocks for the core's outcome, or ctx cancellation.
func (s *putStream) waitResult(ctx context.Context) *capOpError {
	select {
	case <-s.resultCh:
		return s.result
	case <-ctx.Done():
		return &capOpError{HTTPCode: 500, Err: ctx.Err()}
	}
}

// redispatchInterval is the minimum time a bound handler waits before
// re-pushing task.dispatch for a task it has already successfully pushed once.
// The 3s supervisor tick (reverseDispatchInterval) used to re-push an ACTIVE
// task unconditionally on every tick, so a long-running copy got hit with a
// fresh task.dispatch (and a fresh audit row) every 3 seconds while the agent
// was still working it — pure noise the agent logs as "already running;
// ignoring". A bound handler now dispatches a given taskID at most once per
// redispatchInterval, which still preserves both compensating cases the
// unconditional resend covered: a channel that just came back up re-dispatches
// immediately (a NEW handler starts with an empty dispatched map — see
// RegisterAgentChannelHandlers), and a push that reached the agent but was
// silently dropped/ignored gets one retry after the interval elapses rather
// than never. 60s comfortably exceeds any plausible single-chunk stall while
// staying well under operator-noticeable "is this stuck" timescales.
const redispatchInterval = 60 * time.Second

// agentChannelHandler holds the per-channel handler state (which agent this
// channel belongs to, the API base for src-link fallbacks, the live
// put-content streams, and the redispatch bookkeeping) so the registered
// closures can find it.
type agentChannelHandler struct {
	ch      *channel.Channel
	agentID string
	apiURL  string

	// redispatchInterval overrides the package-level redispatchInterval when
	// non-zero; a test hook so timing-sensitive tests don't need to wait 60s.
	redispatchInterval time.Duration

	// mu guards both streams and dispatched.
	mu      sync.Mutex
	streams map[string]*putStream

	// dispatched records, per taskID, the last time DispatchPending
	// successfully pushed task.dispatch for it over this channel. It is
	// consulted (and pruned on terminal status — see (h *agentChannelHandler)
	// status) to throttle re-pushes of a still-active task; see
	// redispatchInterval's doc for why this exists.
	dispatched map[string]time.Time
}

// RegisterAgentChannelHandlers wires every reverse-channel cap handler onto a
// live channel, bound to the agent the handshake authenticated. Call it once,
// after the channel goes live, for a reverse-mode agent. It also arranges for
// every in-flight put-content stream to be aborted if the channel tears down,
// so a dropped channel can never leave an op.Put goroutine blocked on a pipe
// that will never receive its next chunk.
//
// It RETURNS the bound handler so the reverse supervisor pushes tasks through
// h.DispatchPending — a method closed over THIS channel and THIS handshake-
// proven agent id — rather than the free DispatchAgentTaskOverChannel with a
// caller-supplied id. That is the R3-review's security contract made
// structural (design R4): dispatch can only ever target the exact channel and
// agent identity the handshake authenticated for this handler, so a task for
// agent B can never be pushed over agent A's channel because there is no seam
// to hand DispatchPending a different id. Callers with no need to dispatch may
// discard the return value.
func RegisterAgentChannelHandlers(ch *channel.Channel, agentID string) *agentChannelHandler {
	h := &agentChannelHandler{
		ch:         ch,
		agentID:    agentID,
		apiURL:     common.GetApiUrl(nil),
		streams:    make(map[string]*putStream),
		dispatched: make(map[string]time.Time),
	}
	ch.Handle(proto.MethodHeartbeat, h.heartbeat)
	ch.Handle(proto.MethodTaskSrcLink, h.srcLink)
	ch.Handle(proto.MethodTaskTryRapid, h.tryRapid)
	ch.Handle(proto.MethodTaskProgress, h.progress)
	ch.Handle(proto.MethodTaskStatus, h.status)
	ch.Handle(proto.MethodTaskPutBegin, h.putBegin)
	ch.Handle(proto.MethodTaskPutChunk, h.putChunk)
	ch.Handle(proto.MethodTaskPutEnd, h.putEnd)
	ch.Handle(proto.MethodTaskRenewCap, h.renewCap)

	go func() {
		<-ch.Done()
		h.abortAllStreams(errors.New("agent channel: torn down"))
	}()
	return h
}

// AgentID returns the handshake-proven agent identity this handler is bound to.
func (h *agentChannelHandler) AgentID() string { return h.agentID }

// DispatchPending pushes any pending/active task for THIS handler's bound agent
// over THIS handler's bound channel. It sources both the channel and the agent
// id from the handler's own fields — the exact pair the handshake
// authenticated — so it is structurally impossible to dispatch a task over the
// wrong agent's channel (design R4 security contract; see
// RegisterAgentChannelHandlers). This is the ONLY dispatch path the reverse
// supervisor uses.
//
// Unlike the free DispatchAgentTaskOverChannel (which always pushes), this
// throttles: a taskID already pushed within redispatchInterval is skipped
// (returns false, nil) rather than re-pushed. See redispatchInterval's doc for
// why — this is what stops a long-running task from being re-dispatched, and
// re-audited, every 3s supervisor tick while it is still active.
func (h *agentChannelHandler) DispatchPending(ctx context.Context) (bool, error) {
	task, err := agentDispatchFindTask(h.agentID)
	if err != nil {
		return false, err
	}
	if task == nil {
		return false, nil
	}

	interval := h.redispatchInterval
	if interval <= 0 {
		interval = redispatchInterval
	}
	h.mu.Lock()
	if last, ok := h.dispatched[task.TaskID]; ok && time.Since(last) < interval {
		h.mu.Unlock()
		return false, nil
	}
	h.mu.Unlock()

	if err := agentDispatchPush(ctx, h.ch, h.agentID, task); err != nil {
		return false, err
	}

	h.mu.Lock()
	h.dispatched[task.TaskID] = time.Now()
	h.mu.Unlock()
	return true, nil
}

// frameError renders a capOpError as a structured channel FrameError, carrying
// the HTTP-status-shaped code so the agent side can tell classes of failure
// apart if it needs to.
func frameError(ce *capOpError) *channel.FrameError {
	return &channel.FrameError{Code: strconv.Itoa(ce.HTTPCode), Message: ce.Err.Error()}
}

// heartbeat refreshes this channel's agent LastSeen and re-checks Enabled LIVE.
// The agent's identity is already established by the channel handshake, so there
// is no signature to verify here (that is the HTTP path's job, where there is no
// prior mutual auth) — but Enabled is still re-loaded every beat, so disabling
// an agent stops its reverse liveness exactly as it stops its HTTP heartbeat.
func (h *agentChannelHandler) heartbeat(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	a, err := db.GetAgentByAgentID(h.agentID)
	if err != nil {
		audit.Event(ctx, 0, h.agentID, "agent.heartbeat", "agent:"+h.agentID, false)
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		return nil, frameError(&capOpError{HTTPCode: code, Err: err})
	}
	if !a.Enabled {
		audit.Event(ctx, 0, a.AgentID, "agent.heartbeat", "agent:"+a.AgentID, false)
		return nil, frameError(&capOpError{HTTPCode: 403, Err: errs.AgentDisabled})
	}
	now := time.Now()
	a.LastSeen = &now
	if err := db.UpdateAgent(a); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 500, Err: err})
	}
	audit.Event(ctx, 0, a.AgentID, "agent.heartbeat", "agent:"+a.AgentID, true)
	return json.Marshal(proto.ChannelHeartbeatResp{OK: true, Enabled: true})
}

func (h *agentChannelHandler) srcLink(ctx context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelSrcLinkReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	resp, ce := agentTaskSrcLinkCore(ctx, h.agentID, h.apiURL, req.TaskID, req.CapToken)
	if ce != nil {
		return nil, frameError(ce)
	}
	return json.Marshal(proto.ChannelSrcLinkResp{
		URL:     resp.URL,
		Headers: resp.Headers,
		Size:    resp.Size,
		Proxied: resp.Proxied,
	})
}

// tryRapid decodes a rapid-upload request off the channel and funnels it into
// the SAME agentTaskTryRapidCore the HTTP handler uses, binding this channel's
// handshake-proven agent id as boundAgentID (defense in depth) — the channel is
// transport, not authorization. A miss/error surfaces as Rapid=false (never a
// frame error), exactly like the HTTP 200 rapid=false; only an authz/live-state
// failure becomes a frame error.
func (h *agentChannelHandler) tryRapid(ctx context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelTryRapidReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	rapid, ce := agentTaskTryRapidCore(ctx, h.agentID, req.TaskID, req.CapToken)
	if ce != nil {
		return nil, frameError(ce)
	}
	return json.Marshal(proto.ChannelTryRapidResp{Rapid: rapid})
}

func (h *agentChannelHandler) progress(ctx context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelProgressReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	dto, ce := agentTaskProgressCore(ctx, h.agentID, req.TaskID, req.CapToken, req.Progress, req.Detail)
	if ce != nil {
		return nil, frameError(ce)
	}
	return json.Marshal(dto)
}

func (h *agentChannelHandler) status(ctx context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelStatusReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	dto, ce := agentTaskStatusCore(ctx, h.agentID, req.TaskID, req.CapToken, req.Status, req.Detail)
	if ce != nil {
		return nil, frameError(ce)
	}
	if dto.Status == model.AgentTaskDone || dto.Status == model.AgentTaskFailed {
		// The task reached a terminal state: it can never be dispatched again
		// (db.GetActiveAgentTaskForAgent excludes done/failed), so its
		// redispatch bookkeeping would otherwise sit in the map forever.
		h.mu.Lock()
		delete(h.dispatched, req.TaskID)
		h.mu.Unlock()
	}
	return json.Marshal(dto)
}

// renewCap is the channel form of the cap-renewal op (design cap-renewal): it
// funnels into the SAME agentTaskRenewCapCore the HTTP endpoint uses, so a
// reverse-mode copy's proactive renewal gets exactly the same re-authorization
// chain — nothing here trusts the channel's standing handshake for anything
// beyond boundAgentID's usual defense-in-depth check inside resolveTaskCapCore.
func (h *agentChannelHandler) renewCap(ctx context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelRenewCapReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	tok, exp, ce := agentTaskRenewCapCore(ctx, h.agentID, req.TaskID, req.CapToken)
	if ce != nil {
		return nil, frameError(ce)
	}
	return json.Marshal(proto.ChannelRenewCapResp{CapToken: tok, ExpiresAt: exp})
}

// putBegin opens a streaming content upload. It launches agentTaskPutContentCore
// — the SAME core the HTTP PUT runs — in a goroutine reading the pipe, and
// BLOCKS until that core signals (via onAuthorized) that authorization fully
// passed, or returns early with a denial. So an unauthorized write is refused
// synchronously at begin and NEVER accepts a chunk; only an authorized stream
// proceeds to the chunk phase. The core's ctx is derived from Background (the
// upload outlives this begin request's own ctx) and is cancelled on end or
// channel teardown.
func (h *agentChannelHandler) putBegin(ctx context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelPutBeginReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	if req.StreamID == "" {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: errors.New("stream_id required")})
	}
	if req.Size < 0 {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: errors.New("size required")})
	}

	pr, pw := io.Pipe()
	streamCtx, cancel := context.WithCancel(context.Background())
	st := &putStream{
		pw:       pw,
		cancel:   cancel,
		size:     req.Size,
		authzOK:  make(chan struct{}),
		resultCh: make(chan struct{}),
	}

	h.mu.Lock()
	if _, exists := h.streams[req.StreamID]; exists {
		h.mu.Unlock()
		cancel()
		_ = pw.Close()
		return nil, frameError(&capOpError{HTTPCode: 400, Err: errors.New("stream id already in use")})
	}
	h.streams[req.StreamID] = st
	h.mu.Unlock()

	go func() {
		onAuthorized := func() { close(st.authzOK) }
		ce := agentTaskPutContentCore(streamCtx, h.agentID, req.TaskID, req.CapToken, pr, req.Size, onAuthorized)
		st.setResult(ce)
		// Unblock any chunk write still waiting on op.Put (which has now
		// returned — success EOF, authz denial before any read, or a write
		// error): a pending/next pw.Write returns this instead of hanging.
		_ = pr.CloseWithError(errors.New("agent channel: put stream ended"))
	}()

	select {
	case <-st.authzOK:
		// Authorized; the chunk phase may begin.
		return json.Marshal(proto.ChannelAck{OK: true})
	case <-st.resultCh:
		// The core returned before authorizing (denied / invalid cap / size) —
		// no byte was ever read. Surface the denial and drop the stream.
		h.removeStream(req.StreamID)
		cancel()
		ce := st.result
		if ce == nil {
			ce = &capOpError{HTTPCode: 500, Err: errors.New("put stream ended before authorization")}
		}
		return nil, frameError(ce)
	case <-ctx.Done():
		h.removeStream(req.StreamID)
		cancel()
		_ = pw.CloseWithError(ctx.Err())
		return nil, frameError(&capOpError{HTTPCode: 500, Err: ctx.Err()})
	}
}

// putChunk feeds one ordered chunk into the stream's pipe. It enforces, BEFORE
// writing a byte: the stream exists, the chunk is the next expected sequence
// (defense in depth atop the channel's own in-order AEAD delivery), the chunk
// is within MaxChannelChunkBytes, and the running total stays within BOTH the
// agent-declared size and the hard maxAgentStreamBytes cap. Any violation
// aborts the stream (tearing down op.Put) and returns an error. The pw.Write
// then blocks until op.Put consumes the chunk — backpressure, bounded memory.
func (h *agentChannelHandler) putChunk(_ context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelPutChunkReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	st := h.getStream(req.StreamID)
	if st == nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: errors.New("unknown or closed stream")})
	}

	n := int64(len(req.Data))
	st.mu.Lock()
	if req.Seq != st.nextSeq {
		st.mu.Unlock()
		h.abortStream(req.StreamID, st, fmt.Errorf("out-of-order chunk: got seq %d want %d", req.Seq, st.nextSeq))
		return nil, frameError(&capOpError{HTTPCode: 400, Err: errors.New("out-of-order chunk")})
	}
	if n > proto.MaxChannelChunkBytes {
		st.mu.Unlock()
		h.abortStream(req.StreamID, st, errors.New("chunk exceeds max chunk size"))
		return nil, frameError(&capOpError{HTTPCode: 413, Err: errors.New("chunk too large")})
	}
	if st.written+n > st.size || st.written+n > maxAgentStreamBytes {
		st.mu.Unlock()
		h.abortStream(req.StreamID, st, errors.New("stream exceeds byte cap"))
		return nil, frameError(&capOpError{HTTPCode: 413, Err: errors.New("stream exceeds byte cap")})
	}
	st.nextSeq++
	st.written += n
	st.mu.Unlock()

	// Blocks until op.Put drains it. If op.Put already returned (authz denial
	// discovered post-begin, or a dst write error), the reader was closed with
	// an error and this returns it — surface the core's real outcome.
	if _, err := st.pw.Write(req.Data); err != nil {
		ce := st.waitResultNonBlocking()
		if ce == nil {
			ce = &capOpError{HTTPCode: 500, Err: err}
		}
		h.removeStream(req.StreamID)
		st.cancel()
		return nil, frameError(ce)
	}
	return json.Marshal(proto.ChannelAck{OK: true})
}

// putEnd flushes EOF to op.Put and returns the final outcome — this is where a
// write's ultimate success/failure (including a dst-side op.Put error) is
// reported to the agent.
func (h *agentChannelHandler) putEnd(ctx context.Context, _ string, payload json.RawMessage) (json.RawMessage, error) {
	var req proto.ChannelPutEndReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: err})
	}
	st := h.getStream(req.StreamID)
	if st == nil {
		return nil, frameError(&capOpError{HTTPCode: 400, Err: errors.New("unknown or closed stream")})
	}
	h.removeStream(req.StreamID)

	// EOF to op.Put: it finishes reading and the core returns.
	_ = st.pw.Close()
	ce := st.waitResult(ctx)
	st.cancel()
	if ce != nil {
		return nil, frameError(ce)
	}
	return json.Marshal(proto.ChannelAck{OK: true})
}

// waitResultNonBlocking returns the core result if already set, else nil.
func (s *putStream) waitResultNonBlocking() *capOpError {
	select {
	case <-s.resultCh:
		return s.result
	default:
		return nil
	}
}

func (h *agentChannelHandler) getStream(id string) *putStream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[id]
}

func (h *agentChannelHandler) removeStream(id string) {
	h.mu.Lock()
	delete(h.streams, id)
	h.mu.Unlock()
}

// abortStream tears down one stream: drop it, close the pipe with the cause so
// op.Put unwinds with an error (no partial/garbage write is treated as
// success), and cancel the core ctx.
func (h *agentChannelHandler) abortStream(id string, st *putStream, cause error) {
	h.removeStream(id)
	_ = st.pw.CloseWithError(cause)
	st.cancel()
}

// abortAllStreams tears down every live stream — called on channel teardown so
// no op.Put goroutine is left blocked on a pipe that will never be fed again.
func (h *agentChannelHandler) abortAllStreams(cause error) {
	h.mu.Lock()
	streams := make([]*putStream, 0, len(h.streams))
	for _, st := range h.streams {
		streams = append(streams, st)
	}
	h.streams = make(map[string]*putStream)
	h.mu.Unlock()
	for _, st := range streams {
		_ = st.pw.CloseWithError(cause)
		st.cancel()
	}
}

// DispatchAgentTaskOverChannel is the reverse-mode task-dispatch step: claim a
// pending task for this channel's agent (or re-surface its already-active one),
// mint a fresh capability token, and PUSH it as task.dispatch. It reuses the
// EXACT claim + issue path the HTTP heartbeat's dispatchAgentTask uses, so a
// task can only ever be dispatched to the agent it was created for
// (db.ClaimPendingForAgent filters by agent_id) and the token is scoped +
// bounded identically. Returns whether a task was dispatched. Fails closed: if
// the token can't be minted, the task stays claimed/pending for a later retry
// rather than being pushed with no way to act on it.
//
// SECURITY (design R4): agentID here MUST be the handshake-proven identity of
// the live channel — never a config value or a caller-guessed id — or a task
// for agent B could be pushed over agent A's channel. Production callers do NOT
// call this directly; they go through agentChannelHandler.DispatchPending,
// which sources both ch and agentID from the same authenticated handler so a
// mismatched pair is structurally impossible. This function stays exported only
// for the shared-core reverse e2e test.
func DispatchAgentTaskOverChannel(ctx context.Context, ch *channel.Channel, agentID string) (bool, error) {
	task, err := agentDispatchFindTask(agentID)
	if err != nil {
		return false, err
	}
	if task == nil {
		return false, nil
	}
	if err := agentDispatchPush(ctx, ch, agentID, task); err != nil {
		return false, err
	}
	return true, nil
}

// agentDispatchFindTask is the claim-or-resurface step DispatchAgentTaskOverChannel
// and agentChannelHandler.DispatchPending share: claim a fresh pending task for
// agentID, or, if none, re-surface its already-active (claimed/running) task so
// a caller can decide whether to (re-)push it. Calling it repeatedly is safe —
// once a task is claimed, later calls just find it via
// db.GetActiveAgentTaskForAgent without re-claiming anything.
func agentDispatchFindTask(agentID string) (*model.AgentTask, error) {
	task, err := db.ClaimPendingForAgent(agentID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		task, err = db.GetActiveAgentTaskForAgent(agentID)
		if err != nil {
			return nil, err
		}
	}
	return task, nil
}

// agentDispatchPush mints a fresh capability token for task and pushes it as
// task.dispatch over ch, auditing the outcome either way.
func agentDispatchPush(ctx context.Context, ch *channel.Channel, agentID string, task *model.AgentTask) error {
	capToken, err := agentsrv.IssueTaskCap(agentID, task.TaskID, task.SrcPath, task.DstDir, agentsrv.MaxTaskCapTTL)
	if err != nil {
		audit.Event(ctx, 0, agentID, "agent.task.dispatch", "task:"+task.TaskID, false)
		log.Errorf("agent channel dispatch: issue task cap for task %s: %+v", task.TaskID, err)
		return err
	}

	payload, err := json.Marshal(proto.ChannelDispatch{
		TaskID:   task.TaskID,
		Type:     task.Type,
		SrcPath:  task.SrcPath,
		DstDir:   task.DstDir,
		CapToken: capToken,
	})
	if err != nil {
		return err
	}
	if err := ch.Push(proto.MethodTaskDispatch, payload); err != nil {
		audit.Event(ctx, 0, agentID, "agent.task.dispatch", "task:"+task.TaskID, false)
		return err
	}
	audit.Event(ctx, 0, agentID, "agent.task.dispatch", "task:"+task.TaskID, true)
	return nil
}
