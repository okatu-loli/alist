package handles

import (
	"errors"
	"fmt"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/authn/totp"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// This file is the AUTHENTICATED TOTP self-management surface (Phase C:
// internal TOTP second factor): a logged-in user begins/confirms an
// enrollment, unbinds it, and regenerates their recovery codes. It shares
// currentManageableUser with passkey.go (same guest-refusing, fresh-reload
// helper) and reuses the process-wide totp.Default service so a setup begun
// here can be confirmed here, and later exercised by the login step-up path
// (server/handles/auth.go) against the SAME confirmed secret.
//
// Deliberately separate from the anonymous login step-up endpoint
// (/auth/login/2fa): binding a NEW secret to an account must stay
// authenticated (it can only ever target the caller's own account), while
// the login endpoint only ever verifies a secret that is already bound.

// totpSelfThrottle rate-limits the self-service confirm/unbind/regenerate
// surface (red-team I4), keyed by USER ID rather than client IP — unlike the
// anonymous login paths, every caller here already carries a valid session,
// so IP is the wrong dimension (a shared NAT/office IP must not lock out
// unrelated users, and a single attacker with many source IPs but one stolen
// session must still be capped). Its own counter, independent of
// passwordThrottle/totpStepUpThrottle/authnThrottle/refreshThrottle.
var totpSelfThrottle = newLoginThrottle()

// totpSelfThrottleKey scopes the shared totpSelfThrottle counter to one user.
func totpSelfThrottleKey(userID uint) string {
	return fmt.Sprintf("user:%d", userID)
}

// TOTPStatusResp is the current user's TOTP enrollment state: whether it is
// bound, and how many one-time recovery codes remain unused (never the
// codes/secret themselves — see /me/2fa/totp/begin's response for the only
// place the secret round-trips, and /confirm|/recovery-codes/regenerate's
// response for the only place recovery-code plaintexts round-trip).
type TOTPStatusResp struct {
	Enabled                bool `json:"enabled"`
	RecoveryCodesRemaining int  `json:"recovery_codes_remaining"`
	RecoveryCodesTotal     int  `json:"recovery_codes_total"`
}

// TOTPStatus reports whether the current user has TOTP bound and how many
// recovery codes they have left (M10: the account page needs this to render
// "TOTP enabled, 3 of 10 recovery codes left" without ever re-fetching secret
// or code material).
// @Summary Get my TOTP status
// @Description Reports whether the authenticated user has TOTP bound and how many one-time recovery codes remain unused. Not available to the guest user.
// @Tags auth
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[TOTPStatusResp]
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/me/2fa/totp/status [get]
func TOTPStatus(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	resp := TOTPStatusResp{Enabled: user.OtpSecret != ""}
	if resp.Enabled {
		total, remaining, err := op.RecoveryCodeStatus(user.ID)
		if err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		resp.RecoveryCodesTotal = total
		resp.RecoveryCodesRemaining = remaining
	}
	common.SuccessResp(c, resp)
}

// TOTPBeginResp is the pending enrollment the client renders as a QR code
// (OtpAuthURI) with Secret shown as a manual-entry fallback.
type TOTPBeginResp struct {
	Secret     string `json:"secret"`
	OtpAuthURI string `json:"otpauth_uri"`
}

// TOTPBegin starts a new TOTP enrollment for the current user.
// @Summary Begin TOTP enrollment
// @Description Mints a fresh TOTP secret pending confirmation and returns it plus an otpauth:// URI for a QR code. Refuses if the account already has a confirmed TOTP secret (unbind first). Not available to the guest user.
// @Tags auth
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[TOTPBeginResp]
// @Failure 400 {object} common.Resp[any] "totp already enabled"
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/me/2fa/totp/begin [post]
func TOTPBegin(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	secret, uri, err := totp.Default.BeginSetup(user)
	if err != nil {
		if errors.Is(err, totp.ErrAlreadyEnabled) {
			common.ErrorI18n(c, "auth.totp_already_enabled", 400)
			return
		}
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, TOTPBeginResp{Secret: secret, OtpAuthURI: uri})
}

// TOTPConfirmReq carries the code the user typed back from their
// authenticator app to prove they captured the pending secret.
type TOTPConfirmReq struct {
	Code string `json:"code" binding:"required"`
}

// TOTPConfirmResp is the freshly-minted, one-time-displayed recovery code
// batch — the only time these plaintexts are ever returned.
type TOTPConfirmResp struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

// TOTPConfirm verifies a pending enrollment's code and, on success, persists
// the secret and mints a fresh recovery-code batch.
// @Summary Confirm TOTP enrollment
// @Description Verifies the current code against the pending secret begun via /me/2fa/totp/begin; on success persists OtpSecret and mints 10 one-time recovery codes (returned exactly once). Not available to the guest user.
// @Tags auth
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body TOTPConfirmReq true "current authenticator code"
// @Success 200 {object} common.Resp[TOTPConfirmResp]
// @Failure 400 {object} common.Resp[any] "wrong code / no pending setup / already enabled"
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/me/2fa/totp/confirm [post]
func TOTPConfirm(c *gin.Context) {
	var req TOTPConfirmReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	throttleKey := totpSelfThrottleKey(user.ID)
	if totpSelfThrottle.Blocked(throttleKey) {
		common.ErrorI18n(c, "auth.too_many_login_attempts", 429)
		return
	}
	secret, err := totp.Default.ConfirmSetup(user, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, totp.ErrAlreadyEnabled):
			common.ErrorI18n(c, "auth.totp_already_enabled", 400)
		case errors.Is(err, totp.ErrSetupNotFound):
			common.ErrorI18n(c, "auth.totp_setup_not_found", 400)
		case errors.Is(err, totp.ErrInvalidCode):
			totpSelfThrottle.Fail(throttleKey)
			audit.Event(c, user.ID, user.Username, "account.totp.enable", "totp", false)
			common.ErrorI18n(c, "auth.invalid_2fa_code", 400)
		default:
			common.ErrorResp(c, err, 500, true)
		}
		return
	}

	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	codes, err := op.GenerateRecoveryCodes(user.ID)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	totpSelfThrottle.Reset(throttleKey)
	audit.Event(c, user.ID, user.Username, "account.totp.enable", "totp", true)
	common.SuccessResp(c, TOTPConfirmResp{RecoveryCodes: codes})
}

// TOTPDeleteReq carries the proof of possession required to unbind: either
// the current TOTP/recovery code, or the account password.
type TOTPDeleteReq struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

// TOTPDelete unbinds the current user's TOTP second factor, clearing
// OtpSecret and every recovery code.
// @Summary Unbind TOTP
// @Description Removes the authenticated user's TOTP second factor after verifying either their current code (TOTP or recovery) or their account password. Clears every recovery code too. Not available to the guest user.
// @Tags auth
// @Produce json
// @Security ApiKeyAuth
// @Param body body TOTPDeleteReq true "current code or password"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "totp not enabled / wrong proof"
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/me/2fa/totp [delete]
func TOTPDelete(c *gin.Context) {
	var req TOTPDeleteReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	if user.OtpSecret == "" {
		common.ErrorI18n(c, "auth.totp_not_enabled", 400)
		return
	}
	throttleKey := totpSelfThrottleKey(user.ID)
	if totpSelfThrottle.Blocked(throttleKey) {
		common.ErrorI18n(c, "auth.too_many_login_attempts", 429)
		return
	}

	// Red-team M11: EITHER credential verifying is sufficient — a wrong
	// password no longer short-circuits a code the caller also supplied (the
	// previous if/else-if only ever checked ONE of the two, silently ignoring
	// the other if present). This matches the doc's stated contract ("current
	// code (TOTP or recovery), OR their account password") and the docs
	// published in phC-report.md.
	verified := false
	if req.Password != "" && user.ValidateRawPassword(req.Password) == nil {
		verified = true
	}
	if !verified && req.Code != "" && totp.Default.VerifyBoundCode(user, req.Code) {
		verified = true
	}
	if !verified {
		totpSelfThrottle.Fail(throttleKey)
		audit.Event(c, user.ID, user.Username, "account.totp.disable", "totp", false)
		common.ErrorI18n(c, "auth.invalid_2fa_code", 400)
		return
	}

	if err := op.Cancel2FAByUser(user); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if err := op.DeleteRecoveryCodes(user.ID); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	totpSelfThrottle.Reset(throttleKey)
	audit.Event(c, user.ID, user.Username, "account.totp.disable", "totp", true)
	common.SuccessResp(c)
}

// TOTPRegenerateRecoveryCodesResp is the freshly-minted recovery-code batch.
type TOTPRegenerateRecoveryCodesResp struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

// TOTPRegenerateRecoveryCodes replaces the current user's recovery-code batch
// with a fresh one — every previously-issued code (used or not) is
// invalidated.
// @Summary Regenerate TOTP recovery codes
// @Description Mints a fresh batch of 10 one-time recovery codes for the authenticated user, invalidating every previously-issued code. Requires an existing TOTP enrollment. Not available to the guest user.
// @Tags auth
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[TOTPRegenerateRecoveryCodesResp]
// @Failure 400 {object} common.Resp[any] "totp not enabled"
// @Failure 403 {object} common.Resp[any] "guest user"
// @Router /v4/me/2fa/totp/recovery-codes/regenerate [post]
func TOTPRegenerateRecoveryCodes(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	if user.OtpSecret == "" {
		common.ErrorI18n(c, "auth.totp_not_enabled", 400)
		return
	}
	// Shares totpSelfThrottle's per-user counter with confirm/delete (red-team
	// I4): regenerate takes no credential of its own to fail on, but a burst
	// of failed confirm/delete attempts against this account also blocks it —
	// an attacker mid brute-force cannot pivot to nuking the victim's recovery
	// codes as a side channel.
	if totpSelfThrottle.Blocked(totpSelfThrottleKey(user.ID)) {
		common.ErrorI18n(c, "auth.too_many_login_attempts", 429)
		return
	}
	codes, err := op.GenerateRecoveryCodes(user.ID)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	audit.Event(c, user.ID, user.Username, "account.totp.recovery_codes.regenerate", "totp", true)
	common.SuccessResp(c, TOTPRegenerateRecoveryCodesResp{RecoveryCodes: codes})
}
