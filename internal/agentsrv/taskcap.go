package agentsrv

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/sign"
)

// MaxTaskCapTTL is the hard ceiling on a task capability token's lifetime
// (design W12-3a): IssueTaskCap clamps any zero/negative/over-long requested
// ttl down to this, never up — a capability that scopes exactly one task's
// src(read)+dst(write) must not linger past the point a heartbeat would have
// re-issued a fresh one anyway.
const MaxTaskCapTTL = 15 * time.Minute

// TaskCapClaims is the payload signed inside a task capability token: the
// EXACT (agent, task, src, dst) tuple the token authorizes, plus its own
// expiry. A verified TaskCapClaims is proof the token was minted by this
// server for this agent/task/paths and has not expired — nothing more. It
// is NOT proof the caller may still act: every agent-facing endpoint
// re-loads the live task/agent rows and re-runs the storage-whitelist +
// op.Authorize checks against the CREATOR's identity before touching
// anything (see server/handles/agent_task.go's top-of-file doc — "capability
// is not a bypass").
type TaskCapClaims struct {
	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id"`
	Src     string `json:"src"`
	DstDir  string `json:"dst_dir"`
	Exp     int64  `json:"exp"`
}

// IssueTaskCap mints a capability token scoping exactly
// (agentID, taskID, read src, write dstDir) for at most MaxTaskCapTTL.
//
// The token is `base64url(JSON claims) + "." + sign`, where sign is
// internal/sign.WithDuration(payload, ttl) — the existing HMAC-signing
// primitive already used for signed download links. That gives the token
// two independent, redundant expiry enforcements (defense in depth, per the
// security posture this feature demands): the HMAC signature itself embeds
// and checks an expiry (pkg/sign/hmac.go — tamper-proof, since the expiry is
// part of what's signed), and VerifyTaskCap separately re-checks the
// claims' own Exp field after the signature has already been confirmed
// genuine. Either check alone would be sufficient; both run.
//
// ttl <= 0 or > MaxTaskCapTTL is clamped down to MaxTaskCapTTL — callers may
// ask for shorter, never longer. Fails closed: if the underlying signer is
// unavailable (internal/sign has no active signing key yet), no token is
// minted at all — an empty token or one signed with a zero/weak key would
// be worse than refusing outright.
func IssueTaskCap(agentID, taskID, src, dstDir string, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > MaxTaskCapTTL {
		ttl = MaxTaskCapTTL
	}
	claims := TaskCapClaims{
		AgentID: agentID,
		TaskID:  taskID,
		Src:     src,
		DstDir:  dstDir,
		Exp:     time.Now().Add(ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sig := sign.WithDuration(string(payload), ttl)
	if sig == "" {
		// sign.WithDuration returns "" exactly when no signer is available
		// (internal/sign/sign.go) — fail closed rather than hand back an
		// unsigned/garbage token.
		return "", errs.InvalidTaskCap
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + sig, nil
}

// VerifyTaskCap verifies a token minted by IssueTaskCap:
//
//  1. well-formed shape (exactly one "." separator, both halves non-empty,
//     the first half valid base64url JSON);
//  2. HMAC signature over the EXACT payload bytes (internal/sign.Verify —
//     fails closed if the signer is unavailable, and this is where the
//     signature's own embedded expiry is enforced);
//  3. the claims' own Exp field, independently re-checked (see IssueTaskCap's
//     doc on why this is deliberately redundant with #2);
//  4. every claim field is non-empty (a token with an empty task_id, say,
//     must never be treated as "matches an empty path param").
//
// Any failure — malformed, tampered, expired, or structurally incomplete —
// collapses to the single errs.InvalidTaskCap. There is no oracle here: a
// caller cannot distinguish "signature didn't verify" from "expired" from
// "malformed" by the error returned.
//
// VerifyTaskCap alone does NOT establish that the token's claims still match
// live state (the task might have been mutated or removed, the agent might
// have been disabled since) — callers must additionally re-load and
// re-check those, see server/handles/agent_task.go's resolveTaskCap.
func VerifyTaskCap(token string) (TaskCapClaims, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return TaskCapClaims{}, errs.InvalidTaskCap
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return TaskCapClaims{}, errs.InvalidTaskCap
	}
	var claims TaskCapClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return TaskCapClaims{}, errs.InvalidTaskCap
	}
	if err := sign.Verify(string(payload), parts[1]); err != nil {
		return TaskCapClaims{}, errs.InvalidTaskCap
	}
	if claims.Exp <= 0 || time.Now().Unix() > claims.Exp {
		return TaskCapClaims{}, errs.InvalidTaskCap
	}
	if claims.AgentID == "" || claims.TaskID == "" || claims.Src == "" || claims.DstDir == "" {
		return TaskCapClaims{}, errs.InvalidTaskCap
	}
	return claims, nil
}
