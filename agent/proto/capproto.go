package proto

import "net/http"

// Reverse-channel cap-op wire protocol (design R3).
//
// These method names and payload shapes are the contract shared by the two
// ends of a reverse *channel.Channel: the agent-side channelCapClient
// (internal/agent/channel_capclient.go) that SENDS them, and the server-side
// channel handler (server/handles/agent_channel.go) that RECEIVES them and
// funnels each into the SAME shared authorization core the HTTP path uses.
// They live here, in the shared proto package (moved out of internal/agent in
// the W3 proto-split — design analysis §6 W3), because both ends already
// depend on this package — keeping the channel package itself strictly
// domain-agnostic (it knows nothing about tasks or capabilities) as designed.
//
// Every agent->server cap frame carries its own CapToken: the capability token
// is the authority, exactly as over HTTP. The channel's handshake-proven
// identity is defense-in-depth layered ON TOP (the server additionally binds
// the token's agent id to the channel's agent), never a substitute for it.

const (
	// MethodHeartbeat (agent->server) keeps the reverse-mode agent's LastSeen
	// fresh over the channel, the analogue of the HTTP heartbeat. Task dispatch
	// in reverse mode is a server->agent PUSH (MethodTaskDispatch), not a
	// heartbeat response, so this returns only liveness/enabled.
	MethodHeartbeat = "heartbeat"
	// MethodTaskSrcLink (agent->server) resolves a task's source download link.
	MethodTaskSrcLink = "task.srcLink"
	// MethodTaskTryRapid (agent->server, design SP-H) asks the server to attempt
	// a hash-based rapid-upload ("秒传") of the task's source into the destination
	// BEFORE the agent downloads+streams a copy. The server holds the src hash +
	// dst-driver access; the agent just requests it. A true result means the
	// file is already registered (no bytes moved); false means fall back to the
	// existing src-link -> download -> put-content stream.
	MethodTaskTryRapid = "task.tryRapid"
	// MethodTaskProgress (agent->server) reports a throttled progress update.
	MethodTaskProgress = "task.progress"
	// MethodTaskStatus (agent->server) reports a status transition.
	MethodTaskStatus = "task.status"
	// MethodTaskPutBegin/Chunk/End (agent->server) are the three legs of the
	// streaming content upload — see the stream protocol doc in
	// server/handles/agent_channel.go.
	MethodTaskPutBegin = "task.put.begin"
	MethodTaskPutChunk = "task.put.chunk"
	MethodTaskPutEnd   = "task.put.end"
	// MethodTaskDispatch (server->agent PUSH) hands the agent a claimed task
	// plus a freshly minted capability token — the reverse-mode equivalent of
	// the HTTP heartbeat carrying a task.
	MethodTaskDispatch = "task.dispatch"
	// MethodTaskRenewCap (agent->server, design cap-renewal) asks the server to
	// re-mint a fresh capability token for the SAME (agent, task, src, dst)
	// tuple the presented token already names, re-running the identical
	// authorization chain every other cap op does (see
	// server/handles/agent_task_core.go's agentTaskRenewCapCore) — renewal
	// narrows nothing further and grants nothing extra; it is purely a
	// fresh-TTL reissue of an authorization that must independently still
	// hold. It exists so a copy that runs longer than one capability token's
	// TTL (internal/agentsrv.MaxTaskCapTTL server-side) does not start 401ing
	// partway through: the copy executor (this module's task.go,
	// wrapRenewingToken) calls this proactively, well before the current
	// token's remaining life runs out, on EITHER transport.
	MethodTaskRenewCap = "task.renewCap"
	// MethodAgentLogTail (server->agent REQUEST, design SP-G1) pulls the agent's
	// recent in-memory log ring for the admin observability surface. It is a
	// pure poll — the server sends {since}, the agent returns the buffered
	// entries with seq > since (capped at MaxLogTailEntries) plus the next
	// cursor — with NO server->agent push/subscription. SECURITY: log lines
	// egress to the server here; see logring.go's note that log messages must
	// stay secret-free.
	MethodAgentLogTail = "agent.log.tail"
)

// MaxLogTailEntries caps how many log entries one agent.log.tail response may
// carry, bounding both the channel frame and the admin endpoint's response. The
// agent enforces it when slicing its ring; the server additionally re-clamps a
// response it receives (never trust the far end's self-limit).
const MaxLogTailEntries = 200

// ChannelLogTailReq is the server->agent log-tail request: return buffered log
// entries with seq strictly greater than Since.
type ChannelLogTailReq struct {
	Since int64 `json:"since"`
}

// ChannelLogTailResp is the agent's reply: up to MaxLogTailEntries entries
// (oldest first) newer than the requested cursor, plus Next — the cursor to
// pass as Since on the following poll. LogEntry (logring.go) is the per-entry
// shape and the G2 frontend contract.
type ChannelLogTailResp struct {
	Entries []LogEntry `json:"entries"`
	Next    int64      `json:"next"`
}

// MaxChannelChunkBytes bounds ONE streamed content chunk's raw payload. The
// agent chunks the body into pieces no larger than this; the server refuses a
// chunk whose decoded length exceeds it. Kept comfortably under the channel's
// own per-frame ceiling (channel.maxFrameBytes, 8 MiB) even after base64/JSON
// framing overhead, so a legitimate max-size chunk never trips the frame read
// limit. Bounding the chunk is what keeps the streaming upload's transient
// memory footprint small — only one chunk is ever in flight (each chunk is a
// request the agent awaits an ack for before sending the next), and the server
// hands each chunk straight to op.Put without accumulating them.
const MaxChannelChunkBytes = 1 << 20 // 1 MiB

// ChannelSrcLinkReq/Resp mirror the HTTP src-link request/response over the
// channel. Resp is the wire form of server/handles.AgentTaskSrcLinkResp.
type ChannelSrcLinkReq struct {
	TaskID   string `json:"task_id"`
	CapToken string `json:"cap_token"`
}

type ChannelSrcLinkResp struct {
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
	Size    int64       `json:"size"`
	Proxied bool        `json:"proxied"`
}

// ChannelTryRapidReq/Resp are the channel form of the rapid-upload request and
// its result (design SP-H). Rapid=true means the server registered the file by
// hash (no bytes moved); false means the agent must fall back to streaming.
type ChannelTryRapidReq struct {
	TaskID   string `json:"task_id"`
	CapToken string `json:"cap_token"`
}

type ChannelTryRapidResp struct {
	Rapid bool `json:"rapid"`
}

// ChannelProgressReq is the channel form of a progress report.
type ChannelProgressReq struct {
	TaskID   string `json:"task_id"`
	CapToken string `json:"cap_token"`
	Progress int    `json:"progress"`
	Detail   string `json:"detail"`
}

// ChannelStatusReq is the channel form of a status transition.
type ChannelStatusReq struct {
	TaskID   string `json:"task_id"`
	CapToken string `json:"cap_token"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
}

// ChannelRenewCapReq/Resp are the channel form of a cap-renewal request
// (design cap-renewal). Like every other cap op, the CURRENTLY held token is
// what authenticates the request — asking to renew is not itself a standing
// grant, it is re-verified from scratch every time (see
// agentTaskRenewCapCore) — so a token that would fail src-link just as surely
// fails renewal. ExpiresAt is informational only (a unix timestamp); the
// executor re-derives its own renewal timing by peeking at the returned
// token's own claimed expiry (see task.go's capTokenExpiry), since that is
// the one value both transports and the HTTP form guarantee to agree on.
type ChannelRenewCapReq struct {
	TaskID   string `json:"task_id"`
	CapToken string `json:"cap_token"`
}

type ChannelRenewCapResp struct {
	CapToken  string `json:"cap_token"`
	ExpiresAt int64  `json:"expires_at"`
}

// ChannelPutBeginReq opens a streaming content upload. StreamID is a
// per-upload correlation id the agent generates; Size is the exact total the
// agent intends to stream (the server bounds the actual bytes received against
// it and against its own hard cap). The cap token is re-verified server-side
// before a single byte is accepted.
type ChannelPutBeginReq struct {
	TaskID   string `json:"task_id"`
	CapToken string `json:"cap_token"`
	StreamID string `json:"stream_id"`
	Size     int64  `json:"size"`
}

// ChannelPutChunkReq carries one ordered chunk of the stream. Data is raw
// bytes (encoding/json renders a []byte as base64, so the chunk travels as a
// base64 string inside the AEAD-sealed frame). Seq is the 0-based monotonic
// chunk index; the server refuses an out-of-order Seq (defense in depth on top
// of the channel's own in-order, AEAD-authenticated delivery).
type ChannelPutChunkReq struct {
	StreamID string `json:"stream_id"`
	Seq      uint64 `json:"seq"`
	Data     []byte `json:"data"`
}

// ChannelPutEndReq closes the stream; the server flushes EOF to op.Put and
// returns the final outcome.
type ChannelPutEndReq struct {
	StreamID string `json:"stream_id"`
}

// ChannelAck is the trivial ok reply shared by the streaming legs.
type ChannelAck struct {
	OK bool `json:"ok"`
}

// ChannelHeartbeatResp is the reply to a channel heartbeat.
type ChannelHeartbeatResp struct {
	OK      bool `json:"ok"`
	Enabled bool `json:"enabled"`
}

// ChannelDispatch is the server->agent task-dispatch push payload — the
// channel analogue of server/handles.AgentHeartbeatTaskDTO / this package's
// HeartbeatTask: everything the agent needs to run one delegated copy,
// including a freshly minted capability token.
type ChannelDispatch struct {
	TaskID   string `json:"task_id"`
	Type     string `json:"type"`
	SrcPath  string `json:"src_path"`
	DstDir   string `json:"dst_dir"`
	CapToken string `json:"cap_token"`
}
