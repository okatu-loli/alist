package handles

import (
	"encoding/json"

	"github.com/alist-org/alist/v3/internal/authn/sso"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// This file is the admin control plane for SSO providers plus the anonymous
// login-page listing. Two invariants shape it:
//
//  1. client_secret is WRITE-ONLY. A provider is never returned with its secret;
//     GET reports only whether one is set. On update, a blank secret in the body
//     means "keep the stored one" (the admin UI never had it to resubmit), so a
//     routine edit can never accidentally wipe the credential.
//  2. The public listing exposes only what a login button needs (id, key, name,
//     preset) for ENABLED providers — never issuer internals or secrets.

// InitSSO registers the configured providers as AuthNMethods at startup and
// arranges for the registration set to track later config changes. It is called
// once from server.Init after settings are loaded.
func InitSSO() {
	resyncSSOMethods()
	// Reflect out-of-band changes (backup restore, a generic settings save) into
	// the registered method set. The dedicated CRUD handlers also resync
	// explicitly; this is the backstop.
	op.RegisterSettingChangingCallback(func() { resyncSSOMethods() })
}

// ssoProviderView is the redacted, admin-facing shape of a provider: every field
// except the secret, plus a flag for whether a secret is configured.
type ssoProviderView struct {
	Key             string   `json:"key"`
	Name            string   `json:"name"`
	Preset          string   `json:"preset,omitempty"`
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	ClientSecretSet bool     `json:"client_secret_set"`
	Scopes          []string `json:"scopes,omitempty"`
	ClaimEmail      string   `json:"claim_email,omitempty"`
	ClaimUsername   string   `json:"claim_username,omitempty"`
	ClaimName       string   `json:"claim_name,omitempty"`
	Enabled         bool     `json:"enabled"`
	RedirectURL     string   `json:"redirect_url,omitempty"`
}

func toSSOProviderView(pc sso.ProviderConfig) ssoProviderView {
	return ssoProviderView{
		Key:             pc.Key,
		Name:            pc.Name,
		Preset:          pc.Preset,
		Issuer:          pc.Issuer,
		ClientID:        pc.ClientID,
		ClientSecretSet: pc.ClientSecret != "",
		Scopes:          pc.Scopes,
		ClaimEmail:      pc.ClaimEmail,
		ClaimUsername:   pc.ClaimUsername,
		ClaimName:       pc.ClaimName,
		Enabled:         pc.Enabled,
		RedirectURL:     pc.RedirectURL,
	}
}

// saveSSOProviders persists list and re-syncs the registered AuthNMethods so the
// login page and begin endpoint immediately reflect the change.
func saveSSOProviders(list []sso.ProviderConfig) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := op.SaveSettingItem(&model.SettingItem{
		Key: conf.SSOProviders, Value: string(b), Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE,
	}); err != nil {
		return err
	}
	resyncSSOMethods()
	return nil
}

// ListSSOProviders returns every configured provider, secrets redacted.
// @Router /v4/admin/sso/providers [get]
func ListSSOProviders(c *gin.Context) {
	list, err := loadSSOProviders()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	out := make([]ssoProviderView, 0, len(list))
	for _, pc := range list {
		out = append(out, toSSOProviderView(pc))
	}
	common.SuccessResp(c, out)
}

// ListSSOPresets returns the built-in provider presets so the admin form can
// prefill issuer templates, default scopes and claim names.
// @Router /v4/admin/sso/presets [get]
func ListSSOPresets(c *gin.Context) {
	common.SuccessResp(c, sso.Presets())
}

// CreateSSOProvider adds a new provider. The key must be unique.
// @Router /v4/admin/sso/providers [post]
func CreateSSOProvider(c *gin.Context) {
	var pc sso.ProviderConfig
	if err := c.ShouldBind(&pc); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := pc.Validate(); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	list, err := loadSSOProviders()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	for _, existing := range list {
		if existing.Key == pc.Key {
			common.ErrorResp(c, errors.Errorf("sso: a provider with key %q already exists", pc.Key), 400)
			return
		}
	}
	list = append(list, pc)
	if err := saveSSOProviders(list); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, toSSOProviderView(pc))
}

// UpdateSSOProvider replaces the provider named by the :key path param. A blank
// client_secret in the body keeps the stored secret (write-only credential).
// @Router /v4/admin/sso/providers/{key} [put]
func UpdateSSOProvider(c *gin.Context) {
	key := c.Param("key")
	var in sso.ProviderConfig
	if err := c.ShouldBind(&in); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// The path param is authoritative for identity; a body key is ignored so an
	// edit can never rename/rekey a provider out from under its link keys.
	in.Key = key

	list, err := loadSSOProviders()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	idx := -1
	for i, existing := range list {
		if existing.Key == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		common.ErrorResp(c, errors.Errorf("sso: no provider with key %q", key), 404)
		return
	}
	// Preserve the stored secret when the client submits a blank one (it never
	// received the secret to resubmit).
	if in.ClientSecret == "" {
		in.ClientSecret = list[idx].ClientSecret
	}
	if err := in.Validate(); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	list[idx] = in
	// The config changed; drop any cached OIDC provider so the next flow rebuilds.
	sso.InvalidateProvider(key)
	if err := saveSSOProviders(list); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, toSSOProviderView(in))
}

// DeleteSSOProvider removes the provider named by :key and deregisters its
// AuthNMethod. Existing links (User.SsoID) are left untouched — re-adding the
// same key restores them.
// @Router /v4/admin/sso/providers/{key} [delete]
func DeleteSSOProvider(c *gin.Context) {
	key := c.Param("key")
	list, err := loadSSOProviders()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	kept := make([]sso.ProviderConfig, 0, len(list))
	found := false
	for _, pc := range list {
		if pc.Key == key {
			found = true
			continue
		}
		kept = append(kept, pc)
	}
	if !found {
		common.ErrorResp(c, errors.Errorf("sso: no provider with key %q", key), 404)
		return
	}
	// Deregister first so a deleted provider's method disappears even if the
	// persisted list still names it momentarily.
	sso.DeregisterProviderMethod(key)
	if err := saveSSOProviders(kept); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

// setSSOProviderEnabled toggles the Enabled flag of :key.
func setSSOProviderEnabled(c *gin.Context, enabled bool) {
	key := c.Param("key")
	list, err := loadSSOProviders()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	idx := -1
	for i, pc := range list {
		if pc.Key == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		common.ErrorResp(c, errors.Errorf("sso: no provider with key %q", key), 404)
		return
	}
	list[idx].Enabled = enabled
	if err := saveSSOProviders(list); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, toSSOProviderView(list[idx]))
}

// EnableSSOProvider marks :key enabled (its login button appears).
// @Router /v4/admin/sso/providers/{key}/enable [post]
func EnableSSOProvider(c *gin.Context) { setSSOProviderEnabled(c, true) }

// DisableSSOProvider marks :key disabled (its login button disappears, its
// AuthNMethod is deregistered on the next resync).
// @Router /v4/admin/sso/providers/{key}/disable [post]
func DisableSSOProvider(c *gin.Context) { setSSOProviderEnabled(c, false) }

// ssoPublicProvider is the anonymous, login-page view of an enabled provider:
// just enough to render and address a login button.
type ssoPublicProvider struct {
	ID     string `json:"id"`     // the AuthNMethod id: "sso:<key>"
	Key    string `json:"key"`    // the bare provider key
	Name   string `json:"name"`   // login button label
	Preset string `json:"preset"` // for a provider-specific icon
}

// PublicSSOProviders lists the ENABLED providers for the login page. It is
// anonymous and exposes no secrets or issuer internals — only what a button
// needs.
// @Router /v4/auth/sso/providers [get]
func PublicSSOProviders(c *gin.Context) {
	list, err := loadSSOProviders()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	out := make([]ssoPublicProvider, 0, len(list))
	for _, pc := range list {
		if !pc.Enabled || pc.Validate() != nil {
			continue
		}
		out = append(out, ssoPublicProvider{
			ID:     sso.MethodID(pc.Key),
			Key:    pc.Key,
			Name:   pc.Name,
			Preset: pc.Preset,
		})
	}
	common.SuccessResp(c, out)
}
