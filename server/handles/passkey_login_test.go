package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/authn/passkey"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/descope/virtualwebauthn"
	"github.com/gin-gonic/gin"
)

// End-to-end proof that the built-in passkey AuthNMethod logs a user in — and,
// critically, that it may log in an ADMIN (the one trusted carve-out), using a
// real virtual authenticator producing genuine assertion signatures.

const (
	pkTestRPID   = "example.com"
	pkTestOrigin = "https://example.com"
)

// pinWebauthnRP pins the RP config to a fixed test domain so the virtual
// authenticator's signatures verify deterministically regardless of the httptest
// request host. Cleared on cleanup.
func pinWebauthnRP(t *testing.T) {
	t.Helper()
	set := func(key, val string) {
		if err := op.SaveSettingItem(&model.SettingItem{Key: key, Value: val, Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	set(conf.WebAuthnRPID, pkTestRPID)
	set(conf.WebAuthnRPDisplayName, "Example")
	set(conf.WebAuthnRPOrigins, pkTestOrigin)
	t.Cleanup(func() {
		set(conf.WebAuthnRPID, "")
		set(conf.WebAuthnRPDisplayName, "")
		set(conf.WebAuthnRPOrigins, "")
	})
}

// pkFixture holds a virtual authenticator bound to a user, and registers a
// passkey for that user through the real Service so login can exercise it.
type pkFixture struct {
	user *model.User
	rp   virtualwebauthn.RelyingParty
	auth virtualwebauthn.Authenticator
	cred virtualwebauthn.Credential
}

func newPkFixture(t *testing.T, user *model.User) *pkFixture {
	t.Helper()
	auth := virtualwebauthn.NewAuthenticator()
	auth.Options.UserHandle = user.WebAuthnID()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	f := &pkFixture{
		user: user,
		rp:   virtualwebauthn.RelyingParty{ID: pkTestRPID, Name: "Example", Origin: pkTestOrigin},
		auth: auth,
		cred: cred,
	}
	f.registerPasskey(t)
	return f
}

// registerPasskey runs a real registration ceremony through the Service and
// persists the credential onto the user (the same effect the HTTP register
// endpoints have), so a later login can find it.
func (f *pkFixture) registerPasskey(t *testing.T) {
	t.Helper()
	cid, options, err := passkey.Default.BeginRegister(f.user, "")
	if err != nil {
		t.Fatalf("BeginRegister: %v", err)
	}
	optsJSON, err := json.Marshal(options)
	if err != nil {
		t.Fatalf("marshal creation options: %v", err)
	}
	attOpts, err := virtualwebauthn.ParseAttestationOptions(string(optsJSON))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attResp := virtualwebauthn.CreateAttestationResponse(f.rp, f.auth, f.cred, *attOpts)

	pk, err := passkey.Default.FinishRegister(f.user, cid, []byte(attResp), "test key")
	if err != nil {
		t.Fatalf("FinishRegister: %v", err)
	}
	f.auth.AddCredential(f.cred)

	pks, err := model.ParsePasskeys(f.user.Authn)
	if err != nil {
		t.Fatalf("ParsePasskeys: %v", err)
	}
	pks = append(pks, *pk)
	authn, err := model.MarshalPasskeys(pks)
	if err != nil {
		t.Fatalf("MarshalPasskeys: %v", err)
	}
	f.user.Authn = authn
	if err := op.UpdateUser(f.user); err != nil {
		t.Fatalf("persist passkey: %v", err)
	}
}

// login drives the anonymous AuthNMethod begin+finish endpoints for the built-in
// passkey, signing the challenge with the fixture's virtual authenticator at the
// given counter. userHandle overrides the handle the authenticator returns (to
// forge cross-account handles in negative tests); pass nil to use the real one.
func (f *pkFixture) login(t *testing.T, counter uint32, userHandle []byte) (*httptest.ResponseRecorder, authnFinishResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// --- begin (anonymous) ---
	bw := httptest.NewRecorder()
	bc, _ := gin.CreateTestContext(bw)
	breq := httptest.NewRequest("POST", "/api/v4/auth/method/"+passkey.BuiltinMethodID+"/begin", bytes.NewReader([]byte("{}")))
	breq.Header.Set("Content-Type", "application/json")
	bc.Request = breq
	bc.Params = gin.Params{{Key: "id", Value: passkey.BuiltinMethodID}}
	AuthNMethodBegin(bc)

	var begin struct {
		Code int `json:"code"`
		Data struct {
			ChallengeID string `json:"challenge_id"`
			Data        []byte `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bw.Body.Bytes(), &begin); err != nil {
		t.Fatalf("decode begin: %v (body=%s)", err, bw.Body.String())
	}
	if begin.Code != 200 || begin.Data.ChallengeID == "" {
		t.Fatalf("begin should return a challenge: %s", bw.Body.String())
	}

	asOpts, err := virtualwebauthn.ParseAssertionOptions(string(begin.Data.Data))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v (data=%s)", err, string(begin.Data.Data))
	}
	if userHandle != nil {
		f.auth.Options.UserHandle = userHandle
	}
	f.cred.Counter = counter
	asResp := virtualwebauthn.CreateAssertionResponse(f.rp, f.auth, f.cred, *asOpts)

	// --- finish (anonymous) ---
	fw := httptest.NewRecorder()
	fc, _ := gin.CreateTestContext(fw)
	body, _ := json.Marshal(map[string]any{"challenge_id": begin.Data.ChallengeID, "data": []byte(asResp)})
	freq := httptest.NewRequest("POST", "/api/v4/auth/method/"+passkey.BuiltinMethodID+"/finish", bytes.NewReader(body))
	freq.Header.Set("Content-Type", "application/json")
	fc.Request = freq
	fc.Params = gin.Params{{Key: "id", Value: passkey.BuiltinMethodID}}
	authnThrottle.Reset(fc.ClientIP())
	AuthNMethodFinish(fc)

	var resp authnFinishResp
	_ = json.Unmarshal(fw.Body.Bytes(), &resp)
	return fw, resp
}

// A passkey login for a GENERAL user succeeds and returns a core-issued token.
func TestPasskeyLoginGeneralUser(t *testing.T) {
	pinWebauthnRP(t)
	passkey.RegisterBuiltin()
	t.Cleanup(func() { extension.RegisterTrustedAuthNMethodInstance(passkey.BuiltinMethodID, nil) })

	user := mustAuthNUser(t, "pk-general", model.GENERAL, false)
	f := newPkFixture(t, user)

	w, resp := f.login(t, 1, nil)
	if w.Code != 200 || resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("passkey login should succeed: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
	if _, err := common.ParseToken(resp.Data.Token); err != nil {
		t.Fatalf("issued token must validate against the core store: %v", err)
	}

	// The advanced sign counter must be persisted back onto the stored passkey.
	reloaded, err := op.GetUserById(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	pks, _ := model.ParsePasskeys(reloaded.Authn)
	if len(pks) != 1 || pks[0].SignCount != 1 {
		t.Fatalf("sign counter not persisted: %+v", pks)
	}
	if pks[0].LastUsedAt.IsZero() {
		t.Fatal("last-used timestamp not persisted")
	}
}

// The built-in passkey — being a trusted method — MAY log in an admin, unlike
// every other AuthNMethod. This is the whole point of the trust carve-out.
func TestPasskeyLoginAdminAllowed(t *testing.T) {
	pinWebauthnRP(t)
	passkey.RegisterBuiltin()
	t.Cleanup(func() { extension.RegisterTrustedAuthNMethodInstance(passkey.BuiltinMethodID, nil) })

	admin := mustAuthNUser(t, "pk-admin", model.ADMIN, false)
	f := newPkFixture(t, admin)

	w, resp := f.login(t, 1, nil)
	if w.Code != 200 || resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("built-in passkey must log an admin in: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
	name, err := parseTokenUser(t, resp.Data.Token)
	if err != nil || name != "pk-admin" {
		t.Fatalf("admin token must resolve to the admin: name=%q err=%v", name, err)
	}
}

// A forged userHandle pointing at a DIFFERENT account must not yield a session:
// the resolved account does not own the asserted credential, so the ownership
// assertion (and go-webauthn's own rawID match) rejects it.
func TestPasskeyLoginForgedUserHandleRejected(t *testing.T) {
	pinWebauthnRP(t)
	passkey.RegisterBuiltin()
	t.Cleanup(func() { extension.RegisterTrustedAuthNMethodInstance(passkey.BuiltinMethodID, nil) })

	owner := mustAuthNUser(t, "pk-owner", model.GENERAL, false)
	victim := mustAuthNUser(t, "pk-victim-admin", model.ADMIN, false)
	f := newPkFixture(t, owner)

	// The authenticator returns the VICTIM admin's handle while signing with the
	// owner's (non-admin) credential — an attempt to ride a real credential into
	// another account.
	_, resp := f.login(t, 1, victim.WebAuthnID())
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a forged cross-account user handle must mint no session: %+v", resp)
	}
}

// A disabled user gets no passkey session even though the assertion verifies.
func TestPasskeyLoginDisabledUserRejected(t *testing.T) {
	pinWebauthnRP(t)
	passkey.RegisterBuiltin()
	t.Cleanup(func() { extension.RegisterTrustedAuthNMethodInstance(passkey.BuiltinMethodID, nil) })

	user := mustAuthNUser(t, "pk-disabled", model.GENERAL, true)
	f := newPkFixture(t, user)

	_, resp := f.login(t, 1, nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a disabled user must get no passkey session: %+v", resp)
	}
}

// TestPasskeyLoginGatedWhenFeatureDisabled pins the W5 login-path gate: once
// passkey is a manageable built-in, disabling its Feature entry must make the
// anonymous AuthNMethod begin/finish endpoints 403 "feature disabled" BEFORE any
// ceremony runs — the trusted method instance still resolves (separate map), so
// the registry-entry toggle, not the method's existence, is what blocks login.
// Re-enabling restores login. Password login is untouched (see auth.go / the
// note in TestPasskeyLoginGeneralUser): disabling passkey only closes the
// passwordless path.
func TestPasskeyLoginGatedWhenFeatureDisabled(t *testing.T) {
	pinWebauthnRP(t)
	passkey.RegisterBuiltin()
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:      passkey.BuiltinMethodID,
		Name:    map[string]string{"en": "Passkey", "zh": "通行密钥"},
		Kinds:   []ext.Kind{ext.KindFeature},
		Version: "builtin",
	}); err != nil {
		t.Fatalf("register builtin.passkey feature entry: %v", err)
	}
	t.Cleanup(func() {
		extension.RegisterTrustedAuthNMethodInstance(passkey.BuiltinMethodID, nil)
		extension.DefaultRegistry.Deregister(passkey.BuiltinMethodID)
	})

	user := mustAuthNUser(t, "pk-gated", model.GENERAL, false)
	f := newPkFixture(t, user)

	// Enabled → login succeeds through the gate.
	if w, resp := f.login(t, 1, nil); w.Code != 200 || resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("enabled passkey Feature entry must let login through: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}

	// Disabled → begin is refused 403 before the ceremony (no challenge minted).
	if err := extension.DefaultRegistry.Disable(passkey.BuiltinMethodID); err != nil {
		t.Fatalf("disable builtin.passkey: %v", err)
	}
	bw := httptest.NewRecorder()
	bc, _ := gin.CreateTestContext(bw)
	breq := httptest.NewRequest("POST", "/api/v4/auth/method/"+passkey.BuiltinMethodID+"/begin", bytes.NewReader([]byte("{}")))
	breq.Header.Set("Content-Type", "application/json")
	bc.Request = breq
	bc.Params = gin.Params{{Key: "id", Value: passkey.BuiltinMethodID}}
	AuthNMethodBegin(bc)
	var beginResp struct {
		Code int `json:"code"`
		Data struct {
			ChallengeID string `json:"challenge_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(bw.Body.Bytes(), &beginResp)
	if beginResp.Code != 403 {
		t.Fatalf("disabled passkey begin must be 403, got %d (body=%s)", beginResp.Code, bw.Body.String())
	}
	if beginResp.Data.ChallengeID != "" {
		t.Fatal("disabled passkey begin must not mint a challenge")
	}

	// Disabled → finish is refused 403 too (defense in depth: even a challenge id
	// obtained while enabled cannot be completed once disabled).
	fw := httptest.NewRecorder()
	fc, _ := gin.CreateTestContext(fw)
	fbody, _ := json.Marshal(map[string]any{"challenge_id": "anything", "data": []byte("{}")})
	freq := httptest.NewRequest("POST", "/api/v4/auth/method/"+passkey.BuiltinMethodID+"/finish", bytes.NewReader(fbody))
	freq.Header.Set("Content-Type", "application/json")
	fc.Request = freq
	fc.Params = gin.Params{{Key: "id", Value: passkey.BuiltinMethodID}}
	authnThrottle.Reset(fc.ClientIP())
	AuthNMethodFinish(fc)
	var finishResp struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(fw.Body.Bytes(), &finishResp)
	if finishResp.Code != 403 {
		t.Fatalf("disabled passkey finish must be 403, got %d (body=%s)", finishResp.Code, fw.Body.String())
	}

	// Re-enabled → login works again, proving the gate is the toggle, not a
	// one-way teardown of the method.
	if err := extension.DefaultRegistry.Enable(passkey.BuiltinMethodID); err != nil {
		t.Fatalf("re-enable builtin.passkey: %v", err)
	}
	if w, resp := f.login(t, 2, nil); w.Code != 200 || resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("re-enabled passkey must let login through again: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
}
