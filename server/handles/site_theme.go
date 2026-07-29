package handles

import (
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/alist-org/alist/v3/server/static"
	"github.com/gin-gonic/gin"
)

// SetSiteThemeReq is the request body for the dedicated site_theme save
// endpoint (SetSiteTheme, below). Value is the raw Theme descriptor JSON —
// the exact string the generic settings endpoint would otherwise carry as a
// SettingItem's Value for the site_theme key.
type SetSiteThemeReq struct {
	Value string `json:"value" binding:"required"`
	// JSConfirmed must be explicitly true when Value carries a non-blank "js"
	// field — see the handler doc comment. Defaulting to Go's zero value
	// (false) means an ordinary client that simply forgets this field is
	// refused, not silently granted, which is the safe default for a flag
	// that authorizes site-wide script execution.
	JSConfirmed bool `json:"js_confirmed"`
}

// SetSiteTheme is the ONLY endpoint that can persist a conf.SiteTheme value
// carrying custom JS (server/handles/setting.go's generic SaveSettings
// refuses any such value outright, unconditionally — see its doc comment).
//
// SECURITY (HIGH, red-team fix): the js-confirmation gate used to exist ONLY
// in alist-v4-web's SiteThemeCard dialog — a purely client-side UI
// convention. This repo's RBAC supports a role scoped to ONLY
// setting:manage (RequireSystem, delegable — NOT full admin), which could
// otherwise call the generic bulk-settings endpoint directly (a single curl)
// and run arbitrary JS with full page privileges for EVERY visitor, anonymous
// visitors and OTHER ADMINS included, with zero server-side review — the
// "an admin with API access could run code anyway" reasoning the original
// implementation comment relied on does not hold for a non-admin role. Two
// independent layers close that gap:
//
//  1. This endpoint is gated on middlewares.RequireAdmin — the non-delegable
//     authorization control (see router_v4.go's adminV4 doc comment on that
//     distinction) — NOT RequireSystem("setting", "manage"). Site-wide code
//     execution for every visitor is deliberately held to the highest
//     privilege tier this app has, matching how role/permission mutation
//     itself (roles.POST, permissions.POST, ...) is RequireAdmin-only even
//     though "view" reads on those same resources are the delegable scope.
//  2. A request whose Value carries js MUST also set `js_confirmed: true`
//     explicitly, or this refuses with 400 and a readable reason — so even a
//     caller that bypasses alist-v4-web's SiteThemeCard entirely (a direct
//     API call) has to affirmatively assert "I have reviewed and confirm
//     this", not merely hold the right role. A request with NO js in Value
//     needs no confirmation (matches the generic endpoint's behavior for a
//     js-free site_theme, which is unaffected by any of this).
//
// Every call — the admin-only refusal, the js_confirmed refusal, and success
// — is written to the audit trail (internal/audit), recording the acting
// user.
//
// callerIsAdmin (permission.go) is checked here IN ADDITION to the router's
// middlewares.RequireAdmin — the same handler-layer defense-in-depth pattern
// CreatePermission/UpdatePermission/DeletePermission already use for their
// own system-admin-only gate, so this endpoint's admin requirement is
// verifiable by calling the handler directly (as this package's tests do for
// those), not only by trusting the router wiring.
// @Summary Set the site-wide default theme (js-carrying themes only go through here)
// @Description The ONLY endpoint that can save a conf.SiteTheme value carrying custom JS. Requires full admin (not the delegable setting:manage scope) and an explicit js_confirmed:true in the body when the value carries a non-blank js field. Writes an audit event. A js-free value is accepted unconditionally, identically to the generic settings endpoint.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body SetSiteThemeReq true "site theme value + confirmation"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid body, js present without js_confirmed:true, or a setting-value validation failure (bad JSON / oversized)"
// @Failure 403 {object} common.Resp[any] "caller is not a full admin"
// @Router /v4/admin/settings/site-theme [post]
func SetSiteTheme(c *gin.Context) {
	var req SetSiteThemeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)

	if !callerIsAdmin(c) {
		audit.Event(c, user.ID, user.Username, "settings.site_theme.set", "site_theme:not_admin", false)
		common.ErrorI18n(c, "setting.site_theme_admin_only", 403)
		return
	}

	carriesJS := op.SiteThemeCarriesJS(req.Value)
	if carriesJS && !req.JSConfirmed {
		audit.Event(c, user.ID, user.Username, "settings.site_theme.set", "site_theme:js_unconfirmed", false)
		common.ErrorI18n(c, "setting.site_theme_js_confirmation_required", 400)
		return
	}

	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		// Not found is unexpected (the key is always seeded — see
		// internal/bootstrap/data/setting.go) but handled defensively rather
		// than failing the save outright: build a fresh row matching the
		// seeded shape (PUBLIC/STYLE) so the setting still ends up usable.
		item = &model.SettingItem{Key: conf.SiteTheme, Type: conf.TypeText, Group: model.STYLE}
	}
	item.Value = req.Value

	if err := op.SaveSettingItem(item); err != nil {
		audit.Event(c, user.ID, user.Username, "settings.site_theme.set", "site_theme:save_failed", false)
		if op.IsSettingValidationError(err) {
			common.ErrorResp(c, err, 400)
		} else {
			common.ErrorResp(c, err, 500)
		}
		return
	}

	target := "site_theme:cleared"
	if req.Value != "" {
		target = "site_theme:set"
		if carriesJS {
			target = "site_theme:set_with_js"
		}
	}
	audit.Event(c, user.ID, user.Username, "settings.site_theme.set", target, true)
	common.SuccessResp(c)
	static.UpdateIndex()
}
