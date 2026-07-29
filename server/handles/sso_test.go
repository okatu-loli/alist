package handles

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/authn/sso"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	otptotp "github.com/pquerna/otp/totp"
)

func setUserSSOID(t *testing.T, u *model.User, ssoID string) {
	t.Helper()
	u.SsoID = ssoID
	if err := op.UpdateUser(u); err != nil {
		t.Fatalf("set sso id: %v", err)
	}
}

// reloadUser fetches the latest persisted row (bypassing any cached copy) so a
// test can assert what the resolver actually wrote.
func reloadUser(t *testing.T, id uint) *model.User {
	t.Helper()
	u, err := op.GetUserById(id)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	return u
}

// --- identity resolution (design §3, the security core) ---

// A user linked by subject is returned by sub match — the authoritative path.
func TestResolveSubLinkWins(t *testing.T) {
	u := mustAuthNUser(t, "sso-sub-user", model.GENERAL, false)
	setUserSSOID(t, u, "prov:sub-1")

	got, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "sub-1", Email: "someone-else@x", EmailVerified: true})
	if err != nil {
		t.Fatalf("sub-linked user must resolve: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("resolved wrong user: got %d want %d", got.ID, u.ID)
	}
}

// A verified email matching an existing NON-admin account links the subject and
// logs in.
func TestResolveEmailFirstLoginLinksNonAdmin(t *testing.T) {
	u := mustAuthNUser(t, "link@example.com", model.GENERAL, false)

	got, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "sub-new", Email: "link@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("verified-email first login must link + succeed: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("resolved wrong user: %d vs %d", got.ID, u.ID)
	}
	if reloadUser(t, u.ID).SsoID != "prov:sub-new" {
		t.Fatal("first login must persist the subject link on the account")
	}
}

// An UNLINKED admin whose email matches is refused: an admin may never be linked
// or logged in by email — only by an explicit prior link.
func TestResolveUnlinkedAdminRefused(t *testing.T) {
	u := mustAuthNUser(t, "admin@example.com", model.ADMIN, false)

	_, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "sub-admin", Email: "admin@example.com", EmailVerified: true})
	if err == nil {
		t.Fatal("an unlinked admin identity must be refused")
	}
	if reloadUser(t, u.ID).SsoID != "" {
		t.Fatal("a refused admin must NOT get its subject linked")
	}
}

// A LINKED admin (linked earlier by subject) is allowed in — the one path SSO
// admin login is permitted, and only because the link already exists.
func TestResolveLinkedAdminAllowed(t *testing.T) {
	u := mustAuthNUser(t, "sso-admin-linked", model.ADMIN, false)
	setUserSSOID(t, u, "prov:admin-sub")

	got, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "admin-sub", Email: "sso-admin-linked", EmailVerified: true})
	if err != nil {
		t.Fatalf("a linked admin must be allowed: %v", err)
	}
	if got.ID != u.ID || !got.IsAdmin() {
		t.Fatal("linked admin must resolve to the admin account")
	}
}

// Auto-provision default-off: an unmatched identity is refused and no user is
// created.
func TestResolveAutoProvisionOffRefuses(t *testing.T) {
	setAutoProvision(t, false)
	_, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "ghost-sub", Email: "ghost@example.com", EmailVerified: true, Username: "ghost-user"})
	if err == nil {
		t.Fatal("an unmatched identity must be refused while auto-provision is off")
	}
	if _, err := op.GetUserByName("ghost-user"); err == nil {
		t.Fatal("no user may be created while auto-provision is off")
	}
}

// Auto-provision on: an unmatched identity creates a minimal, non-admin user
// bound to the subject.
func TestResolveAutoProvisionOnCreatesMinimalUser(t *testing.T) {
	setAutoProvision(t, true)
	got, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "fresh-sub", Email: "fresh@example.com", EmailVerified: true, Username: "sso-fresh-user"})
	if err != nil {
		t.Fatalf("auto-provision on must create + resolve: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(got.ID) })
	if got.IsAdmin() {
		t.Fatal("an auto-provisioned SSO user must never be an admin")
	}
	if got.SsoID != "prov:fresh-sub" {
		t.Fatalf("provisioned user must be bound to the subject, got %q", got.SsoID)
	}
	if err := got.ValidateRawPassword(""); err == nil {
		t.Fatal("provisioned user must not have a usable/empty local password")
	}
}

// An email that is NOT verified must not link, even to a matching account: the
// resolver falls through to the (off) provision gate and refuses.
func TestResolveUnverifiedEmailDoesNotLink(t *testing.T) {
	setAutoProvision(t, false)
	u := mustAuthNUser(t, "unverified@example.com", model.GENERAL, false)

	_, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "sub-uv", Email: "unverified@example.com", EmailVerified: false})
	if err == nil {
		t.Fatal("an unverified email must not link/login")
	}
	if reloadUser(t, u.ID).SsoID != "" {
		t.Fatal("an unverified email must not write a subject link")
	}
}

// An identity with no subject is refused (the link key would be malformed).
func TestResolveNoSubjectRefused(t *testing.T) {
	if _, err := resolveFederatedUser("prov", sso.FederatedIdentity{Email: "x@example.com", EmailVerified: true}); err == nil {
		t.Fatal("an identity with no subject must be refused")
	}
}

// --- callback (design §2): state single-use, verification, landing ---

func setSSOProvider(t *testing.T, pc sso.ProviderConfig) {
	t.Helper()
	list := []sso.ProviderConfig{pc}
	b, _ := json.Marshal(list)
	if err := op.SaveSettingItem(&model.SettingItem{
		Key: conf.SSOProviders, Value: string(b), Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE,
	}); err != nil {
		t.Fatalf("save providers: %v", err)
	}
	t.Cleanup(func() {
		_ = op.SaveSettingItem(&model.SettingItem{
			Key: conf.SSOProviders, Value: "[]", Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE,
		})
		sso.InvalidateProvider(pc.Key)
	})
}

// doCallback drives the callback. An optional linkCookie sets the sso_link_state
// cookie the link flow's own browser carries (== state); login callbacks omit it.
func doCallback(t *testing.T, methodID, code, state string, linkCookie ...string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	q := url.Values{"code": {code}, "state": {state}}
	req := httptest.NewRequest("GET", "http://app.example.com/api/v4/auth/method/"+methodID+"/callback?"+q.Encode(), nil)
	if len(linkCookie) > 0 && linkCookie[0] != "" {
		req.AddCookie(&http.Cookie{Name: ssoLinkCookie, Value: linkCookie[0]})
	}
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: methodID}}
	AuthNMethodCallback(c)
	return w
}

func locationFragment(t *testing.T, w *httptest.ResponseRecorder) (path string, frag url.Values) {
	t.Helper()
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatalf("expected a redirect Location, got status %d body %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location %q: %v", loc, err)
	}
	frag, _ = url.ParseQuery(u.Fragment)
	return u.Path, frag
}

func TestCallbackLoginHappyPath(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	idp := newSSOMockIdP(t)
	idp.subject = "sub-happy"
	idp.nonce = "nonce-ok"
	setSSOProvider(t, sso.ProviderConfig{Key: "tprov", Name: "T", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})

	u := mustAuthNUser(t, "sso-happy", model.GENERAL, false)
	setUserSSOID(t, u, "tprov:sub-happy")

	state := "state-happy"
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "tprov", Nonce: "nonce-ok", PKCEVerifier: "v", Remember: true, CreatedAt: time.Now()}, sso.StateTTL)

	w := doCallback(t, "sso:tprov", "code-1", state)
	if w.Code != 302 {
		t.Fatalf("expected redirect, got %d: %s", w.Code, w.Body.String())
	}
	path, frag := locationFragment(t, w)
	if path != ssoLoginLandingPath {
		t.Fatalf("login must land on %q, got %q", ssoLoginLandingPath, path)
	}
	tok := frag.Get("sso_token")
	if tok == "" || frag.Get("sso_refresh_token") == "" {
		t.Fatalf("login landing must carry a token pair in the fragment: %v", frag)
	}
	if _, err := common.ParseToken(tok); err != nil {
		t.Fatalf("issued token must validate against the core token store: %v", err)
	}
	// State is single-use: it was consumed by the callback.
	if _, ok := sso.DefaultStateStore.Take(state); ok {
		t.Fatal("callback must consume the state (single-use)")
	}
}

// TestCallbackTOTPBoundUserGetsChallengeNotToken is the red-team I5
// regression (adjudicated: enforce, not exempt): an SSO-verified identity
// does NOT bypass a linked account's TOTP second factor. A user with a
// confirmed TOTP secret completing SSO login must land with a step-up
// challenge in the fragment, NOT a token pair — mirroring the password
// path's 402 response, but as a redirect fragment since this is a bare
// browser navigation.
func TestCallbackTOTPBoundUserGetsChallengeNotToken(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	registerTOTPBuiltinFeature(t)
	idp := newSSOMockIdP(t)
	idp.subject = "sub-totp-bound"
	idp.nonce = "nonce-totp"
	setSSOProvider(t, sso.ProviderConfig{Key: "tprovtotp", Name: "T", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})

	u := mustAuthNUser(t, "sso-totp-bound", model.GENERAL, false)
	setUserSSOID(t, u, "tprovtotp:sub-totp-bound")
	user := newReloadableUser(u)
	secret, _ := httpEnrollTOTP(t, user)

	state := "state-totp-bound"
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "tprovtotp", Nonce: "nonce-totp", PKCEVerifier: "v", CreatedAt: time.Now()}, sso.StateTTL)

	w := doCallback(t, "sso:tprovtotp", "code-1", state)
	if w.Code != 302 {
		t.Fatalf("expected redirect, got %d: %s", w.Code, w.Body.String())
	}
	path, frag := locationFragment(t, w)
	if path != ssoLoginLandingPath {
		t.Fatalf("must land on %q, got %q", ssoLoginLandingPath, path)
	}
	if frag.Get("sso_token") != "" {
		t.Fatalf("a TOTP-bound user's SSO callback must NEVER carry a token, got fragment %v", frag)
	}
	if frag.Get("need_second_factor") != "true" {
		t.Fatalf("expected need_second_factor=true in the redirect fragment, got %v", frag)
	}
	if frag.Get("method_id") != totpBuiltinMethodID {
		t.Fatalf("method_id = %q, want %q", frag.Get("method_id"), totpBuiltinMethodID)
	}
	challengeID := frag.Get("challenge_id")
	if challengeID == "" {
		t.Fatal("expected a non-empty challenge_id in the redirect fragment")
	}

	// The challenge is real and completes through the SAME anonymous
	// /auth/login/2fa endpoint the password path uses.
	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	_, resp := doLoginStepUp(challengeID, code)
	if resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("completing the SSO-originated challenge via /auth/login/2fa must mint a token, got %+v", resp)
	}
}

func TestCallbackMissingStateRejected(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	w := doCallback(t, "sso:tprov", "code", "no-such-state")
	path, _ := locationFragment(t, w)
	if path != ssoLoginLandingPath {
		t.Fatalf("bad state should land on login, got %q", path)
	}
	if got := queryError(t, w); got != "state" {
		t.Fatalf("missing state must redirect with sso_error=state, got %q", got)
	}
}

func TestCallbackStateReusedRejected(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	idp := newSSOMockIdP(t)
	idp.subject = "sub-reuse"
	idp.nonce = "n"
	setSSOProvider(t, sso.ProviderConfig{Key: "tprov", Name: "T", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})
	u := mustAuthNUser(t, "sso-reuse", model.GENERAL, false)
	setUserSSOID(t, u, "tprov:sub-reuse")

	state := "state-reuse"
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "tprov", Nonce: "n", PKCEVerifier: "v", CreatedAt: time.Now()}, sso.StateTTL)

	if w := doCallback(t, "sso:tprov", "c", state); w.Code != 302 {
		t.Fatalf("first callback should succeed-redirect, got %d", w.Code)
	}
	// A replay of the same state must now be rejected.
	w := doCallback(t, "sso:tprov", "c", state)
	if got := queryError(t, w); got != "state" {
		t.Fatalf("a replayed state must be rejected, got sso_error=%q", got)
	}
}

func TestCallbackWrongNonceRejected(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	idp := newSSOMockIdP(t)
	idp.subject = "sub-nonce"
	idp.nonce = "attacker-nonce"
	setSSOProvider(t, sso.ProviderConfig{Key: "tprov", Name: "T", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})
	u := mustAuthNUser(t, "sso-nonce", model.GENERAL, false)
	setUserSSOID(t, u, "tprov:sub-nonce")

	state := "state-nonce"
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "tprov", Nonce: "expected-nonce", PKCEVerifier: "v", CreatedAt: time.Now()}, sso.StateTTL)

	w := doCallback(t, "sso:tprov", "c", state)
	if got := queryError(t, w); got != "verify" {
		t.Fatalf("a nonce mismatch must fail verification, got sso_error=%q", got)
	}
}

func TestCallbackProviderMismatchRejected(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	state := "state-mismatch"
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "other", Nonce: "n", PKCEVerifier: "v", CreatedAt: time.Now()}, sso.StateTTL)
	w := doCallback(t, "sso:tprov", "c", state)
	if got := queryError(t, w); got != "provider" {
		t.Fatalf("state provider key != route id must be rejected, got sso_error=%q", got)
	}
}

func TestCallbackLinkModeBindsInitiatingUser(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	idp := newSSOMockIdP(t)
	idp.subject = "sub-link-me"
	idp.nonce = "n"
	setSSOProvider(t, sso.ProviderConfig{Key: "tprov", Name: "T", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})

	u := mustAuthNUser(t, "sso-linker", model.GENERAL, false)
	state := "state-link"
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "tprov", Nonce: "n", PKCEVerifier: "v", LinkUserID: u.ID, CreatedAt: time.Now()}, sso.StateTTL)

	// The initiating browser carries the sso_link_state cookie SSOLinkBegin set.
	w := doCallback(t, "sso:tprov", "c", state, state)
	path, _ := locationFragment(t, w)
	if path != ssoAccountLandingPath {
		t.Fatalf("a link flow must land on the account page, got %q", path)
	}
	if reloadUser(t, u.ID).SsoID != "tprov:sub-link-me" {
		t.Fatalf("link must bind the subject to the INITIATING user, got %q", reloadUser(t, u.ID).SsoID)
	}
}

// queryError extracts sso_error from the redirect Location's query.
func queryError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatalf("expected redirect, got %d %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse loc: %v", err)
	}
	if !strings.Contains(loc, "sso_error") {
		t.Fatalf("expected an sso_error redirect, got %q", loc)
	}
	return u.Query().Get("sso_error")
}
