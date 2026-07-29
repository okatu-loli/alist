package errs

import "errors"

// AgentNotFound is returned uniformly for a missing remote-agent id (design
// W12-1) — there is no cross-tenant ownership split to hide here (agents are
// admin-only, not per-user), but the same "unknown id" shape as every other
// admin resource (e.g. errs.StorageNotFound) is kept for consistency.
var AgentNotFound = errors.New("agent not found")

// The following (design W12-2a: pair/complete + heartbeat) are deliberately
// generic-worded and never distinguish finer-grained causes in their message
// text — see each var's doc for the specific oracle it closes.

// InvalidPairingCode is returned uniformly by /agent/pair/complete for a
// code that is unknown, expired, OR already used (single-use — a second
// complete with the same code hits this too). Collapsing all three into one
// error/message is deliberate: a guesser must not be able to distinguish
// "never existed" from "expired" from "already claimed by someone else" by
// timing or wording.
var InvalidPairingCode = errors.New("invalid or expired pairing code")

// InvalidAgentPubKey is returned when a pairing request's pubkey does not
// base64-decode to exactly an ed25519 public key (32 bytes).
var InvalidAgentPubKey = errors.New("invalid agent public key")

// InvalidHeartbeatSignature is returned uniformly for a heartbeat whose
// ed25519 signature does not verify against the agent's stored public key
// — malformed base64, wrong length, or a genuinely wrong signature all map
// here rather than leaking which.
var InvalidHeartbeatSignature = errors.New("invalid heartbeat signature")

// StaleHeartbeat is returned when a heartbeat's client-supplied timestamp
// falls outside the server's freshness window — the first leg of replay
// defense (see also ReplayedHeartbeat).
var StaleHeartbeat = errors.New("heartbeat timestamp outside the allowed freshness window")

// ReplayedHeartbeat is returned when a heartbeat's (agent_id, nonce) pair
// has already been accepted within the current freshness window — the
// second leg of replay defense: even a byte-for-byte captured, still-fresh
// heartbeat cannot be resent successfully.
var ReplayedHeartbeat = errors.New("heartbeat nonce already used")

// AgentDisabled is returned when a heartbeat's signature verifies fine but
// the agent has been disabled by an admin.
var AgentDisabled = errors.New("agent is disabled")

// TooManyPairingAttempts is returned when a client IP has exceeded the
// pair/complete rate limit (see server/handles/agent_pair.go's pairThrottle)
// — defense in depth against pairing-code guessing on top of the code's own
// entropy.
var TooManyPairingAttempts = errors.New("too many pairing attempts, please try again later")

// The following (design W12-3a: task delegation) are, again, deliberately
// generic-worded to close an oracle — see each var's doc.

// AgentTaskNotFound is returned uniformly for a missing agent-task id or
// task_id.
var AgentTaskNotFound = errors.New("agent task not found")

// StorageNotWhitelisted is returned when a path a task-creation request or
// an agent-facing capability call touches resolves to a storage the target
// agent's StorageWhitelist does not cover. This is re-checked on EVERY
// agent-facing capability-token call, not just at task-creation time, so an
// admin narrowing (or clearing) a whitelist mid-task cuts the agent off
// immediately — the capability token only narrows what a call may attempt,
// it never substitutes for this check (see server/handles/agent_task.go's
// top-of-file doc).
var StorageNotWhitelisted = errors.New("storage not whitelisted for this agent")

// InvalidTaskCap is returned uniformly by every agent-facing capability
// endpoint (server/handles/agent_task.go) for a capability token that is
// malformed, tampered, expired, minted for a different task/agent than the
// one being addressed, or whose claims no longer match the live task row
// (e.g. the task was mutated after the token was issued). Collapsing all of
// these into one error/message is deliberate, mirroring InvalidPairingCode:
// a caller must not be able to distinguish "expired" from "tampered" from
// "wrong task" by timing or wording.
var InvalidTaskCap = errors.New("invalid or expired task capability")

// AgentLocalDriverUnsupported is returned by CreateAgentTask when the
// source or destination path resolves to a Local-driver storage. A remote
// agent executes on a separate machine from the server — often over the
// open internet — and has no way to reach the server's local disk the way
// it reaches a real cloud drive's direct download URL; Local's only
// possible link back is this server's own signed-proxy fallback (see
// agentTaskSrcLinkCore in server/handles/agent_task_core.go), which
// requires the agent to be able to route to this server's configured
// address at all. Rather than create a task that only fails confusingly
// mid-copy (an agent-side "unsupported protocol scheme" when that address
// is unreachable or unconfigured), CreateAgentTask rejects it up front.
var AgentLocalDriverUnsupported = errors.New("agent copy tasks do not support the Local storage driver as source or destination (the agent cannot reach the server's local disk)")
