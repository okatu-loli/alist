package handles

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/authn/totp"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// This file is the login STEP-UP path: the second-factor step a password (or
// a federated SSO) login drives into once the FIRST factor already succeeded.
// It is deliberately separate from the generic AuthNMethod login endpoints
// (authn.go's Begin/Finish), which refuse a second_factor method outright —
// "it runs after a password succeeds, never as a standalone login". This IS
// that after-a-password path: loginHash (auth.go) and completeSSOLogin
// (sso.go) call resolveSecondFactor/beginSecondFactorStepUp the moment their
// first factor succeeds for a user who has a second factor bound.
//
// TOTP (Phase C) was the FIRST, and today the ONLY, wired second factor:
// resolveSecondFactor hardcodes the "has OtpSecret" binding check, and
// issueSecondFactorPrincipal hardcodes TOTP's principal-minting call. Both are
// the seam a future second-factor method (WebAuthn, a third-party plugin) would
// extend with its own user-binding table and principal mint — the dispatch
// (GetAuthNMethod(methodID).Begin/.Finish) downstream of them is already
// generic (Phase F generalization: TOTP was the first, no-longer-only caller).
//
// Reusing the SAME extension-registry seam every other login method goes
// through (extension.DefaultRegistry.GetAuthNMethod) means disabling a bound
// second factor's Feature entry (internal/bootstrap/core_features.go for
// builtin.totp) makes resolveSecondFactor refuse — the login orchestrator then
// degrades to "no second factor" for every user, even one who still has the
// factor bound (this is a deliberate design choice: disabling the built-in
// must fully turn 2FA off for the login path, not just hide the self-service
// management UI, so an admin has a real emergency kill switch if the TOTP flow
// itself is broken).
//
// The generic completion endpoint (POST /auth/method/:id/stepup/finish,
// authn.go's AuthNMethodStepUpFinish) is what ANY second_factor method's
// challenge is completed through; /auth/login/2fa below is now a TOTP-specific
// compatibility alias that calls into that SAME dispatch (finishSecondFactor).

// totpBuiltinMethodID is a same-package alias for totp.BuiltinMethodID so
// auth.go's loginHash can reference it without importing internal/authn/totp
// directly.
const totpBuiltinMethodID = totp.BuiltinMethodID

// stepUpThrottle rate-limits every second-factor step-up completion path per
// client IP — /auth/login/2fa AND the generic /auth/method/:id/stepup/finish
// share this ONE counter (they are the same dispatch under two routes), kept
// separate from passwordThrottle/authnThrottle/refreshThrottle so a
// code-guessing run here neither interferes with nor benefits from another
// path's counter.
var stepUpThrottle = newLoginThrottle()

// totpMethodEnabled reports whether the builtin.totp Feature entry is
// registered AND enabled — the same "manageable built-in" gate
// authNMethodEntryDisabled applies for builtin.passkey, checked directly here
// since the login orchestrator needs the answer BEFORE it has a challenge id
// to gate on.
func totpMethodEnabled() bool {
	e, ok := extension.DefaultRegistry.Get(totp.BuiltinMethodID)
	return ok && e.Enabled()
}

// resolveSecondFactor reports which second-factor AuthNMethod (if any) user
// must complete before a session is minted, and that method's id. TOTP
// (OtpSecret non-empty + builtin.totp enabled) is the only bound second
// factor today; a future second-factor method would extend this same
// function with its own user-binding table (e.g. a bound-WebAuthn-credential
// lookup) rather than each call site growing its own dispatch. A disabled
// builtin.totp degrades every user to "no second factor" — this is the one
// place that decision is made.
//
// Red-team I5 decision (adjudicated: enforce, not exempt): this same check
// gates the SSO login callback too (sso.go's completeSSOLogin), not just the
// password path. A verified external IdP identity is NOT treated as an
// independent MFA signal that could exempt the account from its own TOTP —
// User.SsoID is a self-service, user-writable field (see SSOLinkBegin in
// sso_account.go), so "this account has a linked SSO identity" carries no
// host-verified guarantee about what the IdP itself required at its end. A
// TOTP-bound user must clear this gate regardless of which login method
// proved their identity first.
func resolveSecondFactor(user *model.User) (methodID string, ok bool) {
	if user.OtpSecret != "" && totpMethodEnabled() {
		return totp.BuiltinMethodID, true
	}
	return "", false
}

// issueSecondFactorPrincipal mints the Begin principal for user against the
// second-factor method methodID resolved. Principal-minting is
// method-specific (each second-factor method has its own notion of "the
// already-identified user's handle"), so this is a small per-method dispatch
// table rather than a generic call — today TOTP is the only entry; a future
// second-factor method would add its own case here.
func issueSecondFactorPrincipal(methodID string, user *model.User) ext.Principal {
	if methodID == totp.BuiltinMethodID {
		return ext.Principal(totp.Default.IssuePrincipal(user))
	}
	return ""
}

// beginSecondFactorStepUp mints a login-step-up principal for user's already
// first-factor-proven identity and drives the resolved methodID's AuthNMethod
// Begin, returning the challenge id the client submits back to complete it
// (either the TOTP-specific /auth/login/2fa alias or the generic
// /auth/method/:id/stepup/finish).
func beginSecondFactorStepUp(ctx context.Context, methodID string, user *model.User) (challengeID string, err error) {
	method, release, err := extension.DefaultRegistry.GetAuthNMethod(methodID)
	if err != nil {
		return "", err
	}
	defer release()
	principal := issueSecondFactorPrincipal(methodID, user)
	challenge, err := method.Begin(ctx, principal, ext.AuthNRequest{})
	if err != nil {
		return "", err
	}
	return challenge.ChallengeID, nil
}

// LoginTOTPStepUpReq is the body of the step-up completion request.
type LoginTOTPStepUpReq struct {
	ChallengeID string `json:"challenge_id" binding:"required"`
	Code        string `json:"code" binding:"required"`
	// Remember, if set, issues a longer-lived refresh token — mirrors
	// LoginReq.Remember (this replaces the login the password step parked).
	Remember bool `json:"remember"`
}

// LoginTOTPStepUp completes a TOTP login step-up begun by loginHash and, on
// success, mints a core session token — the SAME issuer every other login
// path uses (issueAuthNSession/mintAuthNSessionTokens, authn.go). It is now a
// TOTP-specific compatibility alias: it reshapes its body into the generic
// second-factor request and dispatches through the SAME finishSecondFactor
// core every second_factor AuthNMethod's step-up completes through (authn.go's
// AuthNMethodStepUpFinish is the generic route onto that same core).
// @Summary Complete a TOTP login step-up
// @Description Verifies a login step-up challenge's code (a current TOTP code or an unused recovery code) and, on success, issues a session token. Anonymous by necessity (no session exists yet) — the challenge id, minted only after a correct password, is the access control. Compatibility alias for POST /auth/method/builtin.totp/stepup/finish.
// @Tags auth
// @Accept json
// @Produce json
// @Param body body LoginTOTPStepUpReq true "challenge id + code"
// @Success 200 {object} common.Resp[gin.H] "data: {token, refresh_token, device_key}"
// @Failure 400 {object} common.Resp[any] "wrong/expired code or challenge"
// @Failure 403 {object} common.Resp[any] "resolved user disabled since the challenge was minted"
// @Failure 429 {object} common.Resp[any] "too many failed attempts"
// @Router /auth/login/2fa [post]
// @Router /v4/auth/login/2fa [post]
func LoginTOTPStepUp(c *gin.Context) {
	var req LoginTOTPStepUpReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	finishSecondFactor(c, totp.BuiltinMethodID, req.ChallengeID, map[string]string{"code": req.Code}, nil, req.Remember)
}
