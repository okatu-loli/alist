package handles

import (
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// publicLoginMethod is the anonymous, login-page view of a declared
// LoginMethod: the owning plugin id plus the render metadata a login button
// needs. LoginMethod carries no secrets (id/kind/label/icon/ui_ref only), so
// it is passed through verbatim alongside the plugin id that declared it.
//
// UIRef, when present, points at a route in the plugin's sandboxed ui/ bundle
// (served at /api/v4/extensions/:id/ui/*). The login page mounts that route in
// an allow-scripts iframe to drive a ceremony a declarative button can't
// express (a WebAuthn-style flow). It is a same-plugin RELATIVE path, not a
// secret and not a URL — the login-context bridge is what locks the ceremony
// down (it can only relay begin/finish; the core still validates and mints the
// session). Omitted for a method that needs no custom UI (a plain
// redirect/federated button).
type publicLoginMethod struct {
	PluginID string            `json:"plugin_id"`
	ID       string            `json:"id"`
	Kind     string            `json:"kind"` // primary | second_factor | federated
	Label    map[string]string `json:"label"`
	Icon     string            `json:"icon,omitempty"`
	UIRef    string            `json:"ui_ref,omitempty"`
}

// PublicLoginMethods lists the login_methods contributed by every ENABLED
// extension, for the login page. Anonymous, and exposes no secrets or plugin
// internals — only what a login button needs (mirrors PublicSSOProviders).
// @Summary List public login methods
// @Description Flattens the login_methods contribution of every ENABLED extension into one public, anonymous list (plugin id + render metadata only). Drives the login page's rendering of non-built-in login affordances (federated SSO-like methods, primary/second-factor methods declared by a plugin). No secrets are exposed.
// @Tags auth
// @Produce json
// @Success 200 {object} common.Resp[[]publicLoginMethod]
// @Router /v4/auth/login-methods [get]
func PublicLoginMethods(c *gin.Context) {
	doc := extension.AggregateContributions(extension.DefaultRegistry)
	out := make([]publicLoginMethod, 0)
	for _, e := range doc.Extensions {
		for _, lm := range e.Contributions.LoginMethods {
			out = append(out, publicLoginMethod{
				PluginID: e.ID,
				ID:       lm.ID,
				Kind:     lm.Kind,
				Label:    lm.Label,
				Icon:     lm.Icon,
				UIRef:    lm.UIRef,
			})
		}
	}
	common.SuccessResp(c, out)
}
