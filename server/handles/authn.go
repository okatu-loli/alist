package handles

import (
	"fmt"
	"strings"
	"sync"
	"unicode"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/device"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// This file adds the AuthNMethod login path (design §2.2): a NEW, opt-in,
// purely additive way to log in via an installed login-method extension
// (SSO/OIDC, LDAP). It never touches the built-in password path in auth.go.
//
// Three invariants are load-bearing and enforced here, not in the plugin:
//
//  1. Token issuance stays 100% in the core. A plugin's Finish returns only
//     "this identity was verified, here is the username" — it can never sign a
//     token. Every session this file mints goes through the SAME common.Generate
//     Token / common.GenerateRefreshToken the password path uses; there is no
//     second token issuer.
//
//  2. No AuthNMethod path may ever mint an ADMIN session. Regardless of what a
//     (possibly malicious or buggy) method asserts, if the resolved user is an
//     admin, no token is issued. Admin login stays exclusively on the untouched
//     password path. This categorically defeats "a malicious login method forges
//     an admin session", which is the sharpest red-team vector here.
//
//  3. Federated auto-provisioning is gated. A federated method asserting an
//     identity with no matching user creates one ONLY when the admin has enabled
//     conf.AuthNAutoProvision (default off), and even then only with the default
//     role — never admin. A primary method never auto-provisions; it logs in
//     pre-existing users only.

// authnThrottle rate-limits the AuthNMethod login path per client IP, so a login
// method can't be brute-forced (design §2.1: the host is where password/
// credential attempts are throttled). It uses the shared loginThrottle policy
// (auth.go's password path and auth_refresh.go's refresh path use their own
// instances) so the three counters never interfere, while all share one
// implementation and the same admin-tunable thresholds.
var authnThrottle = newLoginThrottle()

// authNMethodEntryDisabled reports whether id names a manageable built-in
// registry entry (Kind=Feature) that an admin has DISABLED. It is the login-path
// toggle for any builtin-backed AuthNMethod — currently the built-in passkey,
// which registers a manageable Feature entry under "builtin.passkey" (design W5)
// IN ADDITION to its trusted method instance. The method instance still resolves
// via GetAuthNMethod (a SEPARATE map), so this registry-entry check — not the
// method's existence — is what blocks a disabled login method's ceremony.
//
// It returns false when id has NO registry entry, so it never touches SSO:
// federated SSO method ids are "sso:<key>" (not registry-entry ids), so Get
// returns !ok and SSO is unaffected here — SSO keeps its own /callback gate on
// builtin.sso. This is a general rule: a disabled built-in Feature that backs a
// login method takes that method offline on the login path too, not just the UI.
func authNMethodEntryDisabled(id string) bool {
	e, ok := extension.DefaultRegistry.Get(id)
	return ok && !e.Enabled()
}

// authKindString renders an ext.AuthKind as the same string vocabulary
// Contributions.LoginMethods.Kind uses ("primary" | "second_factor" |
// "federated"), so a manifest's static declaration can be compared against a
// resolved method's authoritative MethodInfo().Kind.
func authKindString(k ext.AuthKind) string {
	switch k {
	case ext.AuthPrimary:
		return "primary"
	case ext.AuthSecondFactor:
		return "second_factor"
	case ext.AuthFederated:
		return "federated"
	default:
		return "unknown"
	}
}

// loginMethodKindCheckResult caches the FULL outcome (not just "already
// checked") of one id's login_methods/MethodInfo kind cross-check, plus the
// exact *extension.Entry it was computed against.
//
// Review fix (round 1): the first cut only cached a bool ("checked once"),
// so a genuine mismatch 400'd on its first Begin and then, because the
// bool-cache short-circuited every later call to "already checked → nil",
// silently PASSED on every Begin after that — the opposite of what a
// security check must do. Caching the actual err (nil or non-nil) and
// returning that SAME cached value on every hit fixes it: a mismatch stays
// 400 forever, until something changes.
//
// entry mirrors the *Entry-pointer staleness pattern FindOfflineDownloadTool
// ByName's grpcToolCacheLookup uses (internal/extension/offline_download_tool.go):
// a plugin update/reinstall does Deregister+Register, installing a brand-new
// *Entry under the same id, so comparing ids alone would keep serving a
// verdict computed against the OLD Contributions.LoginMethods forever.
// Comparing the entry pointer against the CURRENT extension.DefaultRegistry.
// Get(id) result on every call catches that swap and recomputes.
type loginMethodKindCheckResult struct {
	entry *extension.Entry
	err   error
}

var (
	authNMethodKindCheckMu sync.Mutex
	authNMethodKindCache   = map[string]loginMethodKindCheckResult{}
)

// checkLoginMethodKindDeclaration cross-validates, once per (id, *Entry)
// pair, that its manifest's Contributions.LoginMethods[].Kind (the static
// declaration the login page's rendering — and the login_methods
// contribution generally — relies on) agrees with the resolved method's own,
// authoritative MethodInfo().Kind. A mismatch would let a plugin declare
// itself "primary" in the aggregated UI contribution while actually behaving
// as "second_factor" (or vice versa), which would confuse the login-flow
// orchestrator's routing (a declared-primary/actually-second_factor method
// could otherwise slip past this endpoint's AuthSecondFactor refusal).
// Extensions with no registry Entry (the built-in SSO bridge's synthetic
// "sso:<key>" ids, which never carry a real Manifest — see
// ext/authn_method.go's AuthMethodInfo.ID doc) or no login_methods
// declaration for this id have nothing to cross-check and pass through
// untouched.
func checkLoginMethodKindDeclaration(id string, resolvedKind ext.AuthKind) error {
	e, ok := extension.DefaultRegistry.Get(id)
	var curEntry *extension.Entry
	if ok {
		curEntry = e
	}

	authNMethodKindCheckMu.Lock()
	if cached, found := authNMethodKindCache[id]; found && cached.entry == curEntry {
		authNMethodKindCheckMu.Unlock()
		return cached.err
	}
	authNMethodKindCheckMu.Unlock()

	var result error
	if ok {
		declaredKind, found := "", false
		for _, lm := range e.Manifest.Contributions.LoginMethods {
			if lm.ID == id {
				declaredKind, found = lm.Kind, true
				break
			}
		}
		if found {
			if resolved := authKindString(resolvedKind); declaredKind != resolved {
				result = fmt.Errorf("login method %q: declared login_methods kind %q does not match its resolved kind %q", id, declaredKind, resolved)
			}
		}
	}

	authNMethodKindCheckMu.Lock()
	authNMethodKindCache[id] = loginMethodKindCheckResult{entry: curEntry, err: result}
	authNMethodKindCheckMu.Unlock()

	// Logged once per (id, *Entry) — this line only runs on a cache MISS
	// (a fresh computation), never on a cache hit, so a persistent mismatch
	// against the SAME entry logs exactly once, not on every login attempt.
	if result != nil {
		utils.Log.Errorf("extension: %v", result)
	}
	return result
}

// AuthNMethodBeginReq is the optional body of a begin request: method-specific
// client inputs (e.g. a typed LDAP username) the host relays into Begin.
type AuthNMethodBeginReq struct {
	Params map[string]string `json:"params"`
}

// AuthNMethodBeginResp is the challenge the host relays back to the client.
type AuthNMethodBeginResp struct {
	ChallengeID string `json:"challenge_id,omitempty"`
	RedirectURL string `json:"redirect_url,omitempty"`
	Data        []byte `json:"data,omitempty"`
}

// AuthNMethodBegin starts an AuthNMethod login challenge.
// @Summary Begin a login-method challenge
// @Description Starts an installed login method's challenge (federated redirect URL, LDAP prompt). Anonymous; only primary/federated methods are usable here — a second-factor method is rejected (it runs after a password succeeds, never as a standalone login).
// @Tags auth
// @Accept json
// @Produce json
// @Param id path string true "extension id of the login method"
// @Param body body AuthNMethodBeginReq false "method-specific client inputs"
// @Success 200 {object} common.Resp[AuthNMethodBeginResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any] "no such enabled login method"
// @Router /v4/auth/method/{id}/begin [post]
func AuthNMethodBegin(c *gin.Context) {
	id := c.Param("id")
	method, release, err := extension.DefaultRegistry.GetAuthNMethod(id)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	defer release()

	// A builtin-backed login method whose manageable Feature entry an admin has
	// disabled is dead on every path: refuse before running any ceremony (no
	// challenge is minted). SSO method ids are not registry-entry ids, so this
	// never gates SSO (see authNMethodEntryDisabled).
	if authNMethodEntryDisabled(id) {
		common.ErrorResp(c, errors.New("feature disabled"), 403)
		return
	}

	info := method.MethodInfo()

	// First-Begin cross-check (design decision, Phase F): the manifest's static
	// login_methods[].Kind declaration must agree with the method's own
	// authoritative MethodInfo().Kind. See checkLoginMethodKindDeclaration's doc
	// for why this runs here rather than at Registry.Enable.
	if err := checkLoginMethodKindDeclaration(id, info.Kind); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	// A second_factor method is a step-up AFTER a password succeeds; it must not
	// be usable as a standalone login at this anonymous endpoint. Refuse it up
	// front so a login method's kind can never be confused for a primary login.
	if info.Kind == ext.AuthSecondFactor {
		common.ErrorI18n(c, "auth.method_not_a_login", 400)
		return
	}

	var req AuthNMethodBeginReq
	_ = c.ShouldBind(&req) // body is optional; a method may need no client input to begin

	// Capture the browser's Client-Id (authoritative, from the header the begin
	// XHR carries) and hand it to the method. A federated method completes
	// out-of-band via a browser redirect that has NO Client-Id header, so it must
	// park this now to mint the eventual session under the same device key the
	// frontend's later XHRs present; without it the SSO session is keyed to an
	// empty client id and every authed call fails the device-session check.
	// Overriding any client-supplied "client_id" param keeps it non-spoofable.
	clientID := c.GetHeader("Client-Id")
	if clientID == "" {
		clientID = c.Query("client_id")
	}
	if req.Params == nil {
		req.Params = map[string]string{}
	}
	req.Params["client_id"] = clientID

	// State is a host-minted anti-CSRF nonce the method round-trips through any
	// external handshake; the redirect URI points back at AList. (Full federated
	// browser-callback handling — persisting and re-validating state — lands with
	// the real OIDC plugin; the foundation mints and passes it so the contract is
	// exercised end to end.)
	challenge, err := method.Begin(c.Request.Context(), "", ext.AuthNRequest{
		RedirectURI: common.GetApiUrl(c.Request) + "/api/v4/auth/method/" + id + "/callback",
		State:       random.String(32),
		Params:      req.Params,
	})
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, AuthNMethodBeginResp{
		ChallengeID: challenge.ChallengeID,
		RedirectURL: challenge.RedirectURL,
		Data:        challenge.Data,
	})
}

// AuthNMethodFinishReq is the body of a finish request: the client's response to
// the challenge plus the usual "remember me" flag.
type AuthNMethodFinishReq struct {
	ChallengeID string            `json:"challenge_id"`
	Params      map[string]string `json:"params"`
	Data        []byte            `json:"data"`
	Remember    bool              `json:"remember"`
}

// AuthNMethodFinish completes an AuthNMethod login and, on success, mints a
// core session token.
// @Summary Finish a login-method challenge
// @Description Verifies a login method's challenge response and, if the resolved identity passes the host's gates, issues a core session token. The plugin never issues the token. No login-method path can mint an admin session; federated auto-provisioning is gated by authn_auto_provision (default off).
// @Tags auth
// @Accept json
// @Produce json
// @Param id path string true "extension id of the login method"
// @Param body body AuthNMethodFinishReq true "challenge response"
// @Success 200 {object} common.Resp[gin.H] "data: {token, refresh_token, device_key}"
// @Failure 400 {object} common.Resp[any] "verification failed / unknown user / provisioning disabled"
// @Failure 403 {object} common.Resp[any] "resolved user is disabled or an admin (no login-method admin sessions)"
// @Failure 404 {object} common.Resp[any] "no such enabled login method"
// @Failure 429 {object} common.Resp[any] "too many failed attempts"
// @Router /v4/auth/method/{id}/finish [post]
func AuthNMethodFinish(c *gin.Context) {
	id := c.Param("id")

	ip := c.ClientIP()
	if authnThrottle.Blocked(ip) {
		common.ErrorI18n(c, "auth.too_many_login_attempts", 429)
		return
	}

	method, release, err := extension.DefaultRegistry.GetAuthNMethod(id)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	defer release()

	// Same disabled-built-in gate as Begin (design W5): a disabled passkey (or any
	// disabled builtin-backed method) cannot complete a ceremony, even with a
	// challenge id obtained while it was enabled. Checked before touching the body
	// or the method so it consumes no throttle budget. Never gates SSO.
	if authNMethodEntryDisabled(id) {
		common.ErrorResp(c, errors.New("feature disabled"), 403)
		return
	}

	info := method.MethodInfo()
	if info.Kind == ext.AuthSecondFactor {
		common.ErrorI18n(c, "auth.method_not_a_login", 400)
		return
	}

	var req AuthNMethodFinishReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	result, err := method.Finish(c.Request.Context(), ext.AuthNResponse{
		ChallengeID: req.ChallengeID,
		Params:      req.Params,
		Data:        req.Data,
	})
	if err != nil {
		// Operational failure (IdP unreachable, malformed response). Not a
		// credential rejection, but still a failed attempt for rate-limiting.
		authnThrottle.Fail(ip)
		common.ErrorResp(c, err, 400)
		return
	}

	// The plugin's assertion is exactly that — an assertion. Everything below is
	// the host independently deciding whether to trust it. A false Verified (or
	// an empty username) is a clean rejection: the host mints nothing.
	if !result.Verified || result.Username == "" {
		audit.Event(c, 0, result.Username, "auth.login", id, false)
		authnThrottle.Fail(ip)
		common.ErrorI18n(c, "auth.invalid_credentials", 400)
		return
	}

	user, err := resolveOrProvisionUser(info.Kind, result)
	if err != nil {
		audit.Event(c, 0, result.Username, "auth.login", id, false)
		authnThrottle.Fail(ip)
		common.ErrorResp(c, err, 400, true)
		return
	}

	// Invariant 2 (with the built-in carve-out): no login-method path may mint an
	// admin session UNLESS the method is a host-registered trusted built-in (the
	// built-in passkey). Trust is decided by the host at registration
	// (extension.RegisterTrustedAuthNMethodInstance) and read back here — it is
	// never self-reported by the method, so a subprocess/third-party method is
	// structurally excluded and still blocked. This keeps "a malicious login
	// method forges an admin session" defeated for every untrusted method while
	// letting the built-in passkey serve admins.
	if user.IsAdmin() && !extension.DefaultRegistry.AuthNMethodTrusted(id) {
		audit.Event(c, user.ID, user.Username, "auth.login", id, false)
		authnThrottle.Fail(ip)
		common.ErrorI18n(c, "auth.method_cannot_login_admin", 403)
		return
	}
	// A disabled user gets no session even if the method verified them — reuse
	// the same disabled gate the refresh path enforces.
	if user.Disabled {
		audit.Event(c, user.ID, user.Username, "auth.login", id, false)
		common.ErrorI18n(c, "auth.current_user_disabled", 403)
		return
	}

	// Issue the session through the core's existing token issuers — the same
	// functions the password path calls. There is no second token issuer.
	if !issueAuthNSession(c, user, req.Remember) {
		return // issueAuthNSession already wrote the error response
	}
	audit.Event(c, user.ID, user.Username, "auth.login", id, true)
	authnThrottle.Reset(ip)
}

// AuthNMethodStepUpFinishReq is the body of a step-up completion request:
// challenge id + method-specific response fields, mirroring
// AuthNMethodFinishReq but for the SECOND-FACTOR path (a step-up AFTER a
// password/primary/federated login already succeeded), not a standalone login.
type AuthNMethodStepUpFinishReq struct {
	ChallengeID string            `json:"challenge_id" binding:"required"`
	Params      map[string]string `json:"params"`
	Data        []byte            `json:"data"`
	Remember    bool              `json:"remember"`
}

// AuthNMethodStepUpFinish completes a second-factor step-up challenge for ANY
// installed second_factor AuthNMethod (Phase F generalization: TOTP
// (login_stepup.go) was the first, no-longer-only, caller of this dispatch — a
// future WebAuthn or third-party second-factor method completes through this
// SAME endpoint instead of the host growing a bespoke route per method). It is
// the mirror image of AuthNMethodFinish's gate immediately above: THAT endpoint
// refuses a second_factor method outright (it must not be usable as a
// standalone login); THIS one refuses anything that is NOT second_factor (a
// primary/federated method has no step-up ceremony — it logs in through
// begin/finish directly). /auth/login/2fa (LoginTOTPStepUp) is a TOTP-specific
// compatibility alias that calls into this same core (finishSecondFactor).
// @Summary Complete a second-factor step-up challenge
// @Description Verifies a second-factor AuthNMethod's step-up challenge response (begun internally after a password/primary/federated login already succeeded) and, on success, issues a session token. Anonymous by necessity — the challenge id, minted only after the first factor succeeded, is the access control. Rejects a primary/federated method id with 400 — it has no step-up ceremony.
// @Tags auth
// @Accept json
// @Produce json
// @Param id path string true "extension id of the second-factor method"
// @Param body body AuthNMethodStepUpFinishReq true "challenge response"
// @Success 200 {object} common.Resp[gin.H] "data: {token, refresh_token, device_key}"
// @Failure 400 {object} common.Resp[any] "verification failed / not a second-factor method"
// @Failure 403 {object} common.Resp[any] "resolved user disabled / feature disabled"
// @Failure 404 {object} common.Resp[any] "no such enabled method"
// @Failure 429 {object} common.Resp[any] "too many failed attempts"
// @Router /v4/auth/method/{id}/stepup/finish [post]
func AuthNMethodStepUpFinish(c *gin.Context) {
	id := c.Param("id")
	var req AuthNMethodStepUpFinishReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	finishSecondFactor(c, id, req.ChallengeID, req.Params, req.Data, req.Remember)
}

// finishSecondFactor is the generic second-factor step-up completion core:
// both AuthNMethodStepUpFinish (any second_factor method, generic route) and
// LoginTOTPStepUp (login_stepup.go, the TOTP-specific compatibility alias)
// dispatch through this SAME function — there is exactly one implementation of
// "verify a step-up challenge and mint a session", regardless of which route
// or which method id reached it.
func finishSecondFactor(c *gin.Context, id, challengeID string, params map[string]string, data []byte, remember bool) {
	ip := c.ClientIP()
	if stepUpThrottle.Blocked(ip) {
		common.ErrorI18n(c, "auth.too_many_login_attempts", 429)
		return
	}

	// A builtin-backed second-factor method whose manageable Feature entry an
	// admin has disabled is dead on every path: refuse before running any
	// ceremony, spending no throttle budget (nothing has been checked yet).
	// Mirrors the same gate AuthNMethodBegin/Finish apply for primary/federated
	// methods.
	if authNMethodEntryDisabled(id) {
		common.ErrorResp(c, errors.New("feature disabled"), 403)
		return
	}

	method, release, err := extension.DefaultRegistry.GetAuthNMethod(id)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	defer release()

	// The mirror image of AuthNMethodBegin/Finish's second_factor refusal: a
	// primary/federated method has no step-up ceremony to complete here — it
	// logs in through begin/finish directly, never through this path.
	if method.MethodInfo().Kind != ext.AuthSecondFactor {
		common.ErrorI18n(c, "auth.method_not_a_stepup", 400)
		return
	}

	result, err := method.Finish(c.Request.Context(), ext.AuthNResponse{
		ChallengeID: challengeID,
		Params:      params,
		Data:        data,
	})
	if err != nil {
		stepUpThrottle.Fail(ip)
		common.ErrorResp(c, err, 400)
		return
	}
	if !result.Verified || result.Username == "" {
		stepUpThrottle.Fail(ip)
		// Red-team I3 (login_stepup.go): a step-up method may populate Username
		// even on a failed attempt (wrong/replayed code, since-unbound factor) —
		// resolve it to a real actorUID so the audit trail attributes the attempt
		// to the actual account instead of a blank/anonymous (0, "") actor.
		actorUID := uint(0)
		if result.Username != "" {
			if u, err := op.GetUserByName(result.Username); err == nil {
				actorUID = u.ID
			}
		}
		audit.Event(c, actorUID, result.Username, "auth.login", id, false)
		common.ErrorI18n(c, "auth.invalid_2fa_code", 400)
		return
	}

	user, err := op.GetUserByName(result.Username)
	if err != nil {
		stepUpThrottle.Fail(ip)
		common.ErrorResp(c, err, 400, true)
		return
	}
	if user.Disabled {
		audit.Event(c, user.ID, user.Username, "auth.login", id, false)
		common.ErrorI18n(c, "auth.current_user_disabled", 403)
		return
	}

	if !issueAuthNSession(c, user, remember) {
		return
	}
	audit.Event(c, user.ID, user.Username, "auth.login", id, true)
	stepUpThrottle.Reset(ip)
}

// resolveOrProvisionUser maps a verified identity onto an AList user. For a
// primary (LDAP) method the user must already exist. For a federated method an
// unknown user triggers auto-provisioning — but only behind the default-off
// policy gate, and never as an admin.
func resolveOrProvisionUser(kind ext.AuthKind, result ext.AuthNResult) (*model.User, error) {
	user, err := op.GetUserByName(result.Username)
	if err == nil {
		return user, nil
	}
	// User not found. Provisioning is the only way forward, and it is tightly
	// bounded.
	if kind != ext.AuthFederated {
		// Primary methods never create users: an unknown identity is refused.
		return nil, errors.New("no such user")
	}
	if !setting.GetBool(conf.AuthNAutoProvision) {
		// Invariant 3: federated auto-provision is off by default. An unknown
		// federated identity is refused a session until an admin opts in.
		return nil, errors.New("auto-provision disabled for federated login")
	}
	return provisionFederatedUser(result)
}

// provisionFederatedUser creates a minimal-privilege user for a verified
// federated identity. It assigns the configured default role, sets an unusable
// random password (federated users authenticate via their IdP, never a local
// password), and refuses — as a hard backstop — to ever build an admin.
// validateProvisionedUsername rejects a federated identity whose username is
// not a clean single token before it becomes a local account. Auto-provision
// (default off) is the one path that mints users from plugin-asserted strings,
// so a whitespace-only, space-padded, or control-char name — the confusable /
// junk-account surface the red team flagged — must not create an account. This
// is defense-in-depth, not an authorization control: authorization is
// role-based, so such a name could never be admin anyway. System-wide
// case-insensitive username uniqueness (an "Admin" vs "admin" concern) is a
// broader design decision deferred to the Phase 1 SSO/LDAP work.
func validateProvisionedUsername(name string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("refusing to auto-provision a user with a blank or space-padded username")
	}
	for _, r := range name {
		// IsGraphic is false for control AND format characters (Cf), so this
		// also rejects zero-width / invisible code points (e.g. U+200B) — the
		// homoglyph/confusable surface — which IsControl/IsSpace miss. IsSpace
		// additionally rejects ordinary and non-breaking spaces (which ARE
		// graphic).
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) {
			return fmt.Errorf("refusing to auto-provision a user with whitespace or non-graphic characters in the username")
		}
	}
	return nil
}

func provisionFederatedUser(result ext.AuthNResult) (*model.User, error) {
	if err := validateProvisionedUsername(result.Username); err != nil {
		return nil, err
	}
	roleID := op.GetDefaultRoleID()
	u := &model.User{
		Username: result.Username,
		Role:     model.Roles{roleID},
		Disabled: false,
		Authn:    "[]",
	}
	if result.AutoProvision != nil {
		u.SsoID = result.AutoProvision.Email
	}
	// An unusable local password: a long random secret the user never learns,
	// so the provisioned account cannot be logged into via the password path.
	u.SetPassword(random.String(48))

	// Hard backstop: the default role must never resolve to admin. If it somehow
	// does (misconfiguration), refuse to create the user rather than mint an
	// admin. This makes "auto-provision an admin" unreachable by construction.
	if u.IsAdmin() {
		return nil, fmt.Errorf("refusing to auto-provision an admin user")
	}
	if err := op.CreateUser(u); err != nil {
		return nil, err
	}
	// Reload so the returned user carries its assigned id and role detail.
	return op.GetUserByName(result.Username)
}

// mintAuthNSessionTokens registers the login device and mints the access+refresh
// token pair for user through the core's existing issuers (common.GenerateToken /
// common.GenerateRefreshToken) — the SAME functions the password path calls,
// with no second token issuer. It mirrors the token-issuing tail of loginHash
// (auth.go) exactly — deliberately duplicated rather than sharing the password
// path — so the password path stays byte-for-byte unchanged. On failure it
// writes the error response and returns ok=false. The token/refresh/deviceKey
// let a caller land the login however it needs (a JSON body for the XHR-driven
// AuthNMethod finish; a browser redirect for the federated SSO callback).
func mintAuthNSessionTokens(c *gin.Context, user *model.User, remember bool) (token, refreshToken, deviceKey string, ok bool) {
	clientID := c.GetHeader("Client-Id")
	if clientID == "" {
		clientID = c.Query("client_id")
	}
	key := utils.GetMD5EncodeStr(fmt.Sprintf("%d-%s", user.ID, clientID))

	if err := device.EnsureActiveOnLogin(user.ID, key, c.Request.UserAgent(), c.ClientIP()); err != nil {
		if errors.Is(err, errs.TooManyDevices) {
			common.ErrorResp(c, err, 403)
		} else {
			common.ErrorResp(c, err, 400, true)
		}
		return "", "", "", false
	}
	token, err := common.GenerateToken(user, key)
	if err != nil {
		common.ErrorResp(c, err, 400, true)
		return "", "", "", false
	}
	refreshToken, err = common.GenerateRefreshToken(user, key, remember)
	if err != nil {
		common.ErrorResp(c, err, 400, true)
		return "", "", "", false
	}
	return token, refreshToken, key, true
}

// issueAuthNSession mints the session token pair and writes the standard login
// response (used by the XHR AuthNMethod finish path). Returns false (and writes
// the error response) on failure.
func issueAuthNSession(c *gin.Context, user *model.User, remember bool) bool {
	token, refreshToken, key, ok := mintAuthNSessionTokens(c, user, remember)
	if !ok {
		return false
	}
	common.SuccessResp(c, gin.H{"token": token, "refresh_token": refreshToken, "device_key": key})
	return true
}
