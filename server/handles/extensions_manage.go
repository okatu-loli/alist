package handles

import (
	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// This file is the admin-facing "extension manager" surface (design §2.5/§3): a
// single list that unifies the host-registered built-in extensions (labels/SSO/
// passkey feature bundles, the FRP background service) with the installed disk
// plugins, plus enable/disable endpoints for the built-ins. It sits beside
// plugins.go (the disk-plugin lifecycle) on purpose — the two data sources have
// different shapes (a built-in has no version/uninstall) and different mutation
// paths, so the built-in enable/disable endpoints here ONLY accept built-ins
// (extension.EnableBuiltin/DisableBuiltin reject a disk-plugin or unknown id),
// and the frontend routes a disk plugin to the existing /admin/plugins endpoints.

// BuiltinExtensionDTO is the wire shape of one host-registered built-in in the
// extension manager list. It carries only what the manager UI renders: no
// version and no uninstall (a built-in is not installed from disk and cannot be
// removed, only enabled/disabled). Builtin is always true — the field exists so
// the frontend can render the "built-in" badge and route enable/disable to the
// built-in endpoints rather than the disk-plugin ones.
type BuiltinExtensionDTO struct {
	ID        string            `json:"id"`
	Name      map[string]string `json:"name"` // i18n display name (Manifest.Name)
	Kind      string            `json:"kind"` // primary registry Kind
	Enabled   bool              `json:"enabled"`
	Builtin   bool              `json:"builtin"`    // always true for this DTO
	HasStatus bool              `json:"has_status"` // renders a status card (BackgroundService)
	// DisableWarning is the declarative disable-consequence copy (Manifest.
	// DisableWarning, i18n LangMap). When non-empty the frontend shows a
	// "are you sure?" confirm carrying this message before disabling — replacing
	// the old hardcoded CONFIRM_ON_DISABLE_IDS set. Absent for the common case
	// where disabling has no special consequence.
	DisableWarning map[string]string `json:"disable_warning,omitempty"`
}

// ManagedExtensionsResp is the unified extension-manager list: the built-in
// section (host-registered, enable/disable-able, not uninstallable) and the disk
// plugin section (the existing PluginState lifecycle records). The frontend
// merges the two into one page but keeps them distinct so it can gate the UI
// affordances (uninstall/version only for plugins; "built-in" badge for builtins)
// and route mutations to the right endpoint family.
type ManagedExtensionsResp struct {
	Builtins []BuiltinExtensionDTO   `json:"builtins"`
	Plugins  []extension.PluginState `json:"plugins"`
}

// ListManagedExtensions returns the unified extension list: every host-registered
// built-in (including the ones an admin has disabled — they must still be listed
// so they can be re-enabled) plus every installed disk plugin.
// @Summary List managed extensions
// @Description Returns the unified extension-manager list: host-registered built-ins (labels/SSO/passkey/FRP…), listed even while disabled so they can be re-enabled, plus installed disk plugins. Admin only.
// @Tags admin_extension
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[ManagedExtensionsResp]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/extensions [get]
func ListManagedExtensions(c *gin.Context) {
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	entries := extension.DefaultRegistry.BuiltinEntries()
	builtins := make([]BuiltinExtensionDTO, 0, len(entries))
	for _, e := range entries {
		builtins = append(builtins, BuiltinExtensionDTO{
			ID:      e.ID(),
			Name:    e.Manifest.Name,
			Kind:    string(e.Kind),
			Enabled: e.Enabled(),
			Builtin: true,
			// DeclaresKind (Manifest truth source), not HasKind: a built-in
			// that declares BackgroundService alongside another primary
			// Kind would never get an extraKinds(BackgroundService) entry
			// (extraKinds is populated solely by FeatureBundle expansion
			// onto APIRoute/EventHook/ScheduledJob/FileDecorator, never
			// BackgroundService) and so would wrongly render without a
			// status card despite declaring the service.
			HasStatus:      e.DeclaresKind(ext.KindBackgroundService),
			DisableWarning: e.Manifest.DisableWarning,
		})
	}
	common.SuccessResp(c, ManagedExtensionsResp{
		Builtins: builtins,
		Plugins:  mgr.List(),
	})
}

// EnableBuiltinExtension turns a built-in extension on.
// @Summary Enable built-in extension
// @Description Enables a host-registered built-in extension (its contributions re-aggregate; a BackgroundService restarts) and clears it from the persisted disabled set. Rejects a disk-plugin or unknown id — those go through /admin/plugins. Admin only.
// @Tags admin_extension
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "extension id"
// @Success 200 {object} common.Resp[string]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/extensions/{id}/enable [post]
func EnableBuiltinExtension(c *gin.Context) {
	builtinStateChange(c, "extension.builtin.enable", extension.EnableBuiltin)
}

// DisableBuiltinExtension turns a built-in extension off.
// @Summary Disable built-in extension
// @Description Disables a host-registered built-in extension (its contributions stop aggregating; a BackgroundService is stopped) and records it in the persisted disabled set so the choice survives a restart. Rejects a disk-plugin or unknown id — those go through /admin/plugins. Admin only.
// @Tags admin_extension
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "extension id"
// @Success 200 {object} common.Resp[string]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/extensions/{id}/disable [post]
func DisableBuiltinExtension(c *gin.Context) {
	builtinStateChange(c, "extension.builtin.disable", extension.DisableBuiltin)
}

// builtinStateChange is the shared enable/disable body for built-ins: it calls
// fn(id) (EnableBuiltin/DisableBuiltin, which validate the id is a registered
// built-in and reject anything else), audits both outcomes, and maps a rejected
// id to 400 — mirroring plugins.go's pluginStateChange convention so the two
// endpoint families behave consistently.
func builtinStateChange(c *gin.Context, action string, fn func(string) error) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	if err := fn(id); err != nil {
		audit.Event(c, user.ID, user.Username, action, "extension:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	audit.Event(c, user.ID, user.Username, action, "extension:"+id, true)
	common.SuccessResp(c, "ok")
}
