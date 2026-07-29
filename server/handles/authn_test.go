package handles

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// stubAuthN is a configurable in-process ext.AuthNMethod double. It only PROVES
// identity (returns a username + verified flag); it can never mint a token. The
// tests drive its returned identity to probe the HOST's gates — exactly the
// red-team surface: can a login method forge an admin session, bypass the
// provisioning gate, or log in a disabled user?
type stubAuthN struct {
	id       string
	kind     ext.AuthKind
	verified bool
	// username, if set, is asserted verbatim; otherwise the method echoes back
	// params["username"], letting a test drive "assert an arbitrary username".
	username  string
	provision *ext.ProvisionHint
}

func (s *stubAuthN) Manifest() ext.Manifest {
	return ext.Manifest{ID: s.id, Kinds: []ext.Kind{ext.KindAuthNMethod}}
}
func (s *stubAuthN) Init(ctx context.Context, h ext.Host) error { return nil }
func (s *stubAuthN) Shutdown(ctx context.Context) error         { return nil }
func (s *stubAuthN) MethodInfo() ext.AuthMethodInfo {
	return ext.AuthMethodInfo{ID: s.id, Kind: s.kind}
}
func (s *stubAuthN) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	return ext.AuthNChallenge{ChallengeID: "chal"}, nil
}
func (s *stubAuthN) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	username := s.username
	if username == "" {
		username = req.Params["username"]
	}
	return ext.AuthNResult{Username: username, Verified: s.verified, AutoProvision: s.provision}, nil
}

// registerStubAuthN registers m under id and clears it on cleanup. Registered
// through the plain (UNtrusted) path — the red-team regressions depend on these
// stubs never being trusted.
func registerStubAuthN(t *testing.T, m *stubAuthN) {
	t.Helper()
	extension.RegisterAuthNMethodInstance(m.id, m)
	t.Cleanup(func() { extension.RegisterAuthNMethodInstance(m.id, nil) })
}

// registerTrustedStubAuthN registers m through the host's TRUSTED built-in path,
// modeling the built-in passkey. A trusted method is the only kind allowed to
// mint an admin session through the AuthNMethod login path.
func registerTrustedStubAuthN(t *testing.T, m *stubAuthN) {
	t.Helper()
	extension.RegisterTrustedAuthNMethodInstance(m.id, m)
	t.Cleanup(func() { extension.RegisterTrustedAuthNMethodInstance(m.id, nil) })
}

type authnFinishResp struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
	Data struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		DeviceKey    string `json:"device_key"`
	} `json:"data"`
}

// doAuthNFinish drives AuthNMethodFinish for method id with the given params.
func doAuthNFinish(t *testing.T, id string, params map[string]string) (*httptest.ResponseRecorder, authnFinishResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{"params": params})
	req := httptest.NewRequest("POST", "/api/v4/auth/method/"+id+"/finish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: id}}
	authnThrottle.Reset(c.ClientIP()) // isolate rate-limit counter across tests
	AuthNMethodFinish(c)
	var resp authnFinishResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

func mustAuthNUser(t *testing.T, username string, role int, disabled bool) *model.User {
	t.Helper()
	u := &model.User{Username: username, Role: model.Roles{role}, Disabled: disabled, Authn: "[]"}
	u.SetPassword("pw-" + username)
	if err := op.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	// Force-remove at the db layer on cleanup. op.DeleteUserById refuses to
	// delete a lone admin (last-admin lockout guard), which would otherwise leak
	// these admin fixtures into the package's shared in-memory DB and break the
	// unrelated last-admin tests. Going straight to db (plus a cache purge)
	// guarantees removal without tripping that guard.
	t.Cleanup(func() {
		_ = db.DeleteUserById(u.ID)
		_ = db.DeleteUserRolesByUser(u.ID)
		_ = op.DelUserCache(username)
	})
	return u
}

func setAutoProvision(t *testing.T, on bool) {
	t.Helper()
	val := "false"
	if on {
		val = "true"
	}
	if err := op.SaveSettingItem(&model.SettingItem{
		Key: conf.AuthNAutoProvision, Value: val, Type: conf.TypeBool, Group: model.GLOBAL, Flag: model.PRIVATE,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = op.SaveSettingItem(&model.SettingItem{
			Key: conf.AuthNAutoProvision, Value: "false", Type: conf.TypeBool, Group: model.GLOBAL, Flag: model.PRIVATE,
		})
	})
}

// --- Invariant ②: a primary method logs in a pre-existing user and gets a
// CORE-issued token. ---

func TestAuthNPrimaryLoginIssuesCoreToken(t *testing.T) {
	mustAuthNUser(t, "authn-alice", model.GENERAL, false)
	registerStubAuthN(t, &stubAuthN{id: "m.primary.ok", kind: ext.AuthPrimary, verified: true, username: "authn-alice"})

	w, resp := doAuthNFinish(t, "m.primary.ok", nil)
	if w.Code != 200 || resp.Code != 200 {
		t.Fatalf("primary login should succeed: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
	if resp.Data.Token == "" || resp.Data.RefreshToken == "" {
		t.Fatalf("login must return a core-issued token pair: %+v", resp.Data)
	}
	// The token must be a genuine core token: it parses and validates against the
	// core's own key/store — proof the CORE minted it, not the plugin.
	if _, err := common.ParseToken(resp.Data.Token); err != nil {
		t.Fatalf("issued access token must validate against the core token store: %v", err)
	}
}

// An unverified assertion mints nothing, even for an existing user.
func TestAuthNUnverifiedMintsNothing(t *testing.T) {
	mustAuthNUser(t, "authn-bob", model.GENERAL, false)
	registerStubAuthN(t, &stubAuthN{id: "m.primary.no", kind: ext.AuthPrimary, verified: false, username: "authn-bob"})

	_, resp := doAuthNFinish(t, "m.primary.no", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("an unverified result must not issue a token: %+v", resp)
	}
}

// --- Invariant ③: federated auto-provision is default-off; when on it creates
// a minimal-role user, never an admin. ---

func TestAuthNFederatedAutoProvisionDefaultDenied(t *testing.T) {
	setAutoProvision(t, false)
	registerStubAuthN(t, &stubAuthN{id: "m.fed.off", kind: ext.AuthFederated, verified: true, username: "fed-newuser-a"})

	_, resp := doAuthNFinish(t, "m.fed.off", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("federated login for an unknown user must be denied while auto-provision is off: %+v", resp)
	}
	if u, err := op.GetUserByName("fed-newuser-a"); err == nil {
		_ = op.DeleteUserById(u.ID)
		t.Fatal("no user may be created while auto-provision is off")
	}
}

func TestAuthNFederatedAutoProvisionEnabledCreatesMinimalUser(t *testing.T) {
	setAutoProvision(t, true)
	registerStubAuthN(t, &stubAuthN{
		id: "m.fed.on", kind: ext.AuthFederated, verified: true, username: "fed-newuser-b",
		provision: &ext.ProvisionHint{Email: "b@idp.example.com"},
	})

	w, resp := doAuthNFinish(t, "m.fed.on", nil)
	if w.Code != 200 || resp.Code != 200 {
		t.Fatalf("federated login should provision + succeed with auto-provision on: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
	created, err := op.GetUserByName("fed-newuser-b")
	if err != nil {
		t.Fatalf("auto-provision must have created the user: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(created.ID) })
	if created.IsAdmin() {
		t.Fatal("an auto-provisioned user must never be an admin")
	}
	// The provisioned account must not be usable via the password path: its local
	// password is an unusable random secret, so the IdP is the only way in.
	if err := created.ValidateRawPassword(""); err == nil {
		t.Fatal("provisioned user must not have an empty/guessable password")
	}
}

// --- Invariant ④: a login method asserting an arbitrary/privileged username
// cannot bypass the gates into a session. ---

// A federated method asserting an existing ADMIN's username, even with
// auto-provision ON, must never yield an admin session.
func TestAuthNFederatedCannotLogInExistingAdmin(t *testing.T) {
	setAutoProvision(t, true)
	mustAuthNUser(t, "authn-admin-fed", model.ADMIN, false)
	registerStubAuthN(t, &stubAuthN{id: "m.fed.admin", kind: ext.AuthFederated, verified: true, username: "authn-admin-fed"})

	w, resp := doAuthNFinish(t, "m.fed.admin", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a login method must never mint an admin session: http=%d body=%s", w.Code, w.Body.String())
	}
}

// A primary method asserting an existing ADMIN's username must likewise be
// refused: admin login stays exclusively on the untouched password path.
func TestAuthNPrimaryCannotLogInExistingAdmin(t *testing.T) {
	mustAuthNUser(t, "authn-admin-pri", model.ADMIN, false)
	registerStubAuthN(t, &stubAuthN{id: "m.pri.admin", kind: ext.AuthPrimary, verified: true, username: "authn-admin-pri"})

	_, resp := doAuthNFinish(t, "m.pri.admin", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a primary login method must never mint an admin session: %+v", resp)
	}
}

// --- Trusted (built-in) relaxation: a TRUSTED method — and only a trusted one —
// may mint an admin session. This is the built-in-passkey carve-out. ---

// A trusted built-in method logging in an existing admin succeeds and gets a
// CORE-issued token. This is the ONE relaxation of the no-admin-session
// invariant, gated entirely by host-side trust.
func TestAuthNTrustedMethodCanLogInAdmin(t *testing.T) {
	mustAuthNUser(t, "authn-admin-trusted", model.ADMIN, false)
	registerTrustedStubAuthN(t, &stubAuthN{id: "builtin.trusted.admin", kind: ext.AuthPrimary, verified: true, username: "authn-admin-trusted"})

	w, resp := doAuthNFinish(t, "builtin.trusted.admin", nil)
	if w.Code != 200 || resp.Code != 200 {
		t.Fatalf("a trusted built-in method must log an admin in: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
	if resp.Data.Token == "" || resp.Data.RefreshToken == "" {
		t.Fatalf("trusted admin login must return a core-issued token pair: %+v", resp.Data)
	}
	if _, err := common.ParseToken(resp.Data.Token); err != nil {
		t.Fatalf("issued admin token must validate against the core token store: %v", err)
	}
}

// A trusted built-in method is still blocked from a DISABLED admin: trust relaxes
// the admin-cap gate only, never the disabled gate.
func TestAuthNTrustedMethodStillBlocksDisabledAdmin(t *testing.T) {
	mustAuthNUser(t, "authn-admin-trusted-off", model.ADMIN, true)
	registerTrustedStubAuthN(t, &stubAuthN{id: "builtin.trusted.admin.off", kind: ext.AuthPrimary, verified: true, username: "authn-admin-trusted-off"})

	_, resp := doAuthNFinish(t, "builtin.trusted.admin.off", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a disabled admin must get no session even from a trusted method: %+v", resp)
	}
}

// A primary method asserting a username that does not exist must be refused (a
// primary method never auto-provisions), so it cannot conjure a session for an
// arbitrary name.
func TestAuthNPrimaryUnknownUserDenied(t *testing.T) {
	registerStubAuthN(t, &stubAuthN{id: "m.pri.unknown", kind: ext.AuthPrimary, verified: true, username: "nobody-here"})

	_, resp := doAuthNFinish(t, "m.pri.unknown", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a primary method must not log in a nonexistent user: %+v", resp)
	}
	if _, err := op.GetUserByName("nobody-here"); err == nil {
		t.Fatal("a primary method must never create a user")
	}
}

// --- Invariant ⑤: a disabled user gets no session even when the method
// verifies them. ---

func TestAuthNDisabledUserDenied(t *testing.T) {
	mustAuthNUser(t, "authn-disabled", model.GENERAL, true)
	registerStubAuthN(t, &stubAuthN{id: "m.pri.disabled", kind: ext.AuthPrimary, verified: true, username: "authn-disabled"})

	w, resp := doAuthNFinish(t, "m.pri.disabled", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a disabled user must get no session: http=%d body=%s", w.Code, w.Body.String())
	}
}

// --- Endpoint guard: a second_factor method is not a standalone login. ---

func TestAuthNSecondFactorRejectedAsStandaloneLogin(t *testing.T) {
	mustAuthNUser(t, "authn-2fa", model.GENERAL, false)
	registerStubAuthN(t, &stubAuthN{id: "m.2fa", kind: ext.AuthSecondFactor, verified: true, username: "authn-2fa"})

	_, resp := doAuthNFinish(t, "m.2fa", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a second_factor method must not be usable as a standalone login: %+v", resp)
	}
}

// --- Begin endpoint ---

type authnBeginResp struct {
	Code int `json:"code"`
	Data struct {
		ChallengeID string `json:"challenge_id"`
		RedirectURL string `json:"redirect_url"`
	} `json:"data"`
}

func doAuthNBegin(t *testing.T, id string) (*httptest.ResponseRecorder, authnBeginResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/api/v4/auth/method/"+id+"/begin", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: id}}
	AuthNMethodBegin(c)
	var resp authnBeginResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// A primary method's Begin returns a challenge id to correlate with Finish.
func TestAuthNBeginPrimaryReturnsChallenge(t *testing.T) {
	registerStubAuthN(t, &stubAuthN{id: "m.begin.primary", kind: ext.AuthPrimary, verified: true})
	_, resp := doAuthNBegin(t, "m.begin.primary")
	if resp.Code != 200 || resp.Data.ChallengeID == "" {
		t.Fatalf("begin should return a challenge: %+v", resp)
	}
}

// A second_factor method must not be beginnable as a standalone login either.
func TestAuthNBeginSecondFactorRejected(t *testing.T) {
	registerStubAuthN(t, &stubAuthN{id: "m.begin.2fa", kind: ext.AuthSecondFactor, verified: true})
	_, resp := doAuthNBegin(t, "m.begin.2fa")
	if resp.Code == 200 {
		t.Fatalf("second_factor begin must be rejected at the standalone-login endpoint: %+v", resp)
	}
}

// --- login_methods/MethodInfo kind cross-check (Phase F, design decision:
// validated on first Begin rather than at Registry.Enable — see
// checkLoginMethodKindDeclaration's doc in authn.go). ---

// registerLoginMethodEntry registers a Kind=AuthNMethod registry Entry (through
// the built-in path, which skips ValidateManifest so a deliberately-mismatched
// test fixture can still register) declaring a single login_methods[] entry for
// id with the given static kind, and deregisters it on cleanup.
func registerLoginMethodEntry(t *testing.T, id, declaredKind string) {
	t.Helper()
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    id,
		Kinds: []ext.Kind{ext.KindAuthNMethod},
		Contributions: ext.Contributions{
			LoginMethods: []ext.LoginMethod{{ID: id, Kind: declaredKind}},
		},
	}); err != nil {
		t.Fatalf("register login_methods entry for %q: %v", id, err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(id) })
}

// A manifest that declares login_methods[].kind="second_factor" but whose live
// AuthNMethod actually resolves to AuthPrimary (a lying/misconfigured
// declaration) must be refused at Begin, not silently trusted — on EVERY
// Begin, not just the first.
//
// Review fix regression test (round 1, reproduced by the reviewer): the
// first cut of checkLoginMethodKindDeclaration cached "already checked" as a
// bare bool, so a genuine mismatch 400'd once and then, because the cache
// short-circuited every later call straight to "already checked → nil",
// silently PASSED forever after. The fix caches the actual verdict (err or
// nil) and returns that SAME verdict on every cache hit — so this drives
// Begin against the SAME id twice and requires the mismatch to still be
// rejected the second time.
func TestAuthNBeginRejectsLoginMethodsKindMismatch(t *testing.T) {
	id := "m.kindmismatch"
	registerStubAuthN(t, &stubAuthN{id: id, kind: ext.AuthPrimary, verified: true})
	registerLoginMethodEntry(t, id, "second_factor")

	_, first := doAuthNBegin(t, id)
	if first.Code == 200 {
		t.Fatalf("a login_methods kind declaration that disagrees with MethodInfo().Kind must be rejected: %+v", first)
	}
	_, second := doAuthNBegin(t, id)
	if second.Code == 200 {
		t.Fatalf("a persistent kind mismatch must still be rejected on a SECOND Begin against the same id (cache-bypass regression), got: %+v", second)
	}
}

// TestAuthNBeginKindMismatchCacheInvalidatesOnReRegister proves the cache is
// keyed on the *Entry pointer, not just the id: a plugin update/reinstall
// (Deregister + re-register under the same id, exactly like Update/Rollback/
// LoadEnabled) installs a brand-new *Entry, and a stale "mismatch" verdict
// computed against the OLD Contributions.LoginMethods must not stick to the
// id forever — the corrected declaration on the NEW Entry must be
// re-evaluated and accepted.
func TestAuthNBeginKindMismatchCacheInvalidatesOnReRegister(t *testing.T) {
	id := "m.kindmismatch.reregister"
	registerStubAuthN(t, &stubAuthN{id: id, kind: ext.AuthPrimary, verified: true})
	registerLoginMethodEntry(t, id, "second_factor") // mismatch: MethodInfo is primary

	_, first := doAuthNBegin(t, id)
	if first.Code == 200 {
		t.Fatalf("expected the initial mismatch to be rejected: %+v", first)
	}

	// Simulate a plugin update: deregister the stale Entry and register a
	// FRESH one (new *Entry pointer) under the same id, this time declaring
	// the CORRECT kind.
	extension.DefaultRegistry.Deregister(id)
	registerLoginMethodEntry(t, id, "primary")

	_, second := doAuthNBegin(t, id)
	if second.Code != 200 {
		t.Fatalf("a corrected declaration on a re-registered Entry must be accepted, not stuck on the stale verdict: %+v", second)
	}
}

// A manifest whose declared login_methods[].kind agrees with the resolved
// method's MethodInfo().Kind begins normally.
func TestAuthNBeginAllowsLoginMethodsKindMatch(t *testing.T) {
	id := "m.kindmatch"
	registerStubAuthN(t, &stubAuthN{id: id, kind: ext.AuthPrimary, verified: true})
	registerLoginMethodEntry(t, id, "primary")

	_, resp := doAuthNBegin(t, id)
	if resp.Code != 200 {
		t.Fatalf("a matching login_methods kind declaration must be accepted: %+v", resp)
	}
}

// An unknown / unregistered method id is a clean 404, never a nil-deref.
func TestAuthNUnknownMethodDenied(t *testing.T) {
	w, resp := doAuthNFinish(t, "does.not.exist", nil)
	if resp.Code != 404 || resp.Data.Token != "" {
		t.Fatalf("unknown method must 404: code=%d body=%s", resp.Code, w.Body.String())
	}
}
