package agent

import (
	"context"
	"io"
)

// CapClient is the transport abstraction the copy executor drives the four
// capability operations through (design R3). It exists so RunCopyTask's logic
// — report running, fetch the source link, stream the bytes to the
// destination, report progress/status — is written ONCE and runs UNCHANGED
// over either transport:
//
//   - outbound mode (the original W12 model): httpCapClient, which POSTs/PUTs
//     the anonymous cap_token-authed HTTP endpoints server/handles/agent_task.go
//     exposes. This is what `alist agent run` uses today.
//   - reverse mode (design R2/R3): channelCapClient (channel_capclient.go),
//     which sends each op as a frame over the mutual-authed, AEAD-encrypted
//     reverse *channel.Channel the server dialed. The server's channel handler
//     funnels every op into the SAME shared authorization core the HTTP path
//     uses (server/handles/agent_task_core.go), so the capability gating is
//     identical no matter which transport carried the request — the channel is
//     transport, never authorization.
//
// Every method takes the freshest capability token EXPLICITLY (not captured
// once) so a copy that outlives a single token's TTL keeps working as the
// caller renews it — see TokenFunc's doc in task.go. A token is a bearer
// secret threaded through call sites, never tucked inside a long-lived struct.
type CapClient interface {
	// SrcLink resolves where/how to download the task's source content.
	SrcLink(ctx context.Context, taskID, capToken string) (SrcLink, error)
	// TryRapid asks the server to attempt a hash-based rapid-upload ("秒传") of
	// the task's source into the destination without transferring bytes (design
	// SP-H). true => the file is already registered (skip the whole
	// download+stream); false => fall back to SrcLink -> download -> PutContent.
	// A transport error is NON-fatal to the copy: the caller falls back to
	// streaming, it never fails the task.
	TryRapid(ctx context.Context, taskID, capToken string) (bool, error)
	// PutContent streams exactly size bytes of body to the task's destination,
	// reporting read progress through onPct (which may be nil). It must never
	// buffer the whole body — the transport streams it.
	PutContent(ctx context.Context, taskID, capToken string, body io.Reader, size int64, onPct func(pct int)) error
	// Progress reports a throttled progress update (0-100 + free-form detail).
	Progress(ctx context.Context, taskID, capToken string, pct int, detail string) error
	// Status reports a status transition (running|done|failed + detail).
	Status(ctx context.Context, taskID, capToken, status, detail string) error
	// RenewCap asks the server to re-mint a fresh capability token for the
	// SAME task the presented capToken already names (design cap-renewal) —
	// see task.go's wrapRenewingToken, which is what actually calls this
	// proactively from inside the copy executor, well before the presented
	// token's own remaining life runs out. expiresAt is a unix timestamp,
	// informational only. A denial (task finished, agent disabled, whitelist
	// narrowed, creator de-authorized — the exact same live re-check every
	// other cap op runs) surfaces as a non-nil error; the caller treats that
	// as non-fatal and keeps using the OLD token until it is genuinely
	// rejected (see RunCopyTaskWith).
	RenewCap(ctx context.Context, taskID, capToken string) (newToken string, expiresAt int64, err error)
}

// httpCapClient is the outbound-mode CapClient: a thin adapter over the
// package-level HTTP helpers (FetchSrcLink / uploadContent / ReportProgress /
// ReportStatus) that have driven `agent run` since W12-3b. base is the paired
// server's base URL. It carries no per-call state, so one instance is safe to
// reuse across a run.
type httpCapClient struct {
	base string
}

// NewHTTPCapClient builds an outbound-mode CapClient targeting the paired
// server at base (a URL like "http://localhost:5244", trailing slash
// optional).
func NewHTTPCapClient(base string) CapClient { return &httpCapClient{base: base} }

func (h *httpCapClient) SrcLink(ctx context.Context, taskID, capToken string) (SrcLink, error) {
	return FetchSrcLink(ctx, h.base, taskID, capToken)
}

func (h *httpCapClient) TryRapid(ctx context.Context, taskID, capToken string) (bool, error) {
	return TryRapidUpload(ctx, h.base, taskID, capToken)
}

func (h *httpCapClient) PutContent(ctx context.Context, taskID, capToken string, body io.Reader, size int64, onPct func(pct int)) error {
	return uploadContent(ctx, h.base, taskID, capToken, body, size, onPct)
}

func (h *httpCapClient) Progress(ctx context.Context, taskID, capToken string, pct int, detail string) error {
	return ReportProgress(ctx, h.base, taskID, capToken, pct, detail)
}

func (h *httpCapClient) Status(ctx context.Context, taskID, capToken, status, detail string) error {
	return ReportStatus(ctx, h.base, taskID, capToken, status, detail)
}

func (h *httpCapClient) RenewCap(ctx context.Context, taskID, capToken string) (string, int64, error) {
	return RenewCap(ctx, h.base, taskID, capToken)
}
