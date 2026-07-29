package handles

import (
	"strconv"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/alist-org/alist/v3/server/static"
	"github.com/gin-gonic/gin"
)

func getRoleOptions() string {
	roles, _, err := op.GetRoles(1, model.MaxInt)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		if r.Name == "admin" || r.Name == "guest" {
			continue
		}
		names = append(names, r.Name)
	}
	return strings.Join(names, ",")
}

// offlineDownloadToolBuiltinOptions are the 4 always-available choices for the
// offline_download_tool setting (the built-in direct path plus the 3 built-in
// daemon extensions), matching internal/bootstrap/data/setting.go's seeded
// default Options exactly. getOfflineDownloadToolOptions appends any
// currently-enabled Kind=OfflineDownloadTool gRPC plugin's self-reported name
// after these, so a third-party tool plugin shows up in the admin UI's select
// without a static Options edit (Phase E ring 9).
var offlineDownloadToolBuiltinOptions = []string{"SimpleHttp", "aria2", "qBittorrent", "Transmission"}

func getOfflineDownloadToolOptions() string {
	opts := make([]string, 0, len(offlineDownloadToolBuiltinOptions))
	opts = append(opts, offlineDownloadToolBuiltinOptions...)
	opts = append(opts, extension.OfflineDownloadToolGRPCNames(extension.DefaultRegistry)...)
	return strings.Join(opts, ",")
}

// searchIndexBuiltinOptions are the 4 always-available choices for the
// search_index setting (the index-less fallback plus the 3 built-in real-index
// backends), matching internal/bootstrap/data/setting.go's seeded default
// Options exactly. getSearchIndexOptions appends any currently-enabled
// Kind=SearchIndexer gRPC plugin's self-reported name after these, so a
// third-party index backend plugin shows up in the admin UI's select without a
// static Options edit (Phase E ring 1b).
var searchIndexBuiltinOptions = []string{"none", "database_non_full_text", "bleve", "meilisearch"}

func getSearchIndexOptions() string {
	opts := make([]string, 0, len(searchIndexBuiltinOptions))
	opts = append(opts, searchIndexBuiltinOptions...)
	opts = append(opts, extension.SearchIndexerGRPCNames(extension.DefaultRegistry)...)
	return strings.Join(opts, ",")
}

type SetTokenReq struct {
	Token string `json:"token" form:"token" binding:"required"`
	// Reveal opts into storing the token in plaintext (readable back via
	// GetToken). Defaults to false: the token is stored as a salted,
	// irreversible hash (see conf.TokenPlaintext / pkg/utils.HashToken) and
	// only ever returned in the response of this call.
	Reveal bool `json:"reveal" form:"reveal"`
}

// ResetTokenReq carries the optional reveal flag for ResetToken. It is bound
// from either a JSON body ({"reveal":true}) or a query/form value
// (?reveal=true) — both are accepted since ResetToken has historically taken
// no body at all; c.ShouldBind picks the right source from Content-Type and
// leaves Reveal at its zero value (false) when neither is present.
type ResetTokenReq struct {
	Reveal bool `json:"reveal" form:"reveal"`
}

// saveTokenSetting persists the admin API token, storing it either as
// plaintext or as a salted irreversible hash depending on reveal, and
// records the active mode in conf.TokenPlaintext so GetToken and the auth
// middleware know how to read it back (storage model: see
// pkg/utils.HashToken).
func saveTokenSetting(token string, reveal bool) error {
	value := token
	if !reveal {
		hashed, err := utils.HashToken(token)
		if err != nil {
			return err
		}
		value = hashed
	}
	tokenItem := model.SettingItem{Key: conf.Token, Value: value, Type: conf.TypeString, Group: model.SINGLE, Flag: model.PRIVATE}
	if err := op.SaveSettingItem(&tokenItem); err != nil {
		return err
	}
	plaintextItem := model.SettingItem{Key: conf.TokenPlaintext, Value: strconv.FormatBool(reveal), Type: conf.TypeBool, Group: model.SINGLE, Flag: model.PRIVATE}
	return op.SaveSettingItem(&plaintextItem)
}

// ResetToken generates and persists a new random admin API token, replacing the previous one.
// @Summary Reset admin token
// @Description Generates a new random admin API token and saves it, replacing the previous one. By default the token is stored as a salted hash (irreversible); pass reveal=true to store it in plaintext instead. The plaintext value is always returned once in the response, regardless of storage mode.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param reveal query bool false "store the new token in plaintext (readable back via GET token) instead of hashed"
// @Success 200 {object} common.Resp[string] "data: new token (plaintext, shown once)"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/setting/reset_token [post]
// @Router /v4/admin/settings/token/reset [post]
func ResetToken(c *gin.Context) {
	var req ResetTokenReq
	_ = c.ShouldBind(&req) // reveal is optional; absent/unparsable body just leaves it false (hashed)
	token := random.Token()
	if err := saveTokenSetting(token, req.Reveal); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, token)
}

// SetToken sets the admin API token to a caller-supplied value.
// @Summary Set admin token
// @Description Sets the admin API token to a caller-supplied value, replacing the previous one. By default the token is stored as a salted hash (irreversible); pass reveal=true to store it in plaintext instead. The plaintext value is always returned once in the response, regardless of storage mode.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body SetTokenReq true "new token"
// @Success 200 {object} common.Resp[string] "data: new token (plaintext, shown once)"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/setting/set_token [post]
// @Router /v4/admin/settings/token [put]
func SetToken(c *gin.Context) {
	var req SetTokenReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := saveTokenSetting(req.Token, req.Reveal); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, req.Token)
}

// GetToken returns the current admin API token's metadata. In plaintext mode
// (conf.TokenPlaintext=true) it echoes the stored token back; in hash mode
// the stored value is a salted, irreversible digest, so the token field is
// withheld (empty) and plaintext:false tells the caller it can only be
// recovered by minting a new one via ResetToken/SetToken.
// @Summary Get admin token
// @Description Returns the current admin API token if it is stored in plaintext, or plaintext:false with an empty token if it is stored hashed.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any] "data: {token, plaintext}"
// @Router /admin/setting/token [get]
// @Router /v4/admin/settings/token [get]
func GetToken(c *gin.Context) {
	if setting.GetBool(conf.TokenPlaintext) {
		common.SuccessResp(c, gin.H{"token": setting.GetStr(conf.Token), "plaintext": true})
		return
	}
	common.SuccessResp(c, gin.H{"token": "", "plaintext": false})
}

// GetSetting returns a single setting by key, or multiple settings by a comma-separated keys list.
// @Summary Get setting(s)
// @Description Returns a single setting item by key, or multiple items by a comma-separated "keys" list. If neither is set, behavior depends on empty key/keys lookup. The default-role setting is enriched with role name and available role options; the offline_download_tool setting is enriched with the built-in tools plus any currently-enabled gRPC tool plugin names.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param key query string false "setting key (single lookup)"
// @Param keys query string false "comma-separated setting keys (batch lookup)"
// @Success 200 {object} common.Resp[model.SettingItem] "single item when key is set"
// @Failure 400 {object} common.Resp[any]
// @Router /admin/setting/get [get]
// @Param key path string true "setting key"
// @Router /v4/admin/settings/{key} [get]
func GetSetting(c *gin.Context) {
	key := pathOrQuery(c, "key")
	keys := c.Query("keys")
	if key != "" {
		item, err := op.GetSettingItemByKey(key)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		if item.Key == conf.DefaultRole {
			copy := *item
			copy.Options = getRoleOptions()
			if id, err := strconv.Atoi(copy.Value); err == nil {
				if r, err := op.GetRole(uint(id)); err == nil {
					copy.Value = r.Name
				}
			}
			common.SuccessResp(c, copy)
			return
		}
		if item.Key == conf.OfflineDownloadTool {
			copy := *item
			copy.Options = getOfflineDownloadToolOptions()
			common.SuccessResp(c, copy)
			return
		}
		if item.Key == conf.SearchIndex {
			copy := *item
			copy.Options = getSearchIndexOptions()
			common.SuccessResp(c, copy)
			return
		}
		common.SuccessResp(c, item)
	} else {
		items, err := op.GetSettingItemInKeys(strings.Split(keys, ","))
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		for i := range items {
			switch items[i].Key {
			case conf.DefaultRole:
				if id, err := strconv.Atoi(items[i].Value); err == nil {
					if r, err := op.GetRole(uint(id)); err == nil {
						items[i].Value = r.Name
					}
				}
				items[i].Options = getRoleOptions()
			case conf.OfflineDownloadTool:
				items[i].Options = getOfflineDownloadToolOptions()
			case conf.SearchIndex:
				items[i].Options = getSearchIndexOptions()
			}
		}
		common.SuccessResp(c, items)
	}
}

// SaveSettings upserts a batch of setting items.
// @Summary Save settings
// @Description Upserts a batch of setting items. If a default-role item is included, its value is resolved from a role name to a role id (admin/guest are rejected), and the site index is refreshed on success.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body []model.SettingItem true "setting items"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid body, unknown default role, admin/guest as default role, a site_theme value carrying js (use POST /v4/admin/settings/site-theme instead), or any other setting-value validation failure (e.g. malformed site_theme JSON)"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/setting/save [post]
// @Router /v4/admin/settings [put]
func SaveSettings(c *gin.Context) {
	var req []model.SettingItem
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	for i := range req {
		if req[i].Key == conf.DefaultRole {
			role, err := op.GetRoleByName(req[i].Value)
			if err != nil {
				common.ErrorResp(c, err, 400)
				return
			}
			if role.Name == "admin" || role.Name == "guest" {
				common.ErrorI18n(c, "setting.cannot_set_admin_or_guest_default_role", 400)
				return
			}
			req[i].Value = strconv.Itoa(int(role.ID))
		}
		// SECURITY (HIGH, red-team fix): this generic bulk-save endpoint is
		// reachable by a role scoped to ONLY setting:manage (RequireSystem,
		// delegable) — NOT necessarily full admin. A site_theme carrying js
		// runs with full page privileges for every visitor, anonymous
		// included, with no per-visitor consent step (see conf.SiteTheme's
		// doc comment) — so it must never be savable through this endpoint at
		// all, regardless of any confirmation flag a caller might attach. The
		// only path that can ever persist a js-carrying site_theme is the
		// dedicated, RequireAdmin-gated POST /admin/settings/site-theme
		// (SetSiteTheme, site_theme.go), which additionally requires an
		// explicit js_confirmed:true in ITS OWN request body.
		if req[i].Key == conf.SiteTheme && op.SiteThemeCarriesJS(req[i].Value) {
			common.ErrorStrResp(c, "site_theme carrying custom JS cannot be saved through this endpoint — use POST /admin/settings/site-theme (admin-only, requires explicit js_confirmed)", 400)
			return
		}
	}

	if err := op.SaveSettingItems(req); err != nil {
		// A setting-value validation failure (bad site_theme JSON, an invalid
		// default_role, ...) is a CLIENT input error, not a server fault — see
		// op.IsSettingValidationError's doc comment. Anything else (a genuine
		// db failure) still reports 500.
		if op.IsSettingValidationError(err) {
			common.ErrorResp(c, err, 400)
		} else {
			common.ErrorResp(c, err, 500)
		}
	} else {
		common.SuccessResp(c)
		static.UpdateIndex()
	}
}

// ListSettings returns all settings, or settings filtered by group(s).
// @Summary List settings
// @Description Returns all setting items, or those filtered by a single "group" or comma-separated "groups" list. The default-role setting, if present, is enriched with role name and available role options; the offline_download_tool setting, if present, is enriched with the built-in tools plus any currently-enabled gRPC tool plugin names.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param group query int false "setting group id"
// @Param groups query string false "comma-separated setting group ids"
// @Success 200 {object} common.Resp[[]model.SettingItem]
// @Failure 400 {object} common.Resp[any]
// @Router /admin/setting/list [get]
// @Router /v4/admin/settings [get]
func ListSettings(c *gin.Context) {
	groupStr := c.Query("group")
	groupsStr := c.Query("groups")
	var settings []model.SettingItem
	var err error
	if groupsStr == "" && groupStr == "" {
		settings, err = op.GetSettingItems()
	} else {
		var groupStrings []string
		if groupsStr != "" {
			groupStrings = strings.Split(groupsStr, ",")
		} else {
			groupStrings = append(groupStrings, groupStr)
		}
		var groups []int
		for _, str := range groupStrings {
			group, err := strconv.Atoi(str)
			if err != nil {
				common.ErrorResp(c, err, 400)
				return
			}
			groups = append(groups, group)
		}
		settings, err = op.GetSettingItemsInGroups(groups)
	}
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	for i := range settings {
		switch settings[i].Key {
		case conf.DefaultRole:
			if id, err := strconv.Atoi(settings[i].Value); err == nil {
				if r, err := op.GetRole(uint(id)); err == nil {
					settings[i].Value = r.Name
				}
			}
			settings[i].Options = getRoleOptions()
		case conf.OfflineDownloadTool:
			settings[i].Options = getOfflineDownloadToolOptions()
		case conf.SearchIndex:
			settings[i].Options = getSearchIndexOptions()
		}
	}
	common.SuccessResp(c, settings)
}

// DeleteSetting deletes a setting item by key.
// @Summary Delete setting
// @Description Deletes a setting item by key.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param key query string true "setting key"
// @Success 200 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/setting/delete [post]
// @Param key path string true "setting key"
// @Router /v4/admin/settings/{key} [delete]
func DeleteSetting(c *gin.Context) {
	key := pathOrQuery(c, "key")
	if err := op.DeleteSettingItemByKey(key); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// GetSetupStatus returns whether the first-run setup wizard has been
// completed (conf.SetupCompleted). Existing/upgraded instances default to
// true (never interrupted); a brand-new install seeds false until the
// wizard's final step calls CompleteSetup. Requires an authenticated admin —
// the wizard itself only runs post-login (see docs/reskin design §8), so no
// unauthenticated setup endpoint is introduced.
// @Summary Get setup status
// @Description Returns whether the first-run setup wizard has been completed.
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any] "data: {completed: bool}"
// @Router /v4/admin/settings/setup [get]
func GetSetupStatus(c *gin.Context) {
	item, err := op.GetSettingItemByKey(conf.SetupCompleted)
	if err != nil {
		// The row is always seeded by bootstrap before the server accepts any
		// request (internal/bootstrap/data/setting.go), so a missing row here
		// is not expected in practice. If it ever happens anyway, default to
		// "completed" — never force an already-running instance's admin into
		// the wizard just because the flag itself couldn't be read.
		common.SuccessResp(c, gin.H{"completed": true})
		return
	}
	common.SuccessResp(c, gin.H{"completed": item.Value == "true" || item.Value == "1"})
}

// CompleteSetup marks the first-run setup wizard as completed. Idempotent —
// calling it again once already completed is a no-op success. Upserts the
// setting directly (same pattern as saveTokenSetting above) rather than
// requiring the row to already exist, so it converges correctly even in the
// unexpected case where bootstrap has not seeded it yet.
// @Summary Complete setup
// @Description Marks the first-run setup wizard as completed (conf.SetupCompleted=true).
// @Tags admin-setting
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/settings/setup/complete [post]
func CompleteSetup(c *gin.Context) {
	item := model.SettingItem{Key: conf.SetupCompleted, Value: "true", Type: conf.TypeBool, Group: model.SITE, Flag: model.PRIVATE}
	if err := op.SaveSettingItem(&item); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// publicSettingsCacheControl is a short, revalidate-friendly cache lifetime
// for the anonymous, no-auth PublicSettings route. This endpoint is hit on
// every anonymous page load (theming spec §J4 made it noticeably heavier —
// it can now carry a full site_theme descriptor up to op.SiteThemeMaxBytes),
// has no ETag/If-Modified-Since support, and is never gzip-compressed (this
// codebase has no gzip/compression middleware wired at all, so introducing
// one just for this route would be a bigger, unrelated change). A short
// max-age gives browsers/CDNs a cheap revalidation window without materially
// delaying visibility of an admin's setting change — 60s is a small, bounded
// staleness window, not an indefinite one, and every value here is already
// public (nothing here needs the stronger "private"/"no-store" treatment
// down.go or thumbnail.go use for user-specific or heavier payloads).
const publicSettingsCacheControl = "public, max-age=60"

// PublicSettings returns the subset of settings safe to expose to unauthenticated clients.
// @Summary Public settings
// @Description Returns the settings flagged PUBLIC (e.g. site title, logo), no authentication required.
// @Tags public
// @Accept json
// @Produce json
// @Success 200 {object} common.Resp[map[string]string]
// @Router /public/settings [get]
// @Router /v4/public/settings [get]
func PublicSettings(c *gin.Context) {
	c.Header("Cache-Control", publicSettingsCacheControl)
	common.SuccessResp(c, op.GetPublicSettingsMap())
}
