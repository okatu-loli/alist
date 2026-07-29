package handles

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/alist-org/alist/v3/internal/authn/passkey"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
)

// This file is the AUTHENTICATED passkey self-management surface: a logged-in
// user registers, lists, and removes their OWN passkeys. It is deliberately
// separate from the anonymous login path (authn.go's AuthNMethod begin/finish,
// which the built-in passkey adapter drives): registration proves possession for
// an already-identified account, so it must never be reachable anonymously.
//
// Two invariants are load-bearing here:
//
//  1. Registration is authenticated. A passkey can only ever be added to the
//     CALLER's own account (the account in the request context) — there is no
//     path to add a credential to an arbitrary user, so "anonymously enroll a
//     passkey onto someone else's account" is unreachable by construction.
//
//  2. List and delete are self-scoped. Both read and mutate ONLY the caller's
//     own Authn column; a credential id in the URL selects among the caller's own
//     passkeys, never another user's, so one user can neither see nor delete
//     another's credential.

// PasskeyInfo is the non-secret view of a registered passkey returned to its
// owner. It deliberately omits the COSE public key and the sign counter — the
// account page only needs to identify and manage keys, not their crypto material.
type PasskeyInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AAGUID     string    `json:"aaguid,omitempty"`
	Transports []string  `json:"transports,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

func toPasskeyInfo(pk model.Passkey) PasskeyInfo {
	return PasskeyInfo{
		ID:         pk.ID,
		Name:       pk.Name,
		AAGUID:     pk.AAGUID,
		Transports: pk.Transports,
		CreatedAt:  pk.CreatedAt,
		LastUsedAt: pk.LastUsedAt,
	}
}

// currentManageableUser returns the request's authenticated user, refusing the
// guest. Passkeys belong to real accounts; the guest has none to manage. It
// reloads the user fresh from the store so the caller mutates the latest Authn
// column (avoiding a lost update against a context user cached earlier in the
// request lifecycle).
func currentManageableUser(c *gin.Context) (*model.User, bool) {
	ctxUser, ok := c.Get(ctxkey.User)
	if !ok {
		common.ErrorI18n(c, "auth.you_are_guest", 401)
		return nil, false
	}
	user := ctxUser.(*model.User)
	if user.IsGuest() {
		common.ErrorI18n(c, "auth.passkey_guest_forbidden", 403)
		return nil, false
	}
	fresh, err := op.GetUserById(user.ID)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return nil, false
	}
	return fresh, true
}

// PasskeyRegisterBeginResp is the challenge the client feeds to
// navigator.credentials.create.
type PasskeyRegisterBeginResp struct {
	ChallengeID string                       `json:"challenge_id"`
	Options     *protocol.CredentialCreation `json:"options"`
}

// PasskeyRegisterBegin starts registering a new passkey for the current user.
// @Summary Begin passkey registration
// @Description Starts a WebAuthn registration ceremony for the authenticated user; returns creation options for navigator.credentials.create. Not available to the guest user.
// @Tags auth
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[PasskeyRegisterBeginResp]
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/auth/passkey/register/begin [post]
func PasskeyRegisterBegin(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	challengeID, options, err := passkey.Default.BeginRegister(user, common.GetApiUrl(c.Request))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, PasskeyRegisterBeginResp{ChallengeID: challengeID, Options: options})
}

// PasskeyRegisterFinishReq carries the browser's attestation response back for
// verification, the challenge it answers, and the user-chosen label.
type PasskeyRegisterFinishReq struct {
	ChallengeID string `json:"challenge_id" binding:"required"`
	Name        string `json:"name"`
	// Response is the raw navigator.credentials.create() result JSON, passed
	// through verbatim to the WebAuthn engine.
	Response json.RawMessage `json:"response" binding:"required"`
}

// PasskeyRegisterFinish verifies the attestation and stores the new passkey.
// @Summary Finish passkey registration
// @Description Verifies the attestation for the current user's registration ceremony and appends the passkey to their account. Not available to the guest user.
// @Tags auth
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body PasskeyRegisterFinishReq true "attestation response"
// @Success 200 {object} common.Resp[PasskeyInfo]
// @Failure 400 {object} common.Resp[any] "verification failed / expired challenge"
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/auth/passkey/register/finish [post]
func PasskeyRegisterFinish(c *gin.Context) {
	var req PasskeyRegisterFinishReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	pk, err := passkey.Default.FinishRegister(user, req.ChallengeID, req.Response, req.Name)
	if err != nil {
		if errors.Is(err, passkey.ErrChallengeNotFound) {
			common.ErrorI18n(c, "auth.passkey_challenge_expired", 400)
			return
		}
		common.ErrorResp(c, err, 400)
		return
	}

	pks, err := model.ParsePasskeys(user.Authn)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	pks = append(pks, *pk)
	authn, err := model.MarshalPasskeys(pks)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	user.Authn = authn
	if err := op.UpdateUser(user); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, toPasskeyInfo(*pk))
}

// PasskeyList lists the current user's passkeys.
// @Summary List my passkeys
// @Description Lists the authenticated user's registered passkeys (name, created/last-used, no secret material). Not available to the guest user.
// @Tags auth
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]PasskeyInfo]
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/auth/passkey [get]
func PasskeyList(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	pks, err := model.ParsePasskeys(user.Authn)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	out := make([]PasskeyInfo, 0, len(pks))
	for _, pk := range pks {
		out = append(out, toPasskeyInfo(pk))
	}
	common.SuccessResp(c, out)
}

// PasskeyDelete removes one of the current user's passkeys by credential id.
// @Summary Delete one of my passkeys
// @Description Removes the named passkey from the authenticated user's own account only. Not available to the guest user.
// @Tags auth
// @Produce json
// @Security ApiKeyAuth
// @Param credid path string true "credential id (base64url) of the passkey to remove"
// @Success 200 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "guest user"
// @Failure 404 {object} common.Resp[any] "no such passkey on the caller's account"
// @Router /v4/auth/passkey/{credid} [delete]
func PasskeyDelete(c *gin.Context) {
	credID := c.Param("credid")
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	pks, err := model.ParsePasskeys(user.Authn)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	// Filter out the caller's OWN passkey matching credID. Because pks is the
	// caller's own Authn column, a credid can only ever select among the caller's
	// keys — never another user's.
	kept := make([]model.Passkey, 0, len(pks))
	for _, pk := range pks {
		if pk.ID != credID {
			kept = append(kept, pk)
		}
	}
	if len(kept) == len(pks) {
		common.ErrorI18n(c, "auth.passkey_not_found", 404)
		return
	}
	authn, err := model.MarshalPasskeys(kept)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	user.Authn = authn
	if err := op.UpdateUser(user); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}
