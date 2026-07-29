package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AlistGo/alist-agent/proto"
)

// This file holds the agent-side execution of a delegated cross-drive copy
// task (design W12-3b): once a heartbeat (see client.go's Heartbeat /
// HeartbeatResponse.Task) hands back a task and a freshly minted capability
// token, RunCopyTask drives the whole thing — fetch the source's direct
// download link, GET it, stream the bytes up to this server's proxied
// content endpoint, and report progress/status along the way — entirely
// through the same anonymous, cap_token-authed endpoints
// server/handles/agent_task.go exposes. See that file's top-of-file doc
// ("capability is not a bypass"): nothing here is a trust boundary itself —
// every call this file makes is independently re-authorized server-side.
//
// cmd/agent.go's `agent run` loop is a thin wrapper: on a heartbeat that
// carries a task, it calls RunCopyTask and logs the outcome, then resumes
// heartbeating. All the actual protocol logic lives here so it is
// unit-testable without spawning that CLI at all (see task_test.go).

// HeartbeatTask is the task-dispatch payload embedded in a heartbeat
// response — the agent-side mirror of
// server/handles.AgentHeartbeatTaskDTO, kept as an independent
// wire-compatible type (the same convention client.go's
// PairCompleteResponse/HeartbeatResponse already use) so this package never
// needs to import server/handles.
type HeartbeatTask struct {
	TaskID   string `json:"task_id"`
	Type     string `json:"type"`
	SrcPath  string `json:"src_path"`
	DstDir   string `json:"dst_dir"`
	CapToken string `json:"cap_token"`
}

// Task is what RunCopyTask needs to identify and execute one copy —
// HeartbeatTask minus the capability token, which RunCopyTask takes as its
// own explicit parameter: a token is a bearer secret, threaded deliberately
// through call sites rather than tucked inside a struct value that might
// get logged or passed around alongside the rest of a Task.
type Task struct {
	TaskID  string
	Type    string
	SrcPath string
	DstDir  string
}

// TokenFunc supplies the freshest capability token for a task's NEXT server
// call (design W12-4 (A): token-renewal threading). RunCopyTask calls it
// immediately before each HTTP call it makes — report-running, src-link,
// each progress report, the content upload, report-done/failed — instead of
// closing over one capToken value captured at the start of the run. This is
// what lets a copy outlive a single capability token's TTL
// (agentpkg.MaxTaskCapTTL, minted fresh on every heartbeat while a task is
// active — see server/handles.dispatchAgentTask): as long as the caller
// keeps a TokenFunc's backing value current (see TaskToken and
// runner.go's Runner, which does so from the heartbeat loop), each of
// RunCopyTask's calls reads whatever is current AT THAT MOMENT. A single
// already-in-flight streaming request (the content PUT, once started) is
// unaffected by a renewal that lands mid-upload — only the SEQUENCE of
// calls picks one up, which is exactly what a capability token scopes
// (each call is independently re-authorized server-side; see
// server/handles/agent_task.go's top-of-file doc).
type TokenFunc func() string

// StaticToken adapts a single fixed token into a TokenFunc, for callers
// that genuinely have no renewal to offer (tests, or a short copy expected
// to finish well inside one token's TTL).
func StaticToken(tok string) TokenFunc {
	return func() string { return tok }
}

// Task status strings this package's RunCopyTask reports now live as
// proto.TaskStatusRunning/Done/Failed (internal/agent/proto/wire.go, moved
// there in the W3 proto-split — design analysis §6 W3 — since the server
// independently validates the exact same strings, see
// server/handles.AgentTaskStatus). proto.RapidUploadDetail is the done-status
// detail the agent reports when a copy task completed via hash-based
// rapid-upload ("秒传") instead of streaming bytes (design SP-H), for the
// same reason: it is a marker both the agent and the server's observability
// endpoint must agree on byte for byte.

// SrcLink is the agent-side mirror of server/handles.AgentTaskSrcLinkResp:
// where and how to download a task's source content. URL may be a genuine
// direct-from-drive URL (Proxied=false) or this server's own signed /d
// fallback (Proxied=true) — RunCopyTask treats both identically: a plain
// GET with Headers attached.
type SrcLink struct {
	URL     string
	Headers http.Header
	Size    int64
	Proxied bool
}

type srcLinkReq struct {
	CapToken string `json:"cap_token"`
}

type srcLinkWire struct {
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
	Size    int64       `json:"size"`
	Proxied bool        `json:"proxied"`
}

// FetchSrcLink POSTs to /api/v4/agent/task/:id/src-link and returns the
// resolved download source for the task's src.
func FetchSrcLink(ctx context.Context, base, taskID, capToken string) (SrcLink, error) {
	var wire srcLinkWire
	path := fmt.Sprintf("/api/v4/agent/task/%s/src-link", taskID)
	if err := postJSON(ctx, base, path, srcLinkReq{CapToken: capToken}, &wire); err != nil {
		return SrcLink{}, err
	}
	return SrcLink{URL: wire.URL, Headers: wire.Headers, Size: wire.Size, Proxied: wire.Proxied}, nil
}

type rapidReq struct {
	CapToken string `json:"cap_token"`
}

type rapidWire struct {
	Rapid bool `json:"rapid"`
}

// TryRapidUpload POSTs to /api/v4/agent/task/:id/rapid and returns whether the
// server registered the file by hash ("秒传" hit). A false result — or ANY
// transport error the caller treats as non-fatal — means fall back to
// download+stream (design SP-H).
func TryRapidUpload(ctx context.Context, base, taskID, capToken string) (bool, error) {
	var wire rapidWire
	path := fmt.Sprintf("/api/v4/agent/task/%s/rapid", taskID)
	if err := postJSON(ctx, base, path, rapidReq{CapToken: capToken}, &wire); err != nil {
		return false, err
	}
	return wire.Rapid, nil
}

type progressReq struct {
	CapToken string `json:"cap_token"`
	Progress int    `json:"progress"`
	Detail   string `json:"detail"`
}

// ReportProgress POSTs a progress update (0-100 — the server itself clamps
// out-of-range values, see AgentTaskProgress) plus a free-form detail
// string to /api/v4/agent/task/:id/progress.
func ReportProgress(ctx context.Context, base, taskID, capToken string, pct int, detail string) error {
	var discard struct{}
	path := fmt.Sprintf("/api/v4/agent/task/%s/progress", taskID)
	return postJSON(ctx, base, path, progressReq{CapToken: capToken, Progress: pct, Detail: detail}, &discard)
}

type statusReq struct {
	CapToken string `json:"cap_token"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
}

// ReportStatus POSTs a status transition (one of proto.TaskStatusRunning/Done/
// Failed) plus a free-form detail string to /api/v4/agent/task/:id/status.
func ReportStatus(ctx context.Context, base, taskID, capToken, status, detail string) error {
	var discard struct{}
	path := fmt.Sprintf("/api/v4/agent/task/%s/status", taskID)
	return postJSON(ctx, base, path, statusReq{CapToken: capToken, Status: status, Detail: detail}, &discard)
}

type renewCapReq struct {
	CapToken string `json:"cap_token"`
}

type renewCapWire struct {
	CapToken  string `json:"cap_token"`
	ExpiresAt int64  `json:"expires_at"`
}

// RenewCap POSTs to /api/v4/agent/task/:id/renew and returns the freshly
// minted token plus its expiry (design cap-renewal) — the outbound-mode HTTP
// leg of CapClient.RenewCap; see that interface method's doc.
func RenewCap(ctx context.Context, base, taskID, capToken string) (string, int64, error) {
	var wire renewCapWire
	path := fmt.Sprintf("/api/v4/agent/task/%s/renew", taskID)
	if err := postJSON(ctx, base, path, renewCapReq{CapToken: capToken}, &wire); err != nil {
		return "", 0, err
	}
	return wire.CapToken, wire.ExpiresAt, nil
}

// progressReportMinPctStep/-Interval throttle how often the upload's
// progress reader actually calls back to the server: a naive per-Read
// report would fire dozens-to-hundreds of times a second for a large
// transfer over a fast link, hammering the control-plane endpoint for no
// operational benefit. A report fires only once BOTH thresholds are
// crossed since the last one — except the final byte, which always reports
// (unconditionally, if it strictly increases pct) so a caller watching
// progress can tell the upload actually finished reading.
const (
	progressReportMinPctStep  = 5
	progressReportMinInterval = 2 * time.Second
)

// progressReader wraps an io.Reader, throttled-reporting read progress
// (0-100, based on total) via onPct as bytes flow through Read. onPct is
// called synchronously, in-line with Read, and MUST swallow its own errors
// — progress telemetry must never be allowed to fail (nor, beyond whatever
// the report call itself costs, slow-path-block) the transfer it merely
// instruments. See newProgressReader's callers for the swallow-errors
// wiring (ReportProgress's error is discarded there).
type progressReader struct {
	r       io.Reader
	total   int64
	read    int64
	lastPct int
	lastAt  time.Time
	onPct   func(pct int)
}

func newProgressReader(r io.Reader, total int64, onPct func(pct int)) *progressReader {
	return &progressReader{r: r, total: total, onPct: onPct}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		p.maybeReport()
	}
	return n, err
}

func (p *progressReader) maybeReport() {
	if p.total <= 0 || p.onPct == nil {
		return
	}
	pct := int(p.read * 100 / p.total)
	if pct > 100 {
		pct = 100
	}
	if pct <= p.lastPct {
		return
	}
	now := time.Now()
	crossedPct := pct - p.lastPct
	due := pct == 100 || (crossedPct >= progressReportMinPctStep && now.Sub(p.lastAt) >= progressReportMinInterval)
	if !due {
		return
	}
	p.lastPct = pct
	p.lastAt = now
	p.onPct(pct)
}

// downloadSrc GETs link.URL with link.Headers attached and returns a body
// ready to upload plus its length.
//
// Content-Length resolution, in priority order (the content endpoint
// requires a definite length — see server/handles.AgentTaskPutContent's
// doc — so this always returns one, or an error):
//
//  1. the GET response's OWN Content-Length, when known (>=0) — the actual
//     transferred length always wins over what src-link merely predicted,
//     since that is what will actually be read off body.
//  2. link.Size (from src-link's response), when the transport didn't
//     declare one but the server-side object size is known and positive.
//  3. neither known (a genuinely chunked/length-unknown source with no
//     usable fallback): the WHOLE body is spooled to a temp file first,
//     purely to learn its length, then served back off disk — see
//     spoolToTemp.
//
// scrubURLError strips the full request URL out of a *url.Error before it can
// reach a task-failure log line that egresses to the admin UI (agent logs now
// stream to the server): a source download link may be presigned or carry a
// credential in its query string. Keeps the operation, the host, and the
// underlying error — enough to diagnose without leaking the signed URL.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		host := "source"
		if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
			host = u.Host
		}
		return fmt.Errorf("%s %s: %w", ue.Op, host, ue.Err)
	}
	return err
}

func downloadSrc(ctx context.Context, link SrcLink) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("agent: build src download request: %w", err)
	}
	for k, vs := range link.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("agent: src download request failed: %w", scrubURLError(err))
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("agent: src download returned status %d: %s", resp.StatusCode, string(body))
	}

	if resp.ContentLength >= 0 {
		return resp.Body, resp.ContentLength, nil
	}
	if link.Size > 0 {
		return resp.Body, link.Size, nil
	}
	return spoolToTemp(resp.Body)
}

// spoolToTemp copies src (which it always closes, success or failure) to a
// temp file purely to learn its length, then rewinds it for reading. The
// returned ReadCloser deletes the temp file on Close.
func spoolToTemp(src io.ReadCloser) (io.ReadCloser, int64, error) {
	defer src.Close()
	tmp, err := os.CreateTemp("", "alist-agent-task-*")
	if err != nil {
		return nil, 0, fmt.Errorf("agent: create temp spool file: %w", err)
	}
	n, err := io.Copy(tmp, src)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, fmt.Errorf("agent: spool src to temp file: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, fmt.Errorf("agent: rewind temp spool file: %w", err)
	}
	return &tempSpoolFile{File: tmp}, n, nil
}

// tempSpoolFile deletes its backing file on Close, so a spooled download's
// on-disk footprint never outlives the single upload it was spooled for.
type tempSpoolFile struct {
	*os.File
}

func (t *tempSpoolFile) Close() error {
	err := t.File.Close()
	_ = os.Remove(t.File.Name())
	return err
}

// uploadContent PUTs exactly size bytes of body to
// /api/v4/agent/task/:id/content with the capability token on the
// Agent-Cap-Token HEADER — never a query parameter, matching
// server/handles.AgentTaskPutContent's doc on why the header-only form is
// deliberate (a query string is far more likely to be captured verbatim in
// access/proxy logs or a redirect's Referer). body is wrapped in a
// throttled progress reader that reports through onPct as it is read.
func uploadContent(ctx context.Context, base, taskID, capToken string, body io.Reader, size int64, onPct func(pct int)) error {
	pr := newProgressReader(body, size, onPct)
	url := strings.TrimRight(base, "/") + fmt.Sprintf("/api/v4/agent/task/%s/content", taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, io.NopCloser(pr))
	if err != nil {
		return fmt.Errorf("agent: build content upload request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Agent-Cap-Token", capToken)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("agent: content upload request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("agent: read content upload response: %w", err)
	}
	var env envelope[struct{}]
	if jsonErr := json.Unmarshal(respBody, &env); jsonErr != nil {
		return fmt.Errorf("agent: decode content upload response (http %d): %s", resp.StatusCode, string(respBody))
	}
	if env.Code != 200 {
		return fmt.Errorf("agent: server rejected content upload (code %d): %s", env.Code, env.Message)
	}
	return nil
}

// --- proactive cap-token renewal (design cap-renewal) ---
//
// A capability token is scoped to at most agentsrv.MaxTaskCapTTL server-side
// (internal/agentsrv/taskcap.go — this module cannot import that package, an
// independent module, so assumedCapTTL below duplicates the VALUE, not the
// dependency). Before this, a copy that ran longer than that TTL would start
// 401ing on src-link/put-content/progress partway through: outbound mode
// happened to paper over this as long as the heartbeat interval stayed well
// under the TTL (Runner.Tick re-mints a token on every heartbeat for the
// still-active task and updates the SAME TaskToken RunCopyTask reads via
// tokenFn — see runner.go), but reverse mode had no equivalent at all: a
// dispatched copy captures a single StaticToken at dispatch time
// (reverse.go's handleDispatch), and the server's own re-push of a fresh
// token for the still-in-flight task is thrown away by the gate that refuses
// a second concurrent dispatch of the same task id (see TaskGate.TryStart) —
// nothing ever updates the running goroutine's token.
//
// wrapRenewingToken closes that gap uniformly, on EITHER transport, by making
// the copy executor itself proactively ask for a fresh token via
// CapClient.RenewCap well before the current one's remaining life runs out,
// rather than depending on an external mechanism to hand it one.

const (
	// assumedCapTTL mirrors agentsrv.MaxTaskCapTTL server-side. A mismatch
	// only shifts renewal TIMING, never correctness — the server alone
	// decides whether a presented token is still valid (see
	// agentTaskRenewCapCore), and a renewal failure is never fatal (the copy
	// just keeps its old token — see wrapRenewingToken below).
	assumedCapTTL = 15 * time.Minute
	// capRenewThresholdFraction: renew once a token's own remaining life
	// (read back WITHOUT verifying its signature — see capTokenExpiry) drops
	// under assumedCapTTL/this.
	capRenewThresholdFraction = 3
)

// capRenewMaxInterval/capRenewRetryBackoff are vars, not consts, purely so
// tests can shrink them without waiting real minutes; production code never
// mutates them.
var (
	// capRenewMaxInterval backstops the threshold check for a token whose
	// remaining life this module can't parse (or that was minted with a much
	// longer TTL than assumedCapTTL): renew at least this often regardless.
	capRenewMaxInterval = 10 * time.Minute
	// capRenewRetryBackoff floors how soon a FAILED renewal attempt is
	// retried, so a persistently unreachable RenewCap endpoint is not
	// hammered on every single cap call while a token nears expiry.
	capRenewRetryBackoff = 30 * time.Second
)

// capTokenClaims is the subset of agentsrv.TaskCapClaims
// (internal/agentsrv/taskcap.go) this module cares about: just the expiry.
// This module cannot import that package (independent module) and has no
// need to verify the token's signature to read its own expiry back out of a
// token it already holds — that is simply peeking at data the agent already
// possesses, never a way to forge or extend anything (the server alone
// decides what a renewal actually returns; this is purely local scheduling
// input for wrapRenewingToken).
type capTokenClaims struct {
	Exp int64 `json:"exp"`
}

// capTokenExpiry decodes a cap token's own claimed expiry WITHOUT verifying
// its signature (see capTokenClaims's doc). ok is false for anything
// malformed or unparseable — callers must treat that as "unknown expiry",
// never as "already expired".
func capTokenExpiry(token string) (exp time.Time, ok bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return time.Time{}, false
	}
	var claims capTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// wrapRenewingToken wraps base with proactive cap-token renewal.
// RunCopyTaskWith installs this around whatever TokenFunc its caller
// supplied, so a copy that runs longer than one token's TTL keeps working
// without the caller needing to do anything transport-specific.
//
// Every call first re-reads base(): an EXTERNAL renewal the caller's own
// TokenFunc holder picked up on its own (e.g. the outbound Runner updating
// its TaskToken from a heartbeat that re-dispatched this same task, design
// W12-4 (A)) always wins and resets this wrapper's own renewal clock — this
// wrapper's OWN active RenewCap calls are a second, transport-uniform
// mechanism layered underneath that, for exactly the case the external
// mechanism does not cover (reverse mode's push-dispatch renewal never
// reaches an already-in-flight copy — see this section's top doc).
//
// A RenewCap failure (denial or transport error) is swallowed: the OLD token
// keeps being served, and the copy proceeds on it until/unless the server
// genuinely rejects it — a renewal hiccup must never abort an otherwise
// healthy copy (see CapClient.RenewCap's doc). Retries after a failure are
// floored by capRenewRetryBackoff so a persistently failing renewal isn't
// attempted on every single cap call.
func wrapRenewingToken(ctx context.Context, cc CapClient, taskID string, base TokenFunc) TokenFunc {
	var (
		mu          sync.Mutex
		current     = base()
		lastRenew   = time.Now()
		lastAttempt time.Time
	)

	return func() string {
		mu.Lock()
		if ext := base(); ext != current {
			// The caller's own base token changed underneath us — an external
			// renewal always wins and resets our own renewal clock.
			current = ext
			lastRenew = time.Now()
			lastAttempt = time.Time{}
		}
		tok := current
		sinceRenew := time.Since(lastRenew)
		attempted := !lastAttempt.IsZero()
		sinceAttempt := time.Since(lastAttempt)
		mu.Unlock()

		if attempted && sinceAttempt < capRenewRetryBackoff {
			return tok
		}

		due := sinceRenew >= capRenewMaxInterval
		if !due {
			if exp, ok := capTokenExpiry(tok); ok {
				due = time.Until(exp) < assumedCapTTL/capRenewThresholdFraction
			}
		}
		if !due {
			return tok
		}

		mu.Lock()
		lastAttempt = time.Now()
		mu.Unlock()

		newTok, _, err := cc.RenewCap(ctx, taskID, tok)

		mu.Lock()
		defer mu.Unlock()
		if err != nil || newTok == "" {
			return current
		}
		if current == tok {
			// Nothing external landed while we awaited RenewCap — adopt the
			// freshly renewed token.
			current = newTok
			lastRenew = time.Now()
		}
		return current
	}
}

// reportFailureTimeout bounds the best-effort failure report RunCopyTask
// makes after a copy step fails — see RunCopyTask's doc on why this
// deliberately runs on a context derived WITHOUT the original ctx's
// cancellation (a copy failing BECAUSE ctx was cancelled/timed out must not
// also prevent reporting that failure).
const reportFailureTimeout = 15 * time.Second

// RunCopyTask executes one delegated cross-drive copy end to end: reports
// running, fetches the source's download link and GETs it, uploads the
// bytes to this task's destination (throttled progress along the way), and
// reports the final outcome. See this file's top-of-file doc and
// server/handles/agent_task.go's top-of-file doc — every HTTP call this
// makes is independently re-authorized server-side; nothing here is itself
// a trust boundary.
//
// Honors ctx cancellation throughout: both HTTP calls and the temp-spool
// copy are bound to ctx via http.NewRequestWithContext, so a caller
// cancelling ctx unwinds the whole in-flight operation.
//
// On any failure downstream of the initial running-report, a best-effort
// ReportStatus(failed) is attempted (on an independent, un-cancelled
// context, see reportFailureTimeout) before returning the ORIGINAL error —
// the failure report's own error is deliberately discarded so a
// control-plane hiccup while reporting the failure can never mask the real
// cause the caller needs to see.
//
// tokenFn is consulted fresh before every server call (see TokenFunc's
// doc) rather than once at the top — this is what lets a copy that outlives
// one capability token's TTL keep working, as long as the caller keeps
// tokenFn's backing value current.
func RunCopyTask(ctx context.Context, base string, task Task, tokenFn TokenFunc) error {
	return RunCopyTaskWith(ctx, NewHTTPCapClient(base), task, tokenFn)
}

// RunCopyTaskWith is the transport-agnostic copy executor (design R3): it runs
// the exact same sequence RunCopyTask always has — report running, fetch the
// source's download link and GET it, stream the bytes to the destination
// (throttled progress along the way), report the final outcome — but drives
// every capability call through the injected CapClient rather than hard-wired
// HTTP. RunCopyTask is the outbound-mode convenience wrapper (an httpCapClient
// over base); the reverse-mode agent passes a channelCapClient so the identical
// logic runs over the encrypted channel instead. Every cap call this makes is
// independently re-authorized server-side regardless of transport (see
// server/handles/agent_task_core.go's doc) — nothing here is a trust boundary.
//
// The source download itself (downloadSrc) is a plain GET of whatever URL
// SrcLink returned and is deliberately NOT part of CapClient: it is not a
// capability op against the paired server, just a fetch of the (possibly
// direct-from-drive, possibly server-proxied) source bytes.
//
// tokenFn is consulted fresh before every cap call (see TokenFunc's doc) so a
// copy that outlives one capability token's TTL keeps working as the caller
// renews it — and, as of design cap-renewal, tokenFn itself is wrapped in
// wrapRenewingToken, so the copy ALSO proactively renews its own token via
// cc.RenewCap when the caller's own tokenFn does not, closing the gap reverse
// mode otherwise has (see that section's top doc). On any failure downstream
// of the initial running-report a best-effort Status(failed) is attempted on
// an independent, un-cancelled context before the ORIGINAL error is returned.
func RunCopyTaskWith(ctx context.Context, cc CapClient, task Task, tokenFn TokenFunc) error {
	tokenFn = wrapRenewingToken(ctx, cc, task.TaskID, tokenFn)

	if err := cc.Status(ctx, task.TaskID, tokenFn(), proto.TaskStatusRunning, ""); err != nil {
		return fmt.Errorf("agent: report running: %w", err)
	}

	// Rapid-upload first (design SP-H): ask the server to register the file by
	// hash without moving any bytes. On a hit we're done — skip the whole
	// download+stream path. A TryRapid transport error is NON-fatal: fall
	// through to streaming, exactly as a rapid miss does.
	rapid, rerr := cc.TryRapid(ctx, task.TaskID, tokenFn())
	if rerr == nil && rapid {
		if err := cc.Status(ctx, task.TaskID, tokenFn(), proto.TaskStatusDone, proto.RapidUploadDetail); err != nil {
			return fmt.Errorf("agent: report done (rapid): %w", err)
		}
		return nil
	}

	link, err := cc.SrcLink(ctx, task.TaskID, tokenFn())
	if err != nil {
		err = fmt.Errorf("agent: fetch src link: %w", err)
		reportFailure(cc, task.TaskID, tokenFn, err)
		return err
	}

	body, size, err := downloadSrc(ctx, link)
	if err != nil {
		reportFailure(cc, task.TaskID, tokenFn, err)
		return err
	}
	defer body.Close()

	onPct := func(pct int) {
		_ = cc.Progress(ctx, task.TaskID, tokenFn(), pct, "")
	}
	if err := cc.PutContent(ctx, task.TaskID, tokenFn(), body, size, onPct); err != nil {
		reportFailure(cc, task.TaskID, tokenFn, err)
		return err
	}

	if err := cc.Status(ctx, task.TaskID, tokenFn(), proto.TaskStatusDone, ""); err != nil {
		return fmt.Errorf("agent: report done: %w", err)
	}
	return nil
}

// reportFailure best-effort reports a failed status after a copy step
// failed — see RunCopyTaskWith's doc on why its own error is discarded and why
// it runs on an independent, bounded, un-cancelled context. tokenFn is
// consulted at the moment of the report, same as every other cap call.
func reportFailure(cc CapClient, taskID string, tokenFn TokenFunc, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), reportFailureTimeout)
	defer cancel()
	_ = cc.Status(ctx, taskID, tokenFn(), proto.TaskStatusFailed, cause.Error())
}
