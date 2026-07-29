package handles

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/authn/sso"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// This file is the federated-SSO wiring (design §2–§4). It owns the OIDC
// callback route, the identity resolver (the security core), and the provider
// config persistence the admin/account endpoints (sso_admin.go, sso_account.go)
// build on. The pure OAuth2/OIDC engine lives in internal/authn/sso; here is
// where a verified external identity is turned into (or refused) an AList
// session, and where the load-bearing admin invariant is enforced.
//
// The single most important rule (design §3): SSO may log in an admin ONLY when
// that admin has already linked this exact IdP subject on their account page.
// Email matching never links or logs in an admin; an unlinked admin identity is
// always refused. The AuthNMethod "trusted" flag is a NECESSARY condition, not a
// sufficient one — resolveFederatedUser is where "already linked" is enforced,
// so even a trusted-method admin who has not linked is turned away here.

// Front-end landing paths the browser callback redirects to. The SPA is served
// from the same origin (GetApiUrl base); it reads the login result from the URL
// fragment (kept out of server logs and the Referer header) and the link result
// from a non-secret query flag. W3 implements the consumers; these are the
// contract.
const (
	ssoLoginLandingPath   = "/login"
	ssoAccountLandingPath = "/@account"
)

// loadSSOProviders returns the persisted provider list (empty when unset). A
// malformed setting is surfaced as an error so a corrupt value never silently
// disables SSO.
func loadSSOProviders() ([]sso.ProviderConfig, error) {
	item, err := op.GetSettingItemByKey(conf.SSOProviders)
	if err != nil || item == nil || strings.TrimSpace(item.Value) == "" {
		return nil, nil
	}
	var list []sso.ProviderConfig
	if err := json.Unmarshal([]byte(item.Value), &list); err != nil {
		return nil, errors.Wrap(err, "sso: cannot parse configured providers")
	}
	return list, nil
}

// findSSOProvider returns the configured provider for key.
func findSSOProvider(key string) (sso.ProviderConfig, bool) {
	list, err := loadSSOProviders()
	if err != nil {
		return sso.ProviderConfig{}, false
	}
	for _, pc := range list {
		if pc.Key == key {
			return pc, true
		}
	}
	return sso.ProviderConfig{}, false
}

// resyncSSOMethods reconciles the registered "sso:<key>" AuthNMethods with the
// current provider list: enabled providers are (re)registered, and any method
// for a key no longer enabled is removed. Called at boot and after every
// provider-config mutation so the login page and the begin endpoint always
// reflect the persisted config. Returns the set of enabled keys it registered.
func resyncSSOMethods() map[string]bool {
	list, _ := loadSSOProviders()
	enabled := map[string]bool{}
	for _, pc := range list {
		if pc.Enabled && pc.Validate() == nil {
			sso.RegisterProviderMethod(pc)
			enabled[pc.Key] = true
		}
	}
	// Drop methods whose provider was disabled or deleted. We can only see the
	// keys still present in the list; a deleted provider's key is gone from the
	// list, so deregister every configured-but-not-enabled key we do know, and
	// rely on delete/disable handlers to deregister removed keys explicitly.
	for _, pc := range list {
		if !enabled[pc.Key] {
			sso.DeregisterProviderMethod(pc.Key)
		}
	}
	return enabled
}

// ssoCallbackURL rebuilds the exact redirect_uri used for method id, identically
// to the one AuthNMethodBegin handed the IdP, so the authorize and token-exchange
// redirect_uri match. It honors a provider's explicit RedirectURL override.
func ssoCallbackURL(c *gin.Context, methodID string, pc sso.ProviderConfig) string {
	if strings.TrimSpace(pc.RedirectURL) != "" {
		return pc.RedirectURL
	}
	return common.GetApiUrl(c.Request) + "/api/v4/auth/method/" + methodID + "/callback"
}

// Sentinel resolver outcomes. They are deliberately coarse: the callback maps
// them to generic redirect error codes so the browser never learns which
// specific gate rejected it.
var (
	errSSOAdminMustLink = errors.New("sso: an admin may only sign in via SSO after explicitly linking this identity")
	errSSONoUser        = errors.New("sso: no AList user for this identity and auto-provisioning is disabled")
	errSSONoSubject     = errors.New("sso: the identity carries no subject")
	errSSOAlreadyLinked = errors.New("sso: this account is already linked to a different identity")
)

// resolveFederatedUser maps a fully-verified FederatedIdentity onto an AList user
// for a plain (non-linking) login, enforcing design §3 in order:
//
//  1. Sub-link priority: a user whose SsoID == "<providerKey>:<subject>" is the
//     match — stable and non-forgeable. This is the ONLY path that can return an
//     admin, and only because that admin previously linked this exact subject.
//  2. First login by verified email: if email_verified and the email matches an
//     existing account, link the subject to it and log in — EXCEPT an admin,
//     which is refused (an admin must link explicitly, never by email).
//  3. Otherwise the auto-provision gate (default off) decides: refuse, or create
//     a minimal-role user (never admin) bound to this subject.
//
// Since email matching cannot reach an admin and provisioning never mints one,
// an unlinked admin identity is unreachable except through step 1's explicit
// prior link — exactly the invariant.
func resolveFederatedUser(providerKey string, id sso.FederatedIdentity) (*model.User, error) {
	if id.Subject == "" {
		return nil, errSSONoSubject
	}
	linkKey := providerKey + ":" + id.Subject

	// 1. Sub-based link (authoritative, non-forgeable).
	if u, err := op.GetUserBySSOID(linkKey); err == nil && u != nil {
		return u, nil
	}

	// 2. First login: a verified email matching an existing account. The schema
	//    stores no email column, so the only email-shaped identifier we can match
	//    against is the account username (the email-as-username convention). It
	//    must be email_verified, and it must not be an admin.
	if id.EmailVerified && id.Email != "" {
		if u, err := op.GetUserByName(id.Email); err == nil && u != nil {
			if u.IsAdmin() {
				return nil, errSSOAdminMustLink
			}
			if u.SsoID != "" {
				// Already linked to a subject → reachable ONLY via that linked sub
				// (step 1), never by an email match with a DIFFERENT subject.
				// Without this the link gives no protection: any subject presenting
				// a matching verified email takes the account over, and the victim
				// cannot self-defend by linking (SSO red-team FINDING 1). A new
				// subject must go through explicit account-page linking.
				return nil, errSSOAlreadyLinked
			}
			if err := bindSSOToUser(u, linkKey); err != nil {
				return nil, err
			}
			return u, nil
		}
	}

	// 3. Auto-provision gate (default off): refuse, or create a minimal user.
	if !setting.GetBool(conf.AuthNAutoProvision) {
		return nil, errSSONoUser
	}
	return provisionSSOUser(id, linkKey)
}

// bindSSOToUser writes linkKey into u.SsoID as a first-time link. It never
// clobbers an existing link (a user already bound to some subject keeps it), and
// it double-checks no OTHER account already holds linkKey before writing, so one
// external identity can never end up bound to two AList users.
func bindSSOToUser(u *model.User, linkKey string) error {
	if u.SsoID == linkKey {
		return nil
	}
	if u.SsoID != "" {
		// Already linked to a different subject — fail closed. (resolveFederatedUser
		// already refuses the email path for a linked account before reaching here;
		// this makes the primitive itself never silently proceed on a linked user.)
		return errSSOAlreadyLinked
	}
	if other, err := op.GetUserBySSOID(linkKey); err == nil && other != nil && other.ID != u.ID {
		return errors.New("sso: this identity is already linked to another account")
	}
	u.SsoID = linkKey
	return op.UpdateUser(u)
}

// provisionSSOUser creates a minimal-privilege user bound to linkKey for a
// verified federated identity, mirroring provisionFederatedUser (authn.go): the
// configured default role, an unusable random local password, and a hard refusal
// to ever build an admin. The username is the first non-empty of the IdP's
// preferred_username, email, then subject, validated the same way.
func provisionSSOUser(id sso.FederatedIdentity, linkKey string) (*model.User, error) {
	username := firstNonEmpty(id.Username, id.Email, id.Subject)
	if err := validateProvisionedUsername(username); err != nil {
		return nil, err
	}
	if _, err := op.GetUserByName(username); err == nil {
		// The chosen username is taken by an account we did NOT match by sub or
		// verified email — refuse rather than hijack or collide with it.
		return nil, errors.New("sso: cannot auto-provision, username already in use")
	}
	u := &model.User{
		Username: username,
		Role:     model.Roles{op.GetDefaultRoleID()},
		Disabled: false,
		Authn:    "[]",
		SsoID:    linkKey,
	}
	u.SetPassword(random.String(48))
	if u.IsAdmin() {
		return nil, errors.New("sso: refusing to auto-provision an admin user")
	}
	if err := op.CreateUser(u); err != nil {
		return nil, err
	}
	return op.GetUserByName(username)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// AuthNMethodCallback is the OIDC redirect target (design §2). It is anonymous —
// the IdP redirects a raw browser navigation here with no Authorization header;
// the single-use state is the access control. It exchanges the code, verifies the
// id_token, resolves identity (login) or binds the subject (link), then redirects
// the browser back to the SPA. Every rejection redirects with a generic error
// code so the browser never learns which check failed.
// @Router /v4/auth/method/{id}/callback [get]
func AuthNMethodCallback(c *gin.Context) {
	methodID := c.Param("id")
	key, ok := sso.KeyFromMethodID(methodID)
	if !ok {
		redirectSSOError(c, ssoLoginLandingPath, "provider")
		return
	}

	// An IdP-reported error (user denied consent, etc.) short-circuits.
	if e := c.Query("error"); e != "" {
		redirectSSOError(c, ssoLoginLandingPath, "denied")
		return
	}

	// Single-use state: Take deletes it. A missing/expired/replayed state is a
	// clean rejection (CSRF/replay defense). state.ProviderKey is authoritative.
	st, ok := sso.DefaultStateStore.Take(c.Query("state"))
	if !ok {
		redirectSSOError(c, landingFor(st), "state")
		return
	}
	if st.ProviderKey != key {
		redirectSSOError(c, landingFor(st), "provider")
		return
	}

	pc, ok := findSSOProvider(st.ProviderKey)
	if !ok || !pc.Enabled {
		redirectSSOError(c, landingFor(st), "provider")
		return
	}

	prov, err := sso.BuildProvider(pc, ssoCallbackURL(c, methodID, pc))
	if err != nil {
		redirectSSOError(c, landingFor(st), "provider")
		return
	}
	identity, err := prov.ExchangeAndVerify(c.Request.Context(), c.Query("code"), st.PKCEVerifier, st.Nonce)
	if err != nil {
		redirectSSOError(c, landingFor(st), "verify")
		return
	}

	if st.LinkUserID > 0 {
		// The link flow must be completed by the SAME browser that started it: the
		// HttpOnly cookie SSOLinkBegin set must equal this state. A victim who was
		// tricked into opening the attacker's authorization URL has no such cookie,
		// so their subject can never be bound to the attacker's LinkUserID (SSO
		// red-team FINDING 2). The cookie is single-use — cleared either way.
		cookie, cerr := c.Cookie(ssoLinkCookie)
		clearSSOLinkCookie(c)
		if cerr != nil || cookie == "" || cookie != c.Query("state") {
			redirectSSOError(c, ssoAccountLandingPath, "denied")
			return
		}
		completeSSOLink(c, st, pc.Key, identity)
		return
	}
	completeSSOLogin(c, st, pc.Key, methodID, identity)
}

// ssoLinkCookie names the HttpOnly cookie that binds an account link flow to the
// browser that started it (SSO red-team FINDING 2). Its value is the flow's state
// token; the callback requires it to equal ?state= before completing a link.
const ssoLinkCookie = "sso_link_state"

// setSSOLinkCookie stores the link flow's state token in an HttpOnly, path-scoped
// cookie that lives only as long as the state (StateTTL). Secure is derived from
// the request scheme so it works over http on localhost and is Secure in prod.
func setSSOLinkCookie(c *gin.Context, state string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(ssoLinkCookie, state, int(sso.StateTTL/time.Second), "/api/v4/auth/method", "",
		strings.HasPrefix(common.GetApiUrl(c.Request), "https://"), true)
}

// clearSSOLinkCookie expires the link cookie (single-use).
func clearSSOLinkCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(ssoLinkCookie, "", -1, "/api/v4/auth/method", "",
		strings.HasPrefix(common.GetApiUrl(c.Request), "https://"), true)
}

// completeSSOLogin resolves the verified identity to a user, gates it, mints a
// core session, and redirects to the login landing with the token pair in the
// URL fragment.
func completeSSOLogin(c *gin.Context, st sso.State, providerKey, methodID string, identity sso.FederatedIdentity) {
	user, err := resolveFederatedUser(providerKey, identity)
	if err != nil {
		audit.Event(c, 0, identity.Subject, "auth.login", methodID, false)
		redirectSSOError(c, ssoLoginLandingPath, "denied")
		return
	}
	if user.Disabled {
		audit.Event(c, user.ID, user.Username, "auth.login", methodID, false)
		redirectSSOError(c, ssoLoginLandingPath, "disabled")
		return
	}
	// Red-team I5: an SSO-verified identity does NOT bypass a bound second
	// factor. See resolveSecondFactor's doc (login_stepup.go) for the full
	// rationale — in short, User.SsoID is a self-service, user-writable field
	// (SSOLinkBegin), so "this account has a linked SSO identity" is not
	// something the host can treat as an independent, IdP-attested MFA signal;
	// a stolen or federated-provisioned identity must still clear the SAME
	// step-up gate a password login would. No token is minted here — the
	// browser is redirected with a challenge instead, and completes it at
	// either the TOTP-specific POST /auth/login/2fa alias or the generic POST
	// /auth/method/:id/stepup/finish (both dispatch through the SAME
	// finishSecondFactor core, authn.go — transport-agnostic: it only cares
	// about a valid challenge id + response, not how the challenge was begun).
	if stepUpMethodID, ok := resolveSecondFactor(user); ok {
		challengeID, err := beginSecondFactorStepUp(c.Request.Context(), stepUpMethodID, user)
		if err != nil {
			audit.Event(c, user.ID, user.Username, "auth.login", methodID, false)
			redirectSSOError(c, ssoLoginLandingPath, "second_factor")
			return
		}
		audit.Event(c, user.ID, user.Username, "auth.login.second_factor_required", methodID, true)
		redirectSSOResult(c, ssoLoginLandingPath, url.Values{
			"need_second_factor": {"true"},
			"method_id":          {stepUpMethodID},
			"challenge_id":       {challengeID},
		})
		return
	}
	// This callback is a bare browser redirect with no Client-Id header; replay
	// the one captured at begin so mintAuthNSessionTokens keys the device session
	// to the same id the frontend's subsequent XHRs present — otherwise every
	// authed call after landing fails the device-session check and force-logs-out.
	if st.ClientID != "" {
		c.Request.Header.Set("Client-Id", st.ClientID)
	}
	token, refreshToken, deviceKey, ok := mintAuthNSessionTokens(c, user, st.Remember)
	if !ok {
		// mintAuthNSessionTokens already wrote an error response (rare operational
		// failure, e.g. device limit); do not also redirect.
		return
	}
	audit.Event(c, user.ID, user.Username, "auth.login", methodID, true)
	redirectSSOResult(c, ssoLoginLandingPath, url.Values{
		"sso_token":         {token},
		"sso_refresh_token": {refreshToken},
		"sso_device_key":    {deviceKey},
	})
}

// completeSSOLink binds the verified subject to the user who INITIATED the link
// flow (design §4). The target user is taken from st.LinkUserID — set when the
// authenticated account holder started the flow and carried through the single-
// use state — never from the identity's email, so a link always attaches to the
// account that asked for it. It refuses to bind a subject already linked to a
// different account. This is the ONLY way an admin gains SSO login capability.
func completeSSOLink(c *gin.Context, st sso.State, providerKey string, identity sso.FederatedIdentity) {
	if identity.Subject == "" {
		redirectSSOError(c, ssoAccountLandingPath, "verify")
		return
	}
	linkKey := providerKey + ":" + identity.Subject
	user, err := op.GetUserById(st.LinkUserID)
	if err != nil || user == nil {
		redirectSSOError(c, ssoAccountLandingPath, "denied")
		return
	}
	if other, err := op.GetUserBySSOID(linkKey); err == nil && other != nil && other.ID != user.ID {
		redirectSSOError(c, ssoAccountLandingPath, "conflict")
		return
	}
	user.SsoID = linkKey
	if err := op.UpdateUser(user); err != nil {
		redirectSSOError(c, ssoAccountLandingPath, "denied")
		return
	}
	audit.Event(c, user.ID, user.Username, "auth.sso_link", providerKey, true)
	redirectSSOResult(c, ssoAccountLandingPath+"?sso_linked="+url.QueryEscape(providerKey), nil)
}

// landingFor picks the redirect landing appropriate to the flow the parked state
// belonged to (the account page for a link flow, the login page otherwise), so
// an error sends the user back where they started. A zero state (missing/expired)
// defaults to the login page.
func landingFor(st sso.State) string {
	if st.LinkUserID > 0 {
		return ssoAccountLandingPath
	}
	return ssoLoginLandingPath
}

// redirectSSOResult sends the browser to the SPA landing path with result values
// in the URL fragment (never a query, so tokens stay out of logs/Referer). The
// path is a fixed internal constant — never attacker-influenced — so there is no
// open-redirect surface.
func redirectSSOResult(c *gin.Context, path string, frag url.Values) {
	loc := common.GetApiUrl(c.Request) + path
	if len(frag) > 0 {
		loc += "#" + frag.Encode()
	}
	c.Redirect(http.StatusFound, loc)
}

// redirectSSOError redirects to a fixed internal landing with a generic,
// non-revealing error code in the query.
func redirectSSOError(c *gin.Context, path, code string) {
	loc := common.GetApiUrl(c.Request) + path + "?sso_error=" + url.QueryEscape(code)
	c.Redirect(http.StatusFound, loc)
}
