package handles

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"github.com/AlistGo/alist-agent/proto"
	"net/http"
	"net/url"
	"strings"
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

// Reverse-pairing admin endpoints (design subproject F, wave F3). This is the
// SERVER side of TOKEN-AUTHENTICATED first-contact reverse pairing, a TWO-STEP
// operator flow (the agent must already hold the token T before the server
// dials, so pairing is inherently two steps):
//
//	STEP 1 — ReverseMintPairingToken (POST .../pairing/reverse/token): mint a
//	  high-entropy one-time token T, stash it in the in-memory single-use+TTL
//	  store (reversePairTokens), and RETURN it so the operator can copy T out of
//	  band into the agent's pairing web (F2) and arm the agent.
//	STEP 2 — ReverseStartAgentPairing (POST .../pairing/reverse): after the agent
//	  is armed with T, the admin points the server at the agent's PUBLIC serve
//	  address plus the token_id from step 1. The server looks up T, DIALS the
//	  agent's /agentpair endpoint, runs the token pairing handshake
//	  (agentsrv.DialAndPairAgent), and on a passing MUTUAL tag verify persists an
//	  ENABLED reverse Agent row (auto-enable) and burns T.
//
// 🔒 SECURITY:
//   - Both endpoints trigger admin-gated, admin-typed control-plane actions
//     (mounted under the RequireAdmin /agents group) — the same trust model as
//     the offline-download daemon's admin-configured URLs. The OUTBOUND dial in
//     step 2 is bounded (agentsrv.pairingDialTimeout + per-step deadlines).
//   - The token TAG is the anti-MITM anchor: a non-agent endpoint fails the
//     handshake and NO row is created; a wrong-token peer fails the tag verify.
//     A passing mutual verify already authenticated the agent cryptographically,
//     so the row is auto-enabled with NO second human confirm (design spec
//     「服务器侧」: token verified ⇒ auto Enabled=true). The operator's affirmative
//     action was minting + copying T + arming the agent.
//   - T lives in memory only (never DB/disk), is single-use (burned on success),
//     and expires within reversePairTokenTTL. A failed dial does NOT burn it, so
//     the operator can retry within the TTL.
//   - Replay resistance is intrinsic to the token handshake (each tag binds two
//     fresh per-exchange nonces), so unlike the retired SP-E signed flow this
//     path needs no separate per-server seen-nonce cache.

// reversePairDial is the pairing-dial seam (design F). Production wires
// agentsrv.DialAndPairAgent; a handler test swaps in a fake returning canned
// (agentPub, agentID, fingerprint) so it need not stand up a real WS acceptor.
// Kept a package var (not a direct call) purely for that injection point.
var reversePairDial = agentsrv.DialAndPairAgent

const (
	// reversePairPath / reverseChannelPath mirror the agent's serve routes
	// (cmd/agent.go agentPairPath / agentChannelPath). The admin supplies the
	// agent's base serve address; the server derives the FIRST-CONTACT pairing
	// URL (reversePairPath, dialed now) and the DATA-channel URL
	// (reverseChannelPath, persisted as ReverseAddr for the R4 supervisor) by
	// REPLACING the path — never appending — so the derivation is deterministic
	// whether the admin typed a bare host, a trailing slash, or an existing path.
	reversePairPath    = "/agentpair"
	reverseChannelPath = "/agentchannel"
)

// reversePairingURL derives a concrete pairing/data URL from the admin-supplied
// base reverse address by REPLACING its path (and dropping any query/fragment).
// validReverseAddr has already guaranteed raw is an absolute ws/wss URL with a
// host, so this only fails on a pathological parse.
func reversePairingURL(raw, path string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// ReverseMintPairingTokenResp is the response to STEP 1 (mint). Token is the
// base32 one-time token the operator copies out of band into the agent's pairing
// web; TokenID is the opaque handle passed back to STEP 2 (the dial) so the
// server can look the token up; ExpiresAt is when the token is evicted (the
// frontend renders a countdown off it). The Token is a SECRET — the frontend
// treats it as sensitive (copy + clear), the server never persists it.
type ReverseMintPairingTokenResp struct {
	Token     string    `json:"token"`
	TokenID   string    `json:"token_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ReverseMintPairingToken mints a one-time reverse-pairing token (design F, wave
// F3, STEP 1). Admin-only. It mints a high-entropy token via the single-use+TTL
// store and returns {token, token_id, expires_at}. The operator copies `token`
// into the agent's pairing web to arm it, then calls ReverseStartAgentPairing
// with the returned token_id. A server identity is required (reverse pairing is
// impossible without the server's long-term key), so this fails early with a
// clear 400 rather than letting the operator arm an agent against a token the
// dial can never use.
// @Summary Mint a reverse-pairing token
// @Description Admin-only. Mints a high-entropy one-time token (10-min TTL, single-use) for the operator to copy into the agent's pairing web, and returns it with an opaque token_id used by the reverse-pairing dial.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[ReverseMintPairingTokenResp]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/agents/pairing/reverse/token [post]
func ReverseMintPairingToken(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)

	// Minting a token the dial could never use only misleads the operator, so
	// gate on the server identity here too (the dial re-checks it).
	if agentsrv.CurrentServerIdentity() == nil {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse.token", "agent:pairing", false)
		common.ErrorResp(c, errors.New("server identity not initialized: reverse pairing is unavailable (no server identity configured)"), 400)
		return
	}

	token, tokenID, expiresAt, err := reversePairTokens.mint()
	if err != nil {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse.token", "agent:pairing", false)
		common.ErrorResp(c, errors.New("failed to mint reverse-pairing token"), 500)
		return
	}

	audit.Event(c, user.ID, user.Username, "agent.pairing.reverse.token", "agent:pairing", true)
	common.SuccessResp(c, ReverseMintPairingTokenResp{Token: token, TokenID: tokenID, ExpiresAt: expiresAt})
}

// ReverseStartAgentPairingReq is the wire body for POST
// /admin/agents/pairing/reverse (STEP 2). ReverseAddr is the agent's PUBLIC base
// serve address (an absolute ws:// or wss:// URL with a host); the server
// derives the /agentpair and /agentchannel URLs from it. TokenID is the handle
// returned by STEP 1 (mint) — the server looks up the armed token T by it and
// keys the pairing handshake with T. Name is an optional label.
type ReverseStartAgentPairingReq struct {
	ReverseAddr string `json:"reverse_addr" binding:"required"`
	TokenID     string `json:"token_id" binding:"required"`
	Name        string `json:"name"`
}

// ReverseStartAgentPairingResp is the response to a successful reverse pairing
// dial: the created/updated row's id, the derived agent_id, and the
// NON-LOAD-BEARING server-key display fingerprint (offered so the operator can
// optionally eyeball the same value on both ends — the token tag already did the
// authenticating). The row is auto-enabled; there is no follow-up confirm step.
type ReverseStartAgentPairingResp struct {
	ID          uint   `json:"id"`
	AgentID     string `json:"agent_id"`
	Fingerprint string `json:"fingerprint"`
}

// ReverseStartAgentPairing dials an agent's first-contact pairing endpoint and,
// on a passing token mutual-verify, persists an AUTO-ENABLED reverse Agent row
// (design F, wave F3, STEP 2). The server must have a long-term identity wired
// (agentsrv.CurrentServerIdentity) to run the handshake; without one reverse
// pairing is unavailable this run (400). The armed token T is looked up by
// req.TokenID from the in-memory store — a missing/expired/already-used id is a
// 400 and never dials. On a passing verify the row lands Enabled=true /
// Confirmed=true (the token already authenticated the agent — no second human
// confirm), the token is BURNED (single-use), and the reverse supervisor is
// signalled so it dials the pinned-key data channel promptly. On any dial/verify
// failure NO row is created and the token is NOT burned (retryable within TTL).
// Mirrors PairAgentComplete's persist shape, plus Mode="reverse" + the
// /agentchannel ReverseAddr; PendingSAS is repurposed to hold the NON-LOAD-
// BEARING display fingerprint (Confirmed=true means nothing is pending to
// compare).
// @Summary Start reverse agent pairing
// @Description Admin-only. Looks up a token minted by .../pairing/reverse/token by token_id, dials the agent's public /agentpair endpoint, runs the token pairing handshake, and on a passing mutual verify creates an auto-enabled reverse agent row. The reverse_addr is the agent's base ws/wss serve address.
// @Tags admin_agent
// @Security ApiKeyAuth
// @Param body body ReverseStartAgentPairingReq true "reverse pairing target"
// @Success 200 {object} common.Resp[ReverseStartAgentPairingResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 502 {object} common.Resp[any]
// @Router /v4/admin/agents/pairing/reverse [post]
func ReverseStartAgentPairing(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	var req ReverseStartAgentPairingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	// A server identity is required to sign the pairing handshake. Without one
	// reverse pairing simply cannot happen this run (the agent has nothing to
	// pin, and we cannot prove key possession). Fail with a clear 400 rather
	// than a confusing dial error.
	si := agentsrv.CurrentServerIdentity()
	if si == nil {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		common.ErrorResp(c, errors.New("server identity not initialized: reverse pairing is unavailable (no server identity configured)"), 400)
		return
	}

	// Validate the admin-supplied base address up front (absolute ws/wss + host)
	// — reuse the exact validator the reverse-mode PATCH uses, so a junk address
	// can never reach the dialer.
	if !validReverseAddr(req.ReverseAddr) {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		common.ErrorResp(c, errors.New("malformed reverse_addr: must be an absolute ws:// or wss:// URL with a host"), 400)
		return
	}

	pairURL, err := reversePairingURL(req.ReverseAddr, reversePairPath)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		common.ErrorResp(c, err, 400)
		return
	}
	dataURL, err := reversePairingURL(req.ReverseAddr, reverseChannelPath)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		common.ErrorResp(c, err, 400)
		return
	}

	// The server's raw long-term public key (the base64 form is what an agent
	// pins; the handshake wants the raw 32 bytes). Decoding our OWN identity's
	// pubkey can only fail on catastrophic corruption.
	serverPub, err := base64.StdEncoding.DecodeString(si.PubKeyBase64())
	if err != nil || len(serverPub) != ed25519.PublicKeySize {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		common.ErrorResp(c, errors.New("server identity public key is malformed"), 500)
		return
	}

	// Look up the armed token T by the operator-supplied token_id (minted in STEP
	// 1). A missing / expired / already-used id is a client error (400) and must
	// never dial: without the RIGHT T the handshake cannot verify anyway. The
	// lookup is a PEEK — the token is only burned on a SUCCESSFUL pairing below,
	// so a failed attempt stays retryable within the TTL.
	tokenStr, ok := reversePairTokens.lookup(req.TokenID)
	if !ok {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		common.ErrorResp(c, errors.New("reverse-pairing token is unknown, expired, or already used (mint a fresh token and re-arm the agent)"), 400)
		return
	}
	tokenRaw, err := proto.CanonicalizeReversePairToken(tokenStr)
	if err != nil {
		// The store only ever holds tokens minted by GenerateReversePairToken, so
		// this can only fail on catastrophic corruption.
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		common.ErrorResp(c, errors.New("failed to prepare reverse-pairing token"), 500)
		return
	}

	// The OUTBOUND dial: bounded, admin-gated, protocol-guarded. A non-agent
	// endpoint / unreachable host / handshake or tag-verify failure returns an
	// error here and NO row is created.
	agentPub, agentID, fingerprint, err := reversePairDial(c.Request.Context(), pairURL, serverPub, tokenRaw)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:pairing", false)
		// 502: the failure is an unreachable agent / failed pairing handshake on
		// the admin-supplied upstream, not a malformed request.
		common.ErrorResp(c, errors.New("reverse pairing failed: could not reach or pair with the agent ("+err.Error()+")"), 502)
		return
	}

	// Persist the reverse row — mirror PairAgentComplete's shape exactly, plus
	// Mode="reverse" + the /agentchannel data URL. Create-or-update by agentID.
	target, err := db.GetAgentByAgentID(agentID)
	if err != nil && !errors.Is(err, errs.AgentNotFound) {
		audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:"+agentID, false)
		common.ErrorResp(c, err, 500)
		return
	}
	if target == nil {
		target = &model.Agent{AgentID: agentID}
		target.SetWhitelist(nil) // explicit: no storage access until an admin grants it
	}
	target.Name = req.Name
	target.PubKey = base64.StdEncoding.EncodeToString(agentPub)
	target.Mode = model.AgentModeReverse
	target.ReverseAddr = dataURL
	// AUTO-ENABLE (design spec「服务器侧」): the token mutual-verify that just
	// passed IS the authentication — a peer that produced a valid tagA holds the
	// same T over this exact transcript. There is NO second human confirm step in
	// the token model (the operator's affirmative action was minting + copying T +
	// arming the agent), so the row goes live immediately: Enabled=true so the R4
	// supervisor dials it, Confirmed=true so no "pending compare" state dangles.
	target.Enabled = true
	target.Confirmed = true
	// PendingSAS is repurposed here to carry the NON-LOAD-BEARING server-key
	// display fingerprint (the retired SP-E anti-MITM SAS is gone). Because
	// Confirmed=true, this is NOT a pending human-compare — it is only a value the
	// operator MAY eyeball; the fingerprint is also returned in the response.
	target.SetPendingSAS(strings.Fields(fingerprint))

	if target.ID == 0 {
		if err := db.CreateAgent(target); err != nil {
			audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:"+agentID, false)
			common.ErrorResp(c, err, 500)
			return
		}
	} else {
		if err := db.UpdateAgent(target); err != nil {
			audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:"+agentID, false)
			common.ErrorResp(c, err, 500)
			return
		}
	}

	// SINGLE-USE: the token has now paired an agent, so burn it — it can never be
	// reused. Burned only HERE, on success; a failed dial/verify above returned
	// early WITHOUT burning, so the operator can retry within the TTL.
	reversePairTokens.burn(req.TokenID)

	// Nudge the reverse supervisor so it re-enumerates promptly. The row is
	// already Enabled=true, so this brings the pinned-key data channel up now
	// rather than waiting for the periodic re-enumerate cycle.
	signalReverseSupervisorRefresh()

	audit.Event(c, user.ID, user.Username, "agent.pairing.reverse", "agent:"+agentID, true)
	common.SuccessResp(c, ReverseStartAgentPairingResp{ID: target.ID, AgentID: agentID, Fingerprint: fingerprint})
}
