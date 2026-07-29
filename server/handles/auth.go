package handles

import (
	"errors"
	"fmt"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"path"
	"strings"

	"github.com/alist-org/alist/v3/internal/device"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/session"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// passwordThrottle rate-limits the built-in password login path per client IP.
// See loginThrottle for the shared policy; the counter is kept separate from the
// AuthNMethod and refresh throttles so the three paths never lock one another.
var passwordThrottle = newLoginThrottle()

type LoginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password"`
	// Remember, if set, issues a longer-lived refresh token (see
	// common.RefreshTokenRememberFactor) instead of the default refresh TTL.
	Remember bool `json:"remember"`
}

// Login logs in with a plaintext password.
// @Summary Login (deprecated)
// @Description Deprecated: use LoginHash instead, which hashes the password on the client before sending it.
// @Tags auth
// @Accept json
// @Produce json
// @Param body body LoginReq true "username/password credentials"
// @Success 200 {object} common.Resp[gin.H] "data: {token, refresh_token, device_key}"
// @Failure 400 {object} common.Resp[any]
// @Failure 402 {object} common.Resp[any] "second factor required: data carries {need_second_factor, method_id, challenge_id} — complete it at POST /auth/login/2fa"
// @Failure 403 {object} common.Resp[any] "too many devices"
// @Failure 429 {object} common.Resp[any] "too many failed attempts"
// @Router /auth/login [post]
// @Router /v4/auth/login [post]
func Login(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Password = model.StaticHash(req.Password)
	loginHash(c, &req)
}

// LoginHash login with password hashed by sha256
// @Summary Login with hashed password
// @Description Login using a password pre-hashed by the client with SHA256 (see model.StaticHash), avoiding sending the plaintext password over the wire.
// @Tags auth
// @Accept json
// @Produce json
// @Param body body LoginReq true "username/hashed-password credentials"
// @Success 200 {object} common.Resp[gin.H] "data: {token, refresh_token, device_key}"
// @Failure 400 {object} common.Resp[any]
// @Failure 402 {object} common.Resp[any] "second factor required: data carries {need_second_factor, method_id, challenge_id} — complete it at POST /auth/login/2fa"
// @Failure 403 {object} common.Resp[any] "too many devices"
// @Failure 429 {object} common.Resp[any] "too many failed attempts"
// @Router /auth/login/hash [post]
// @Router /v4/auth/login/hash [post]
func LoginHash(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	loginHash(c, &req)
}

func loginHash(c *gin.Context, req *LoginReq) {
	// check count of login
	ip := c.ClientIP()
	if passwordThrottle.Blocked(ip) {
		common.ErrorI18n(c, "auth.too_many_login_attempts", 429)
		return
	}
	// check username
	user, err := op.GetUserByName(req.Username)
	if err != nil {
		audit.Event(c, 0, req.Username, "auth.login", "", false)
		common.ErrorI18n(c, "auth.invalid_credentials", 400)
		passwordThrottle.Fail(ip)
		return
	}
	// validate password hash
	if err := user.ValidatePwdStaticHash(req.Password); err != nil {
		audit.Event(c, user.ID, user.Username, "auth.login", "", false)
		common.ErrorI18n(c, "auth.invalid_credentials", 400)
		passwordThrottle.Fail(ip)
		return
	}
	// Second-factor step-up (Phase F generalization of Phase C's internal TOTP
	// second factor): the password alone is never enough for a user with a
	// bound second factor, but this handler no longer verifies the code/
	// response itself — that would bypass the owning built-in's enable/disable
	// Feature gate and skip the anti-replay/throttle policy the dedicated
	// step-up path (finishSecondFactor, authn.go) applies. Instead it mints a
	// challenge for whichever method resolveSecondFactor resolves and hands it
	// back UNTOKENED; the client completes it at POST /auth/login/2fa (a
	// TOTP-specific alias) or the generic POST /auth/method/:id/stepup/finish.
	// resolveSecondFactor itself checks the owning built-in is enabled, so
	// disabling it degrades every user straight through to a normal token mint
	// below, even one who still has the factor bound.
	if methodID, ok := resolveSecondFactor(user); ok {
		challengeID, err := beginSecondFactorStepUp(c.Request.Context(), methodID, user)
		if err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		// The password WAS correct — reset its throttle counter now. Wrong-code
		// brute-forcing against the challenge is metered separately by
		// stepUpThrottle on the step-up completion paths.
		passwordThrottle.Reset(ip)
		// Red-team M12: a password-correct-but-2FA-pending attempt is its own
		// distinct audit fact — worth its own action name (not "auth.login"
		// success=false, which would misleadingly read as a REJECTED
		// credential) so an operator can tell "wrong password" apart from
		// "right password, now needs its second factor" in the trail.
		audit.Event(c, user.ID, user.Username, "auth.login.second_factor_required", methodID, true)
		common.ErrorWithDataResp(c, i18n.Error(i18n.Lang(c), "auth.second_factor_required", nil), 402, gin.H{
			"need_second_factor": true,
			"method_id":          methodID,
			"challenge_id":       challengeID,
		})
		return
	}

	clientID := c.GetHeader("Client-Id")
	if clientID == "" {
		clientID = c.Query("client_id")
	}
	key := utils.GetMD5EncodeStr(fmt.Sprintf("%d-%s",
		user.ID, clientID))

	if err := device.EnsureActiveOnLogin(user.ID, key, c.Request.UserAgent(), c.ClientIP()); err != nil {
		if errors.Is(err, errs.TooManyDevices) {
			common.ErrorResp(c, err, 403)
		} else {
			common.ErrorResp(c, err, 400, true)
		}
		return
	}

	// generate token pair: short-lived access + longer-lived refresh (renewed
	// via /api/auth/refresh so the client doesn't need to re-login as often)
	token, err := common.GenerateToken(user, key)
	if err != nil {
		common.ErrorResp(c, err, 400, true)
		return
	}
	refreshToken, err := common.GenerateRefreshToken(user, key, req.Remember)
	if err != nil {
		common.ErrorResp(c, err, 400, true)
		return
	}
	audit.Event(c, user.ID, user.Username, "auth.login", "", true)
	common.SuccessResp(c, gin.H{"token": token, "refresh_token": refreshToken, "device_key": key})
	passwordThrottle.Reset(ip)
}

type UserResp struct {
	model.User
	Otp       bool     `json:"otp"`
	RoleNames []string `json:"role_names"`
	// Permissions is the legacy int32-bitmask view kept for the running
	// frontend; it cannot represent system-domain grants. Retire once the
	// frontend migrates to PermissionEntries (Phase 5.2).
	Permissions []model.PermissionEntry `json:"permissions"`
	// PermissionEntries is the typed view (fs + system) built from
	// op.UserPermissions; unknown actions pass through opaque (spec §9).
	PermissionEntries []PermissionEntryView `json:"permission_entries"`
}

// PermissionEntryView is one typed permission row as rendered by /me. fs rows
// carry Path + Recursive; system rows carry Domain (the row's Path is a domain
// selector). Actions are emitted verbatim so an action with no catalog
// descriptor still surfaces rather than being dropped.
type PermissionEntryView struct {
	ResourceType string        `json:"resource_type"`
	Path         string        `json:"path,omitempty"`
	Domain       string        `json:"domain,omitempty"`
	Actions      model.Actions `json:"actions"`
	Effect       string        `json:"effect"`
	Recursive    *bool         `json:"recursive,omitempty"`
}

// normalizePermEffect maps a stored effect onto the two display values. The
// policy engine treats any non-"deny" effect as an allow, so an empty effect
// renders as "allow" to match how it is actually enforced.
func normalizePermEffect(effect string) string {
	if effect == model.EffectDeny {
		return model.EffectDeny
	}
	return model.EffectAllow
}

// CurrentUser get current user by token
// if token is empty, return guest user
// @Summary Get current user
// @Description Returns the user resolved from the request's access token; returns the guest user if no token is present.
// @Tags auth
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[UserResp]
// @Failure 401 {object} common.Resp[any]
// @Router /me [get]
// @Router /v4/me [get]
func CurrentUser(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)

	userResp := UserResp{
		User: *user,
	}
	userResp.Password = ""
	if userResp.OtpSecret != "" {
		userResp.Otp = true
	}

	var roleNames []string
	permMap := map[string]int32{}
	paths := make([]string, 0)

	for _, role := range user.RolesDetail {
		roleNames = append(roleNames, role.Name)
		for _, entry := range role.PermissionScopes {
			cleanPath := path.Clean("/" + strings.TrimPrefix(entry.Path, "/"))
			if _, ok := permMap[cleanPath]; !ok {
				paths = append(paths, cleanPath)
			}
			permMap[cleanPath] |= entry.Permission
		}
	}
	userResp.RoleNames = roleNames

	for _, fullPath := range paths {
		userResp.Permissions = append(userResp.Permissions, model.PermissionEntry{
			Path:       fullPath,
			Permission: permMap[fullPath],
		})
	}

	// Typed view (fs + system) from the union of the user's role permissions.
	// This carries system-domain grants the legacy bitmask above cannot.
	entries, err := op.UserPermissions(user)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	for _, p := range entries {
		rt := p.ResourceType
		if rt == "" {
			rt = model.ResourceFS
		}
		actions := p.Actions
		if actions == nil {
			actions = model.Actions{}
		}
		view := PermissionEntryView{
			ResourceType: rt,
			Actions:      actions,
			Effect:       normalizePermEffect(p.Effect),
		}
		if rt == model.ResourceSystem {
			view.Domain = p.Path
		} else {
			view.Path = p.Path
			recursive := p.Recursive
			view.Recursive = &recursive
		}
		userResp.PermissionEntries = append(userResp.PermissionEntries, view)
	}

	common.SuccessResp(c, userResp)
}

// UpdateCurrent updates the current user's username, password and SSO id.
// @Summary Update current user
// @Description Updates the authenticated user's profile (username, password, SSO ID). Not allowed for the guest user.
// @Tags auth
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.User true "updated user fields (username/password/sso_id)"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "guest user cannot update profile"
// @Failure 500 {object} common.Resp[any]
// @Router /me/update [post]
// @Router /v4/me [put]
func UpdateCurrent(c *gin.Context) {
	var req model.User
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	if user.IsGuest() {
		common.ErrorI18n(c, "auth.guest_cannot_update_profile", 403)
		return
	}
	user.Username = req.Username
	if req.Password != "" {
		user.SetPassword(req.Password)
	}
	user.SsoID = req.SsoID
	if err := op.UpdateUser(user); err != nil {
		common.ErrorResp(c, err, 500)
	} else {
		common.SuccessResp(c)
	}
}

// LogOut invalidates the current access token and marks the device session inactive.
// @Summary Logout
// @Description Marks the caller's device session inactive and invalidates the presented access token.
// @Tags auth
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /auth/logout [get]
// @Router /v4/auth/logout [post]
func LogOut(c *gin.Context) {
	if keyVal, ok := c.Get(ctxkey.DeviceKey); ok {
		if err := session.MarkInactive(keyVal.(string)); err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		c.Set(ctxkey.SessionInactive, true)
	}
	err := common.InvalidateToken(c.GetHeader("Authorization"))
	if u, ok := c.Get(ctxkey.User); ok {
		user := u.(*model.User)
		audit.Event(c, user.ID, user.Username, "auth.logout", "", err == nil)
	}
	if err != nil {
		common.ErrorResp(c, err, 500)
	} else {
		common.SuccessResp(c)
	}
}
