package handles

// Red-team probes against the AuthNMethod login path (server/handles/authn.go).
//
// Each test below constructs an attack a malicious or buggy login-method plugin
// might attempt and asserts the HOST refuses it. They target the three hard
// invariants the foundation must uphold and are complementary to authn_test.go
// (which covers the happy paths and the primary/federated admin-refusal). Where
// a probe can only ever "record actual behavior" (e.g. what a case-variant
// provisioning does), the assertion is scoped to the security-relevant fact —
// no token, no admin, no privilege injection — not to a specific error string.
//
//   Invariant 1  token issuance stays 100% in the core (never the plugin)
//   Invariant 2  no login-method path may mint an admin session
//   Invariant 3  federated auto-provision is default-off, minimal-role, never admin
//
// These stay as permanent regression tests: they must remain green.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// parseTokenUser validates a minted access token against the core token store
// (proving the CORE issued it, not the plugin) and returns its subject username.
func parseTokenUser(t *testing.T, token string) (string, error) {
	t.Helper()
	claims, err := common.ParseToken(token)
	if err != nil {
		return "", err
	}
	return claims.Username, nil
}

// makeDefaultRoleAdmin forces GetDefaultRoleID() to resolve to model.ADMIN (2)
// by seeding a `default = true` role row carrying that exact id — the SAME
// resolution path createuser_escalation_test.go uses, chosen to sidestep the
// default_role SETTING hook (which validates the target role exists). This
// simulates an admin who has misconfigured the auto-provision default role to
// the admin role, so the provisioning backstop can be exercised. Cleaned up
// (row + role cache) so the shared in-memory DB is left untouched.
func makeDefaultRoleAdmin(t *testing.T) {
	t.Helper()
	r := &model.Role{ID: uint(model.ADMIN), Name: "rt-misconfig-admin-role", Description: "red-team backstop fixture", Default: true}
	if err := db.GetDb().Create(r).Error; err != nil {
		t.Fatalf("seed admin-id default role: %v", err)
	}
	t.Cleanup(func() {
		_ = db.GetDb().Where("id = ?", uint(model.ADMIN)).Delete(&model.Role{}).Error
		op.InvalidateRolePolicyForTest(uint(model.ADMIN))
	})
	if got := op.GetDefaultRoleID(); got != model.ADMIN {
		t.Fatalf("default-role fixture failed: GetDefaultRoleID()=%d, want %d", got, model.ADMIN)
	}
}

// --- Invariant 2: admin refusal is ROLE-based, not username-based, and holds
// no matter where the ADMIN role sits in the user's role slice. ---

// A malicious primary plugin asserting a user who carries the ADMIN role among
// OTHER roles must still be refused: IsAdmin is a set-membership test, so an
// attacker cannot dilute it by padding the slice with extra roles.
func TestRedteamAdminAmongMultipleRolesDenied(t *testing.T) {
	u := &model.User{Username: "rt-multi-admin", Role: model.Roles{model.GENERAL, model.ADMIN, 7}, Disabled: false, Authn: "[]"}
	u.SetPassword("pw")
	if err := op.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	// Remove at the db layer: op.DeleteUserById enforces the last-admin lockout
	// guard and would REFUSE to delete this admin fixture, leaking it into the
	// package's shared in-memory DB and breaking the unrelated last-admin tests.
	// Same rationale as mustAuthNUser's cleanup.
	t.Cleanup(func() {
		_ = db.DeleteUserById(u.ID)
		_ = db.DeleteUserRolesByUser(u.ID)
		_ = op.DelUserCache(u.Username)
	})

	registerStubAuthN(t, &stubAuthN{id: "rt.multi.admin", kind: ext.AuthPrimary, verified: true, username: "rt-multi-admin"})
	_, resp := doAuthNFinish(t, "rt.multi.admin", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a user holding ADMIN among several roles must never get a login-method session: %+v", resp)
	}
}

// The admin gate keys on the ADMIN role, not on the literal username "admin". A
// user with the ADMIN role but an ordinary-looking username must be refused just
// the same — an attacker cannot dodge the gate by choosing a non-"admin" name.
func TestRedteamAdminRoleWithOrdinaryNameDenied(t *testing.T) {
	mustAuthNUser(t, "rt-ordinary-name", model.ADMIN, false)
	registerStubAuthN(t, &stubAuthN{id: "rt.ordinary.admin", kind: ext.AuthPrimary, verified: true, username: "rt-ordinary-name"})
	_, resp := doAuthNFinish(t, "rt.ordinary.admin", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("an ADMIN-role user with a non-\"admin\" name must still be refused: %+v", resp)
	}
}

// --- Invariant 2 (+ a documented low-severity note): a case-variant of an
// existing admin's username must not yield an ADMIN session. Because usernames
// are matched case-sensitively, "RT-CASE-ADMIN" is an UNKNOWN identity that
// federated auto-provision (when the admin has enabled it) creates as a fresh,
// distinct, minimal-role user — it is NOT the admin. The security invariant that
// must hold: no admin session, and the genuine admin account is untouched. (That
// a confusable low-privilege account gets minted is a hardening observation, not
// a privilege escalation — see the red-team report.) ---
func TestRedteamFederatedCaseVariantCannotEscalateAdmin(t *testing.T) {
	setAutoProvision(t, true)
	admin := mustAuthNUser(t, "rt-case-admin", model.ADMIN, false)

	registerStubAuthN(t, &stubAuthN{id: "rt.case.admin", kind: ext.AuthFederated, verified: true, username: "RT-CASE-ADMIN"})
	w, resp := doAuthNFinish(t, "rt.case.admin", nil)

	// Whatever happens, any session minted must be for a NON-admin identity.
	if resp.Data.Token != "" {
		claims, err := parseTokenUser(t, resp.Data.Token)
		if err != nil {
			t.Fatalf("issued token must be a valid core token: %v", err)
		}
		u, err := op.GetUserByName(claims)
		if err != nil {
			t.Fatalf("token subject must resolve to a real user: %v", err)
		}
		if u.IsAdmin() {
			t.Fatalf("a case-variant of an admin name minted an ADMIN session — INVARIANT 2 BREACH: http=%d body=%s", w.Code, w.Body.String())
		}
	}
	// The shadow user, if created, must be a distinct non-admin, never the admin.
	if shadow, err := op.GetUserByName("RT-CASE-ADMIN"); err == nil {
		t.Cleanup(func() { _ = op.DeleteUserById(shadow.ID); _ = op.DelUserCache(shadow.Username) })
		if shadow.IsAdmin() {
			t.Fatal("a provisioned case-variant user must never be an admin")
		}
		if shadow.ID == admin.ID {
			t.Fatal("provisioning must not have rebound the existing admin account")
		}
	}
	// The genuine admin must remain an admin and unmodified.
	reloaded, err := op.GetUserByName("rt-case-admin")
	if err != nil || !reloaded.IsAdmin() {
		t.Fatalf("the genuine admin must remain an admin and untouched: err=%v", err)
	}
}

// --- Invariant 3: a PRIMARY method never provisions, even with a provision hint
// AND the auto-provision policy switched ON (only federated may create users). --
func TestRedteamPrimaryNeverProvisionsEvenWithHintAndPolicyOn(t *testing.T) {
	setAutoProvision(t, true)
	registerStubAuthN(t, &stubAuthN{
		id: "rt.pri.provision", kind: ext.AuthPrimary, verified: true, username: "rt-pri-ghost",
		provision: &ext.ProvisionHint{Email: "ghost@idp.example.com", DisplayName: "Ghost"},
	})
	_, resp := doAuthNFinish(t, "rt.pri.provision", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a primary method must not provision even with a hint and policy on: %+v", resp)
	}
	if u, err := op.GetUserByName("rt-pri-ghost"); err == nil {
		_ = op.DeleteUserById(u.ID)
		t.Fatal("a primary method must never create a user, regardless of the auto-provision setting")
	}
}

// --- Invariant 3: a ProvisionHint cannot inject a role or privilege. The host
// assigns only the configured default role and ignores every hint attribute a
// plugin might use to request elevation. ---
func TestRedteamProvisionHintCannotInjectPrivilege(t *testing.T) {
	setAutoProvision(t, true)
	registerStubAuthN(t, &stubAuthN{
		id: "rt.fed.inject", kind: ext.AuthFederated, verified: true, username: "rt-inject",
		provision: &ext.ProvisionHint{
			DisplayName: "admin",
			Email:       "rt-inject@idp.example.com",
			// A hostile plugin stuffing role/admin claims into the free-form map;
			// the wire contract has no role field and the host must ignore these.
			Attributes: map[string]string{"role": "admin", "is_admin": "true", "roles": "2", "base_path": "/etc"},
		},
	})
	w, resp := doAuthNFinish(t, "rt.fed.inject", nil)
	if w.Code != 200 || resp.Code != 200 {
		t.Fatalf("federated provision with a hint should still succeed as a NON-admin: http=%d body=%s", w.Code, w.Body.String())
	}
	created, err := op.GetUserByName("rt-inject")
	if err != nil {
		t.Fatalf("user should have been provisioned: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(created.ID); _ = op.DelUserCache(created.Username) })
	if created.IsAdmin() {
		t.Fatal("hint attributes must never elevate a provisioned user to admin")
	}
	// The user must carry EXACTLY the configured default role — nothing the hint
	// tried to inject. In this migration-only test DB the default resolves to
	// GUEST (model.GUEST); assert the role set is precisely that.
	wantRole := op.GetDefaultRoleID()
	if len(created.Role) != 1 || created.Role[0] != wantRole {
		t.Fatalf("provisioned role = %v, want exactly [%d] (the default role, no injected roles)", created.Role, wantRole)
	}
	// The hint's injected "base_path":"/etc" attribute has no effect: BasePath
	// was removed (the file boundary is RBAC now), so there is nothing for a
	// hint to steer. The role assertion above is the meaningful backstop.
}

// --- Invariant 3 hard backstop: even if an admin MISCONFIGURES the default role
// to the ADMIN role, federated provisioning must refuse to build the user rather
// than mint an admin. ---
func TestRedteamMisconfiguredDefaultRoleAdminBackstop(t *testing.T) {
	setAutoProvision(t, true)
	makeDefaultRoleAdmin(t) // the default role misconfigured to model.ADMIN

	registerStubAuthN(t, &stubAuthN{id: "rt.fed.defadmin", kind: ext.AuthFederated, verified: true, username: "rt-defadmin"})
	_, resp := doAuthNFinish(t, "rt.fed.defadmin", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("provisioning under an admin default role must refuse, not mint an admin: %+v", resp)
	}
	if u, err := op.GetUserByName("rt-defadmin"); err == nil {
		_ = op.DeleteUserById(u.ID)
		t.Fatal("the admin backstop must refuse to CREATE the user, not create an admin")
	}
}

// --- Invariant 1: a federated-provisioned account cannot be pivoted into via
// the untouched password path. Its local password is an unusable random secret,
// so no guessable value logs it in. Drives the REAL Login handler end to end. --
func TestRedteamProvisionedUserCannotPivotToPasswordPath(t *testing.T) {
	setAutoProvision(t, true)
	registerStubAuthN(t, &stubAuthN{
		id: "rt.fed.pivot", kind: ext.AuthFederated, verified: true, username: "rt-pivot",
		provision: &ext.ProvisionHint{Email: "rt-pivot@idp.example.com"},
	})
	if w, resp := doAuthNFinish(t, "rt.fed.pivot", nil); w.Code != 200 || resp.Code != 200 {
		t.Fatalf("federated provision should succeed: http=%d body=%s", w.Code, w.Body.String())
	}
	created, err := op.GetUserByName("rt-pivot")
	if err != nil {
		t.Fatalf("provisioned user missing: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(created.ID); _ = op.DelUserCache(created.Username) })

	// Try to log this account in via the password path with every value an
	// attacker would guess for an auto-provisioned SSO user.
	for _, guess := range []string{"", "rt-pivot", "password", "rt-pivot@idp.example.com", "changeme"} {
		w, resp := doPasswordLogin(t, "rt-pivot", guess)
		if resp.Code == 200 || resp.Data.Token != "" {
			t.Fatalf("provisioned account must not be reachable via the password path (guess %q): http=%d body=%s", guess, w.Code, w.Body.String())
		}
	}
}

// --- Invariant 3 boundary: the auto-provision policy gates USER CREATION only.
// A federated method may still log in a pre-existing NON-admin user with the
// policy OFF (positive pin), but must NEVER log in a pre-existing admin. ---
func TestRedteamFederatedProvisionOffLogsInExistingNonAdmin(t *testing.T) {
	setAutoProvision(t, false)
	mustAuthNUser(t, "rt-existing-general", model.GENERAL, false)
	registerStubAuthN(t, &stubAuthN{id: "rt.fed.existing", kind: ext.AuthFederated, verified: true, username: "rt-existing-general"})

	w, resp := doAuthNFinish(t, "rt.fed.existing", nil)
	if w.Code != 200 || resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("federated login of an existing non-admin must succeed even with provisioning off: http=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRedteamFederatedProvisionOffCannotLoginExistingAdmin(t *testing.T) {
	setAutoProvision(t, false)
	mustAuthNUser(t, "rt-existing-admin", model.ADMIN, false)
	registerStubAuthN(t, &stubAuthN{id: "rt.fed.existadmin", kind: ext.AuthFederated, verified: true, username: "rt-existing-admin"})

	_, resp := doAuthNFinish(t, "rt.fed.existadmin", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("federated login of an existing admin must be refused regardless of the provision setting: %+v", resp)
	}
}

// --- Invariant: a disabled user gets no session even with a GUEST-like role and
// a verifying method (the disabled gate must fire independent of role). ---
func TestRedteamDisabledGuestRoleUserDenied(t *testing.T) {
	mustAuthNUser(t, "rt-disabled-guest", model.GUEST, true)
	registerStubAuthN(t, &stubAuthN{id: "rt.disabled.guest", kind: ext.AuthPrimary, verified: true, username: "rt-disabled-guest"})

	_, resp := doAuthNFinish(t, "rt.disabled.guest", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a disabled user must get no session even with a verifying method: %+v", resp)
	}
}

// --- Invariant 2 (+ low-severity note): a whitespace-only asserted username is
// not caught by the empty-string guard (" " != ""), so with federated
// auto-provision ON it flows into provisioning and mints a fresh minimal-role
// user. The invariant that MUST hold is that this junk identity never becomes an
// admin. (That the host provisions a whitespace-named account at all is the same
// missing-normalization hardening observation as the case-variant test.) ---
func TestRedteamFederatedWhitespaceUsernameStaysNonAdmin(t *testing.T) {
	setAutoProvision(t, true)
	registerStubAuthN(t, &stubAuthN{id: "rt.fed.ws", kind: ext.AuthFederated, verified: true, username: " "})
	w, resp := doAuthNFinish(t, "rt.fed.ws", nil)

	if resp.Data.Token != "" {
		claims, err := parseTokenUser(t, resp.Data.Token)
		if err != nil {
			t.Fatalf("issued token must be a valid core token: %v", err)
		}
		u, err := op.GetUserByName(claims)
		if err != nil {
			t.Fatalf("token subject must resolve to a real user: %v", err)
		}
		if u.IsAdmin() {
			t.Fatalf("a whitespace identity minted an ADMIN session — INVARIANT 2 BREACH: http=%d body=%s", w.Code, w.Body.String())
		}
	}
	if u, err := op.GetUserByName(" "); err == nil {
		t.Cleanup(func() { _ = op.DeleteUserById(u.ID); _ = op.DelUserCache(u.Username) })
		if u.IsAdmin() {
			t.Fatal("a whitespace-named provisioned user must never be an admin")
		}
	}
}

// A truly empty username is refused up front regardless of policy.
func TestRedteamFederatedBlankUsernameRefused(t *testing.T) {
	setAutoProvision(t, true)
	// A stub that asserts Verified=true with a literally empty username.
	registerStubAuthN(t, &stubAuthN{id: "rt.fed.blank", kind: ext.AuthFederated, verified: true, username: ""})
	// username:"" makes the stub fall back to params["username"]; leave it unset
	// so the asserted username is empty.
	_, resp := doAuthNFinish(t, "rt.fed.blank", map[string]string{})
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a blank asserted username must mint nothing: %+v", resp)
	}
}

// --- Anti-brute-force: the login-method path throttles failed attempts per IP,
// so a login method cannot be hammered. After the threshold, even a would-be
// success is blocked with 429. ---
func TestRedteamRateLimitBlocksAfterThreshold(t *testing.T) {
	mustAuthNUser(t, "rt-throttle", model.GENERAL, false)
	registerStubAuthN(t, &stubAuthN{id: "rt.throttle.no", kind: ext.AuthPrimary, verified: false, username: "rt-throttle"})
	registerStubAuthN(t, &stubAuthN{id: "rt.throttle.ok", kind: ext.AuthPrimary, verified: true, username: "rt-throttle"})

	const ip = "203.0.113.77"
	authnThrottle.Reset(ip)
	t.Cleanup(func() { authnThrottle.Reset(ip) })

	// defaultTimes failed attempts from one IP.
	for i := 0; i < defaultTimes; i++ {
		w, resp := doAuthNFinishFromIP(t, "rt.throttle.no", ip, nil)
		if resp.Code == 200 {
			t.Fatalf("failed attempt %d should not succeed: http=%d", i, w.Code)
		}
	}
	// The next attempt — even with CORRECT credentials — must be throttled.
	w, resp := doAuthNFinishFromIP(t, "rt.throttle.ok", ip, nil)
	if resp.Code != 429 {
		t.Fatalf("after %d failures the next attempt must be 429 even if valid, got http=%d code=%d body=%s",
			defaultTimes, w.Code, resp.Code, w.Body.String())
	}
	if resp.Data.Token != "" {
		t.Fatal("a throttled attempt must never return a token")
	}
}

// doPasswordLogin drives the real password Login handler (auth.go) for username
// with a plaintext password. Used to prove a provisioned account has no usable
// local password.
func doPasswordLogin(t *testing.T, username, password string) (*httptest.ResponseRecorder, authnFinishResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{"username": username, "password": password})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	passwordThrottle.Reset(c.ClientIP()) // isolate the password-path throttle across guesses
	Login(c)
	var resp authnFinishResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// doAuthNFinishFromIP is doAuthNFinish with a caller-controlled client IP and
// WITHOUT resetting the rate-limit counter — so a test can exercise throttling
// across multiple attempts from the same address.
func doAuthNFinishFromIP(t *testing.T, id, ip string, params map[string]string) (*httptest.ResponseRecorder, authnFinishResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{"params": params})
	req := httptest.NewRequest("POST", "/api/v4/auth/method/"+id+"/finish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	req.RemoteAddr = ip + ":1234"
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: id}}
	AuthNMethodFinish(c)
	var resp authnFinishResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}
