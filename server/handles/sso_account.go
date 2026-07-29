package handles

import (
	"strings"

	"github.com/alist-org/alist/v3/internal/authn/sso"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// This file is the AUTHENTICATED account-page surface for SSO links (design §4):
// a logged-in user starts a link flow, lists their linked identity, and unlinks
// it. Linking is the ONLY way an admin gains SSO login capability — it binds the
// verified subject to the CALLER's own account (via the state's LinkUserID),
// never by email, so it cannot attach an identity to anyone but the initiator.

// splitLinkKey splits a stored "<provider_key>:<subject>" link key. Provider keys
// are validated to contain no ':', so the FIRST ':' is the boundary.
func splitLinkKey(linkKey string) (providerKey, subject string, ok bool) {
	i := strings.IndexByte(linkKey, ':')
	if i <= 0 || i == len(linkKey)-1 {
		return "", "", false
	}
	return linkKey[:i], linkKey[i+1:], true
}

// SSOLinkBegin starts an account-linking flow for the current user: it parks a
// state carrying LinkUserID = caller's id and returns the IdP authorization URL.
// The shared callback (AuthNMethodCallback) completes it, binding the subject to
// THIS user. Not available to the guest.
// @Router /v4/auth/method/{id}/link [post]
func SSOLinkBegin(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	methodID := c.Param("id")
	key, ok := sso.KeyFromMethodID(methodID)
	if !ok {
		common.ErrorStrResp(c, "not an SSO method", 400)
		return
	}
	pc, ok := findSSOProvider(key)
	if !ok || !pc.Enabled {
		common.ErrorStrResp(c, "no such enabled SSO provider", 404)
		return
	}
	// A link flow does not mint a session (it binds the subject to the caller),
	// so the client id is not load-bearing here; pass the browser's for symmetry.
	authURL, state, err := sso.StartFlow(pc, ssoCallbackURL(c, methodID, pc), false, user.ID, c.GetHeader("Client-Id"))
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// Bind the flow to THIS browser: the callback for a link-mode state requires
	// an HttpOnly cookie equal to the state. So a victim tricked into opening an
	// attacker's link URL cannot complete the attacker's flow and bind their own
	// subject to the attacker's account (SSO red-team FINDING 2).
	setSSOLinkCookie(c, state)
	common.SuccessResp(c, gin.H{"redirect_url": authURL})
}

// ssoLinkView is the non-secret account view of a linked SSO identity.
type ssoLinkView struct {
	ProviderKey  string `json:"provider_key"`
	ProviderName string `json:"provider_name"`
	Subject      string `json:"subject"`
}

// SSOListLinks lists the current user's linked SSO identity. The schema stores a
// single link key per user, so this returns zero or one entry. Not available to
// the guest.
// @Router /v4/me/sso [get]
func SSOListLinks(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	out := make([]ssoLinkView, 0, 1)
	if key, subject, ok := splitLinkKey(user.SsoID); ok {
		name := key
		if pc, found := findSSOProvider(key); found {
			name = pc.Name
		}
		out = append(out, ssoLinkView{ProviderKey: key, ProviderName: name, Subject: subject})
	}
	common.SuccessResp(c, out)
}

// SSOUnlink removes the current user's link to the provider named by :key. It
// only ever touches the CALLER's own SsoID, so one user can never unlink
// another's identity. Not available to the guest.
// @Router /v4/me/sso/{key} [delete]
func SSOUnlink(c *gin.Context) {
	user, ok := currentManageableUser(c)
	if !ok {
		return
	}
	key := c.Param("key")
	curKey, _, ok := splitLinkKey(user.SsoID)
	if !ok || curKey != key {
		common.ErrorStrResp(c, "no such linked provider", 404)
		return
	}
	user.SsoID = ""
	if err := op.UpdateUser(user); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}
