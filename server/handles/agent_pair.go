package handles

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"github.com/AlistGo/alist-agent/proto"
	"net/http"
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/authlimit"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Remote-agent pairing-completion + heartbeat (design W12-2a). Both
// handlers are ANONYMOUS routes (mounted outside the auth group, see
// server/router_v4.go, the same posture as /auth/sso/providers): the
// pairing code and the ed25519 signature ARE the credential, not a user
// session. This file is the crypto-critical half of the remote-agent
// feature — every check here is written to be reviewed line by line:
//
//   - pair/complete: a pairing code is single-use (agentsrv.PairingStore.
//     Complete atomically flips pending -> confirmed once); an unknown,
//     expired, or already-used code all produce the exact same 400
//     errs.InvalidPairingCode — no oracle a guesser could use to tell them
//     apart; the endpoint is rate-limited per client IP on top of the
//     code's own ~190 bits of entropy (see internal/agent/pairing.go).
//   - heartbeat: self-authenticated by verifying
//     ed25519.Verify(storedPubKey, HeartbeatMessage(agent_id, ts, nonce),
//     signature) — never a session/cookie/bearer token. Replay is defended
//     on two independent axes: ts must fall within heartbeatFreshness of
//     the server's clock, AND (agent_id, nonce) must not have been seen
//     before within that same window (heartbeatSeenNonces). A disabled
//     agent's heartbeat is rejected even with a perfectly valid signature.

// pairThrottle rate-limits pairing-completion attempts per client IP —
// defense in depth on top of the pairing code's own entropy (blunts a
// brute-force guesser even though guessing a 32-char random code is
// already computationally infeasible within its 15-minute TTL). A separate
// Throttle instance from passwordThrottle/etc. so a pairing-guessing flood
// can never lock out unrelated login paths and vice versa.
var pairThrottle = authlimit.NewThrottle()

// heartbeatSeenNonces is the replay-defense cache: every (agent_id, nonce)
// pair that has been accepted is recorded here for heartbeatNonceCacheTTL,
// so a byte-for-byte captured-and-resent heartbeat — even one still inside
// the ts freshness window — is refused the second time. TTL is deliberately
// longer than heartbeatFreshness so a nonce can never fall out of the cache
// while its ts would still be considered fresh.
var heartbeatSeenNonces = cache.NewMemCache[bool]()

// maxAgentBodyBytes bounds the request body of the anonymous (pre-auth) agent
// endpoints. Pairing-completion and heartbeat bodies are a few hundred bytes;
// capping the read keeps an unauthenticated caller from streaming a huge body
// into memory on these publicly reachable routes.
const maxAgentBodyBytes = 16 << 10

const (
	// heartbeatFreshness is how far a heartbeat's client-supplied unix
	// timestamp may drift from the server's own clock (either direction)
	// before it is rejected as stale. Wide enough to tolerate real clock
	// skew between an agent host and the server, narrow enough that a
	// captured request is only replayable within a short window even
	// before the nonce cache is considered.
	heartbeatFreshness = 3 * time.Minute
	// heartbeatNonceCacheTTL is how long an accepted (agent_id, nonce) is
	// remembered. Set to more than 2x heartbeatFreshness so a nonce can
	// never expire from the cache while a request bearing it could still
	// pass the freshness check.
	heartbeatNonceCacheTTL = 10 * time.Minute
)

// PairAgentCompleteReq is the wire body for POST /agent/pair/complete.
// PubKey is base64 (std) of the raw 32-byte ed25519 public key.
type PairAgentCompleteReq struct {
	Code     string `json:"code" binding:"required"`
	PubKey   string `json:"pubkey" binding:"required"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// PairAgentCompleteResp is the response to a successful pairing completion.
// SAS is this server's own DERIVATION (proto.DeriveSAS(code, pubkey))
// from exactly what THIS request carried — it is included for the agent
// CLI's convenience/cross-check, but it is NOT what makes the MITM defense
// work: an agent that merely echoes this field back to its operator would
// be trusting a value that travelled over the very channel the SAS exists
// to distrust. See internal/agent/sas.go's top-of-file doc: the agent side
// MUST derive its own displayed SAS locally, from the pairing code the
// operator typed plus the identity it generated itself, never from this
// response.
type PairAgentCompleteResp struct {
	AgentID       string   `json:"agent_id"`
	ServerVersion string   `json:"server_version,omitempty"`
	SAS           []string `json:"sas"`
	// ServerPubKey is this server's own ed25519 public key (base64), from the
	// identity boot loaded via agentsrv.CurrentServerIdentity (design R1). The
	// agent PINS it for a later reverse-connection wave (R2), where the server
	// signs its handshake and the agent verifies against this pinned key — the
	// mirror image of the server verifying an agent's heartbeat signature. A
	// public key is not a secret, so returning it on this anonymous endpoint
	// is safe; the pairing SAS already guarantees it was pinned without a MITM.
	// Empty (omitted) when no server identity was wired at boot — the pairing
	// still succeeds, the agent simply has nothing to pin this wave.
	ServerPubKey string `json:"server_pubkey,omitempty"`
}

// PairAgentComplete completes a pairing session an admin already started
// (StartAgentPairing): an agent presents the pairing code plus its freshly
// generated ed25519 public key, and — if the code is a still-pending,
// unexpired, not-yet-used session — a model.Agent row is created (or, on a
// re-pair with the same derived AgentID, updated in place rather than
// duplicated).
//
// design W12-2b: the resulting row is ALWAYS Enabled=false and
// Confirmed=false — including on a re-pair of an already-confirmed agent,
// which is deliberately reset back to unconfirmed every time. Pairing alone
// (possessing the one-time code) is no longer sufficient to grant an agent
// any capability: a man-in-the-middle able to intercept/substitute the
// pubkey in THIS request could otherwise complete pairing as an
// indistinguishable impostor with zero further human involvement. What
// closes that gap is the SAS this handler derives from exactly what it
// received (proto.DeriveSAS(req.Code, pubRaw)) and persists as
// PendingSAS: an admin must fetch it (GET .../pairing-sas), visually match
// it against what the agent's own terminal independently computed, and only
// then POST .../confirm — see that handler's doc for the rest of the flow.
// Until confirmed, the agent can authenticate (heartbeat) but every
// heartbeat is rejected with errs.AgentDisabled (403): an unconfirmed agent
// can do nothing. StorageWhitelist is left empty only for a brand-new row —
// a re-pair preserves whatever an admin had already granted, same as before
// this wave.
// @Summary Complete agent pairing
// @Description Anonymous, rate-limited: the pairing code is the credential. Validates the code (single-use, uniform invalid/expired error), derives AgentID from the submitted ed25519 pubkey, and creates or updates the paired agent row — always Enabled=false/unconfirmed pending an admin's SAS-matched confirm (POST /admin/agents/:id/confirm).
// @Tags agent
// @Accept json
// @Param body body PairAgentCompleteReq true "pairing completion"
// @Success 200 {object} common.Resp[PairAgentCompleteResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 429 {object} common.Resp[any]
// @Router /v4/agent/pair/complete [post]
func PairAgentComplete(c *gin.Context) {
	ip := c.ClientIP()
	if pairThrottle.Blocked(ip) {
		common.ErrorResp(c, errs.TooManyPairingAttempts, 429)
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req PairAgentCompleteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	pubRaw, err := base64.StdEncoding.DecodeString(req.PubKey)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		pairThrottle.Fail(ip)
		audit.Event(c, 0, req.Name, "agent.pair.complete", "", false)
		common.ErrorResp(c, errs.InvalidAgentPubKey, 400)
		return
	}

	if _, ok := agentsrv.Complete(req.Code); !ok {
		pairThrottle.Fail(ip)
		audit.Event(c, 0, req.Name, "agent.pair.complete", "", false)
		common.ErrorResp(c, errs.InvalidPairingCode, 400)
		return
	}

	agentID := proto.FingerprintPubKey(pubRaw)
	pubB64 := base64.StdEncoding.EncodeToString(pubRaw)
	// Derived from exactly (req.Code, pubRaw) — the code this request's own
	// single-use token validated against, and the pubkey this request itself
	// carried. See this handler's top-of-file doc and internal/agent/sas.go
	// for why binding both is what makes a pubkey-substitution MITM produce
	// a DIFFERENT SAS here than whatever the agent computed locally.
	sas := proto.DeriveSAS(req.Code, pubRaw)

	target, err := db.GetAgentByAgentID(agentID)
	if err != nil && !errors.Is(err, errs.AgentNotFound) {
		common.ErrorResp(c, err, 500)
		return
	}
	if target == nil {
		target = &model.Agent{AgentID: agentID}
		target.SetWhitelist(nil) // explicit: no storage access until an admin grants it
	}
	target.Name = req.Name
	target.PubKey = pubB64
	// Every pairing completion — a brand-new agent OR a re-pair of one the
	// admin already confirmed before — lands unconfirmed and disabled.
	// Re-pairing intentionally re-triggers the SAS check: the whole point is
	// that possessing the pairing code alone (which a re-pair still proves)
	// must never be enough to silently re-activate an agent without the
	// human catching a pubkey substitution this time too.
	target.Enabled = false
	target.Confirmed = false
	target.SetPendingSAS(sas)

	if target.ID == 0 {
		if err := db.CreateAgent(target); err != nil {
			pairThrottle.Fail(ip)
			audit.Event(c, 0, agentID, "agent.pair.complete", "agent:"+agentID, false)
			common.ErrorResp(c, err, 500)
			return
		}
	} else {
		if err := db.UpdateAgent(target); err != nil {
			pairThrottle.Fail(ip)
			audit.Event(c, 0, agentID, "agent.pair.complete", "agent:"+agentID, false)
			common.ErrorResp(c, err, 500)
			return
		}
	}

	// design R1: deliver this server's ed25519 public key so the agent can
	// pin it for the later reverse-connection wave. Omitted (empty) if boot
	// never wired a server identity — the pairing itself is unaffected.
	var serverPubKey string
	if si := agentsrv.CurrentServerIdentity(); si != nil {
		serverPubKey = si.PubKeyBase64()
	}

	pairThrottle.Reset(ip)
	audit.Event(c, 0, agentID, "agent.pair.complete", "agent:"+agentID, true)
	common.SuccessResp(c, PairAgentCompleteResp{AgentID: agentID, ServerVersion: conf.Version, SAS: sas, ServerPubKey: serverPubKey})
}

// AgentHeartbeatReq is the wire body for POST /agent/heartbeat.
type AgentHeartbeatReq struct {
	AgentID   string `json:"agent_id" binding:"required"`
	Ts        int64  `json:"ts" binding:"required"`
	Nonce     string `json:"nonce" binding:"required"`
	Signature string `json:"signature" binding:"required"`
}

// AgentHeartbeatResp is the response to an accepted heartbeat. Task is nil
// (omitted entirely from the JSON) unless this agent has a task to work on
// right now — design W12-3a: see the doc above AgentHeartbeat's dispatch
// step for how it gets populated.
type AgentHeartbeatResp struct {
	OK      bool                   `json:"ok"`
	Enabled bool                   `json:"enabled"`
	Task    *AgentHeartbeatTaskDTO `json:"task,omitempty"`
}

// AgentHeartbeatTaskDTO is the task-dispatch payload embedded in a heartbeat
// response: everything the agent needs to start (or resume) the task,
// including a freshly minted capability token scoping it to exactly this
// task's src(read)/dst(write) for at most agentsrv.MaxTaskCapTTL.
type AgentHeartbeatTaskDTO struct {
	TaskID   string `json:"task_id"`
	Type     string `json:"type"`
	SrcPath  string `json:"src_path"`
	DstDir   string `json:"dst_dir"`
	CapToken string `json:"cap_token"`
}

// AgentHeartbeat authenticates and records one liveness proof from a
// previously paired agent. Unlike every other endpoint in this codebase it
// carries NO session/bearer token: the agent proves who it is by signing
// proto.HeartbeatMessage(agent_id, ts, nonce) with the long-term private
// key that corresponds to the public key it registered at pairing time.
// See this file's top-level doc comment for the full replay-defense
// reasoning (freshness window + seen-nonce cache).
// @Summary Agent heartbeat
// @Description Anonymous but self-authenticated by an ed25519 signature over agent_id|ts|nonce. Rejects a stale ts, a replayed nonce, an unknown agent, or a disabled agent.
// @Tags agent
// @Accept json
// @Param body body AgentHeartbeatReq true "signed heartbeat"
// @Success 200 {object} common.Resp[AgentHeartbeatResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/agent/heartbeat [post]
func AgentHeartbeat(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req AgentHeartbeatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	// Freshness FIRST — this check needs no database and no crypto, so a flood
	// of stale/future-dated heartbeats (the cheapest to forge, since the ts is
	// attacker-chosen and unsigned-until-verified) is rejected before we spend
	// an indexed DB lookup or an ed25519 verify on it. A heartbeat "from the
	// future" is as suspicious as a stale one, so the window is two-sided.
	delta := time.Now().Unix() - req.Ts
	if delta < 0 {
		delta = -delta
	}
	if time.Duration(delta)*time.Second > heartbeatFreshness {
		audit.Event(c, 0, req.AgentID, "agent.heartbeat", "agent:"+req.AgentID, false)
		common.ErrorResp(c, errs.StaleHeartbeat, 401)
		return
	}

	a, err := db.GetAgentByAgentID(req.AgentID)
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		audit.Event(c, 0, req.AgentID, "agent.heartbeat", "agent:"+req.AgentID, false)
		common.ErrorResp(c, err, code)
		return
	}

	pub, err := base64.StdEncoding.DecodeString(a.PubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		// The stored pubkey came from our own PairAgentComplete, which
		// already validated it — this branch is a defensive backstop, not
		// an expected path.
		common.ErrorResp(c, errs.InvalidAgentPubKey, 500)
		return
	}
	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), proto.HeartbeatMessage(req.AgentID, req.Ts, req.Nonce), sig) {
		audit.Event(c, 0, a.AgentID, "agent.heartbeat", "agent:"+a.AgentID, false)
		common.ErrorResp(c, errs.InvalidHeartbeatSignature, 401)
		return
	}

	// Replay defense, second leg: the signature just proved this request
	// really came from the agent's private key, but that alone doesn't
	// stop a captured, still-fresh request from being resent verbatim by
	// an eavesdropper. A nonce already seen for this agent is refused.
	seenKey := req.AgentID + ":" + req.Nonce
	if _, ok := heartbeatSeenNonces.Get(seenKey); ok {
		audit.Event(c, 0, a.AgentID, "agent.heartbeat", "agent:"+a.AgentID, false)
		common.ErrorResp(c, errs.ReplayedHeartbeat, 401)
		return
	}
	heartbeatSeenNonces.Set(seenKey, true, cache.WithEx[bool](heartbeatNonceCacheTTL))

	if !a.Enabled {
		audit.Event(c, 0, a.AgentID, "agent.heartbeat", "agent:"+a.AgentID, false)
		common.ErrorResp(c, errs.AgentDisabled, 403)
		return
	}

	// Dispatch BEFORE persisting this heartbeat's own LastSeen (design
	// W12-4 (B)/(A) interaction — deliberate ordering, do not swap): dispatch
	// funnels through db.ClaimPendingForAgent, which lazily sweeps stale
	// agents' abandoned tasks back to pending before claiming (see that
	// function's doc). If THIS agent itself is the one recovering from a
	// long outage — its own previous claimed/running task orphaned by a
	// crash, no other heartbeat having swept it in the meantime — the sweep
	// needs to see THIS agent's LastSeen as it still stands in the DB from
	// BEFORE this heartbeat, i.e. genuinely stale, to requeue-then-let-this-
	// same-call-reclaim it. Persisting now's LastSeen first would freshen
	// the row an instant too soon, and the sweep would never fire for the
	// very reconnect that should trigger it — the agent's own stuck task
	// would then depend on some OTHER agent's heartbeat happening to sweep
	// it first. Ordering dispatch first closes that gap: this single
	// heartbeat request both revives its own abandoned task AND reports
	// fresh liveness, atomically from the caller's perspective.
	task := dispatchAgentTask(c, a)

	now := time.Now()
	a.LastSeen = &now
	if err := db.UpdateAgent(a); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	audit.Event(c, 0, a.AgentID, "agent.heartbeat", "agent:"+a.AgentID, true)
	common.SuccessResp(c, AgentHeartbeatResp{OK: true, Enabled: a.Enabled, Task: task})
}

// dispatchAgentTask is the heartbeat's task-dispatch step (design W12-3a):
// first try to atomically claim a fresh pending task (db.ClaimPendingForAgent
// — single-flight, see that function's doc: a reconnect racing another
// in-flight heartbeat for the same agent can never double-claim the same
// row), and if there was none, fall back to whatever task is already
// claimed/running for this agent (db.GetActiveAgentTaskForAgent) so a
// reconnecting agent gets handed its in-flight task again rather than
// nothing. Either way, a FRESH capability token is minted for every
// heartbeat response — re-issuing on every heartbeat while a task is active
// is deliberate (see agentsrv.IssueTaskCap's doc): it keeps the token's
// remaining lifetime pinned to "at least until the next heartbeat" without
// ever needing a token to outlive one heartbeat interval.
//
// Returns nil (no task field in the response at all, via omitempty) if
// there is nothing to dispatch, or if minting the capability token failed
// (fails closed: an agent is never handed a task with no way to prove it
// may act on it — the task stays claimed/pending for a later heartbeat to
// retry).
func dispatchAgentTask(c *gin.Context, a *model.Agent) *AgentHeartbeatTaskDTO {
	task, err := db.ClaimPendingForAgent(a.AgentID)
	if err != nil {
		log.Errorf("agent heartbeat: claim pending task for %s: %+v", a.AgentID, err)
		return nil
	}
	if task == nil {
		task, err = db.GetActiveAgentTaskForAgent(a.AgentID)
		if err != nil {
			log.Errorf("agent heartbeat: load active task for %s: %+v", a.AgentID, err)
			return nil
		}
	}
	if task == nil {
		return nil
	}

	capToken, err := agentsrv.IssueTaskCap(a.AgentID, task.TaskID, task.SrcPath, task.DstDir, agentsrv.MaxTaskCapTTL)
	if err != nil {
		log.Errorf("agent heartbeat: issue task cap for task %s: %+v", task.TaskID, err)
		audit.Event(c, 0, a.AgentID, "agent.task.dispatch", "task:"+task.TaskID, false)
		return nil
	}

	audit.Event(c, 0, a.AgentID, "agent.task.dispatch", "task:"+task.TaskID, true)
	return &AgentHeartbeatTaskDTO{
		TaskID:   task.TaskID,
		Type:     task.Type,
		SrcPath:  task.SrcPath,
		DstDir:   task.DstDir,
		CapToken: capToken,
	}
}
