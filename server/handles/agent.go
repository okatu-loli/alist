package handles

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Remote-agent admin API (design W12-1: data-layer + admin-endpoint
// foundation only — no channel/SAS/task machinery this wave). Every handler
// here is admin-gated (mounted under adminV4, see server/router_v4.go) and
// audited, mirroring backup.go's shape: fetch the caller off ctxkey.User,
// audit.Event on both the success and failure path with a stable action
// name, respond through common.SuccessResp/ErrorResp.
//
// AgentDTO deliberately excludes PubKey (already `json:"-"` on the model —
// this DTO is the belt to that suspenders, same reasoning as AppPasswordDTO
// excluding Hash/Salt): an admin manages an agent's name/enabled/whitelist,
// never its authentication key material.

// agentInstallHint is a short, static pointer at how an operator actually
// joins an agent to this instance once they have a pairing code. It carries
// no secrets (the code itself is a separate response field) — the real
// `alist agent join` CLI and its channel land in W12-2.
const agentInstallHint = "On the agent host, run: alist agent join --code <code> --server <this server's URL>"

// PairingStartResp is the response to a freshly started pairing session.
type PairingStartResp struct {
	Code        string    `json:"code"`
	ExpiresAt   time.Time `json:"expires_at"`
	InstallHint string    `json:"install_hint"`
}

// StartAgentPairing mints a new pairing session (internal/agentsrv.PairingStore)
// and returns its code, expiry and a static install hint. This wave never
// creates a model.Agent row here — that happens when an agent actually
// connects and completes pairing (W12-2).
// @Summary Start agent pairing
// @Description Mints a high-entropy, 15-minute pairing code an operator hands to a not-yet-paired agent. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[PairingStartResp]
// @Router /v4/admin/agents/pairing [post]
func StartAgentPairing(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	code, expiresAt := agentsrv.Start()
	audit.Event(c, user.ID, user.Username, "agent.pairing.start", "agent:pairing", true)
	common.SuccessResp(c, PairingStartResp{
		Code:        code,
		ExpiresAt:   expiresAt,
		InstallHint: agentInstallHint,
	})
}

// AgentDTO is the wire form of a paired agent: enough to identify and manage
// it, never its authentication key material (PubKey). Confirmed reflects
// whether this agent has ever passed the SAS-matched admin confirm (design
// W12-2b, see ConfirmAgent) — distinct from Enabled, which an admin can also
// flip directly via UpdateAgent as an administrative override.
type AgentDTO struct {
	ID               uint       `json:"id"`
	AgentID          string     `json:"agent_id"`
	Name             string     `json:"name"`
	Enabled          bool       `json:"enabled"`
	Confirmed        bool       `json:"confirmed"`
	StorageWhitelist []string   `json:"storage_whitelist"`
	LastSeen         *time.Time `json:"last_seen,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	// Mode is "outbound" (default) or "reverse" (design R4). ReverseAddr is the
	// dialable WS URL, meaningful only in reverse mode.
	Mode        string `json:"mode"`
	ReverseAddr string `json:"reverse_addr,omitempty"`
	// ReverseStatus is the live reverse-connection status the server's reverse
	// supervisor tracks for this agent (connected/dialing/error + last-seen over
	// the channel). Nil for an outbound agent, or a reverse agent the supervisor
	// is not yet tracking.
	ReverseStatus *ReverseStatus `json:"reverse_status,omitempty"`
}

func toAgentDTO(a model.Agent) AgentDTO {
	dto := AgentDTO{
		ID:               a.ID,
		AgentID:          a.AgentID,
		Name:             a.Name,
		Enabled:          a.Enabled,
		Confirmed:        a.Confirmed,
		StorageWhitelist: a.Whitelist(),
		LastSeen:         a.LastSeen,
		CreatedAt:        a.CreatedAt,
		Mode:             agentModeOrDefault(a.Mode),
		ReverseAddr:      a.ReverseAddr,
	}
	if a.IsReverse() {
		if st := reverseStatusFor(a.AgentID); st.State != "" {
			dto.ReverseStatus = &st
		}
	}
	return dto
}

// agentModeOrDefault normalizes a stored mode: an empty column is outbound.
func agentModeOrDefault(mode string) string {
	if mode == "" {
		return model.AgentModeOutbound
	}
	return mode
}

// validReverseAddr accepts only a well-formed absolute ws:// or wss:// URL with
// a host — the exact shape the reverse supervisor can hand to a WebSocket
// dialer. Anything else (http scheme, no host, junk) is rejected so a bad
// address can never be persisted and then silently fail to dial.
func validReverseAddr(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return false
	}
	return u.Host != ""
}

// ListAgents returns every paired agent. No pubkey, ever (see AgentDTO doc).
// @Summary List agents
// @Description Lists every paired remote agent. Never includes key material. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]AgentDTO]
// @Router /v4/admin/agents [get]
func ListAgents(c *gin.Context) {
	agents, err := db.ListAgents()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dtos := make([]AgentDTO, 0, len(agents))
	for _, a := range agents {
		dtos = append(dtos, toAgentDTO(a))
	}
	common.SuccessResp(c, dtos)
}

// UpdateAgentReq is the PATCH body: every field is optional (pointer), so an
// omitted field leaves the stored value untouched — the same partial-update
// contract as the labels/settings admin resources.
type UpdateAgentReq struct {
	Name             *string   `json:"name"`
	Enabled          *bool     `json:"enabled"`
	StorageWhitelist *[]string `json:"storage_whitelist"`
	// Mode / ReverseAddr configure reverse-connection mode (design R4). Mode
	// must be "outbound" or "reverse"; when the RESULTING mode is reverse the
	// resulting ReverseAddr must be a well-formed ws/wss URL, else the PATCH is
	// rejected — a reverse agent with no dialable address is never persisted.
	Mode        *string `json:"mode"`
	ReverseAddr *string `json:"reverse_addr"`
}

// UpdateAgent partially updates an agent's name, enabled flag and/or storage
// whitelist. An unknown id is a 404 (errs.AgentNotFound).
// @Summary Update an agent
// @Description Partially updates a paired agent's name, enabled flag and/or storage whitelist. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param id path int true "agent id"
// @Param body body UpdateAgentReq true "fields to update"
// @Success 200 {object} common.Resp[AgentDTO]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/agents/{id} [patch]
func UpdateAgent(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	var req UpdateAgentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	target := fmt.Sprintf("agent:%d", id)

	a, err := db.GetAgent(uint(id))
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		audit.Event(c, user.ID, user.Username, "agent.update", target, false)
		common.ErrorResp(c, err, code)
		return
	}
	if req.Name != nil {
		a.Name = *req.Name
	}
	if req.Enabled != nil {
		a.Enabled = *req.Enabled
	}
	if req.StorageWhitelist != nil {
		a.SetWhitelist(*req.StorageWhitelist)
	}
	if req.Mode != nil {
		mode := *req.Mode
		if mode == "" {
			mode = model.AgentModeOutbound
		}
		if mode != model.AgentModeOutbound && mode != model.AgentModeReverse {
			audit.Event(c, user.ID, user.Username, "agent.update", target, false)
			common.ErrorResp(c, fmt.Errorf("invalid mode %q: must be %q or %q", *req.Mode, model.AgentModeOutbound, model.AgentModeReverse), 400)
			return
		}
		a.Mode = mode
	}
	if req.ReverseAddr != nil {
		a.ReverseAddr = *req.ReverseAddr
	}
	// Validate the RESULTING state: a reverse agent must carry a well-formed
	// ws/wss address. This runs on the merged row, so setting mode=reverse
	// without (ever) an address, or with a junk address, is rejected — a
	// reverse agent the supervisor cannot dial is never persisted.
	if a.IsReverse() {
		if a.ReverseAddr == "" {
			audit.Event(c, user.ID, user.Username, "agent.update", target, false)
			common.ErrorResp(c, errors.New("reverse mode requires a reverse_addr (ws:// or wss:// URL)"), 400)
			return
		}
		if !validReverseAddr(a.ReverseAddr) {
			audit.Event(c, user.ID, user.Username, "agent.update", target, false)
			common.ErrorResp(c, fmt.Errorf("malformed reverse_addr %q: must be an absolute ws:// or wss:// URL", a.ReverseAddr), 400)
			return
		}
	}
	if err := db.UpdateAgent(a); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.update", target, false)
		common.ErrorResp(c, err, 500)
		return
	}
	// Nudge the reverse supervisor so a mode/addr/enabled change re-dials (or
	// drops) this agent promptly rather than waiting for the periodic
	// re-enumerate. No-op when no supervisor is running.
	signalReverseSupervisorRefresh()
	audit.Event(c, user.ID, user.Username, "agent.update", target, true)
	common.SuccessResp(c, toAgentDTO(*a))
}

// AgentReverseStatus returns the live reverse-connection status the server's
// reverse supervisor tracks for one agent (design R4): connected / dialing /
// error, plus the last time it was seen over the channel. A 400 if reverse mode
// is unavailable this run (no server identity), a 404 for an unknown agent.
// @Summary Get an agent's reverse-connection status
// @Description Returns the live reverse-channel connection status for a reverse-mode agent. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param id path int true "agent id"
// @Success 200 {object} common.Resp[ReverseStatus]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/agents/{id}/reverse-status [get]
func AgentReverseStatus(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	a, err := db.GetAgent(uint(id))
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		common.ErrorResp(c, err, code)
		return
	}
	if getReverseSupervisor() == nil {
		common.ErrorResp(c, errNoReverseSupervisor, 400)
		return
	}
	common.SuccessResp(c, reverseStatusFor(a.AgentID))
}

// DeleteAgent removes a paired agent by id.
// @Summary Remove an agent
// @Description Removes a paired agent. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param id path int true "agent id"
// @Success 200 {object} common.Resp[any]
// @Router /v4/admin/agents/{id} [delete]
func DeleteAgent(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	target := fmt.Sprintf("agent:%d", id)

	if err := db.DeleteAgent(uint(id)); err != nil {
		audit.Event(c, user.ID, user.Username, "agent.remove", target, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "agent.remove", target, true)
	common.SuccessResp(c)
}

// AgentPairingSASResp is the response to GET /admin/agents/:id/pairing-sas:
// the SAS this server derived for the agent's most recent (re-)pairing
// attempt, and whether it has already been confirmed. SAS is empty once
// Confirmed is true (ConfirmAgent clears PendingSAS) — there is nothing
// left pending to compare.
type AgentPairingSASResp struct {
	SAS       []string `json:"sas"`
	Confirmed bool     `json:"confirmed"`
}

// AgentPairingSAS returns the SAS an admin compares, eyes-on, against
// whatever the agent's own terminal printed at `alist agent join` time
// (design W12-2b — see internal/agent/sas.go's top-of-file doc for exactly
// what a match/mismatch here proves). This is the server's OWN
// independently-derived value, read straight off the agent row's
// PendingSAS (set by PairAgentComplete from whatever pubkey THIS server
// actually received) — never anything relayed through the agent, which is
// precisely the channel a pubkey-substitution attacker controls.
// @Summary Get an agent's pending pairing SAS
// @Description Returns the SAS this server derived for the agent's current pairing attempt, for the admin to visually match against the agent's own terminal before confirming. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param id path int true "agent id"
// @Success 200 {object} common.Resp[AgentPairingSASResp]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/agents/{id}/pairing-sas [get]
func AgentPairingSAS(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	a, err := db.GetAgent(uint(id))
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		common.ErrorResp(c, err, code)
		return
	}
	common.SuccessResp(c, AgentPairingSASResp{SAS: a.PendingSASList(), Confirmed: a.Confirmed})
}

// ConfirmAgent is the SAS-gated affirmative path that actually activates a
// freshly (re-)paired agent (design W12-2b): the admin has visually matched
// this agent's SAS (AgentPairingSAS) against what the agent's own terminal
// showed, and is now asserting "yes, this is the device I meant to pair,
// not an impostor". Only after this does the agent's heartbeat stop being
// rejected with errs.AgentDisabled. Sets Confirmed=true and Enabled=true,
// and clears PendingSAS (nothing left pending to compare). Idempotent: an
// already-confirmed agent's second confirm is a no-op that still reports
// success — a UI need not special-case "already confirmed" as an error.
//
// This is deliberately a SEPARATE endpoint from PATCH /admin/agents/:id
// {enabled:true}, which still works and still flips Enabled — that PATCH
// path is kept as an explicit administrative override (e.g. re-enabling an
// agent an admin disabled themselves, no SAS involved), never a substitute
// for the SAS-matched first confirmation of a newly (re-)paired agent.
// @Summary Confirm a paired agent's SAS
// @Description Marks an agent's pairing as SAS-confirmed and enables it. Idempotent. Admin only.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param id path int true "agent id"
// @Success 200 {object} common.Resp[AgentDTO]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/agents/{id}/confirm [post]
func ConfirmAgent(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	target := fmt.Sprintf("agent:%d", id)

	a, err := db.GetAgent(uint(id))
	if err != nil {
		code := 500
		if errors.Is(err, errs.AgentNotFound) {
			code = 404
		}
		audit.Event(c, user.ID, user.Username, "agent.confirm", target, false)
		common.ErrorResp(c, err, code)
		return
	}
	if !a.Confirmed || !a.Enabled || a.PendingSAS != "" {
		a.Confirmed = true
		a.Enabled = true
		a.SetPendingSAS(nil)
		if err := db.UpdateAgent(a); err != nil {
			audit.Event(c, user.ID, user.Username, "agent.confirm", target, false)
			common.ErrorResp(c, err, 500)
			return
		}
	}
	audit.Event(c, user.ID, user.Username, "agent.confirm", target, true)
	common.SuccessResp(c, toAgentDTO(*a))
}
