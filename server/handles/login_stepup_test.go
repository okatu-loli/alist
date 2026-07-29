package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/authn/totp"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
	otptotp "github.com/pquerna/otp/totp"
)

// This file drives the login step-up path (server/handles/login_stepup.go,
// Phase C: internal TOTP second factor) end to end through the real HTTP
// handlers: a TOTP-bound user's password login must park at a 402 challenge
// (never a token), and only POST /auth/login/2fa with the right code/recovery
// code actually mints one — mirroring passkey_login_test.go's
// TestPasskeyLoginGatedWhenFeatureDisabled for the "register the Feature entry
// inline in the test" pattern.

// registerTOTPBuiltinFeature registers builtin.totp as BOTH a trusted
// AuthNMethod instance and a manageable Feature entry (the same two-map split
// InitTOTP + InitCoreFeatureExtensions do at boot), and tears both down on
// cleanup. Tests in this file that need the step-up path live at all must call
// this first — server/handles' test binary never runs bootstrap.Init*.
func registerTOTPBuiltinFeature(t *testing.T) {
	t.Helper()
	totp.RegisterBuiltin()
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:      totp.BuiltinMethodID,
		Name:    map[string]string{"en": "TOTP Second Factor", "zh": "TOTP 二次验证"},
		Kinds:   []ext.Kind{ext.KindFeature},
		Version: "builtin",
	}); err != nil {
		t.Fatalf("register builtin.totp feature entry: %v", err)
	}
	t.Cleanup(func() {
		extension.RegisterTrustedAuthNMethodInstance(totp.BuiltinMethodID, nil)
		extension.DefaultRegistry.Deregister(totp.BuiltinMethodID)
	})
}

type loginStepUpEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Token            string `json:"token"`
		RefreshToken     string `json:"refresh_token"`
		DeviceKey        string `json:"device_key"`
		NeedSecondFactor bool   `json:"need_second_factor"`
		MethodID         string `json:"method_id"`
		ChallengeID      string `json:"challenge_id"`
	} `json:"data"`
}

func doLoginRaw(username, password string) (*httptest.ResponseRecorder, loginStepUpEnvelope) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{"username": username, "password": password})
	req := httptest.NewRequest("POST", "/api/v4/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	passwordThrottle.Reset(c.ClientIP())
	Login(c)
	var resp loginStepUpEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// doLoginStepUp deliberately does NOT reset stepUpThrottle before each
// call (unlike doLogin's passwordThrottle.Reset) — TestLoginStepUpWrongCode
// IsRejectedAndThrottled depends on failures ACCUMULATING across repeated
// calls within one test. Every test that drives this must reset the shared,
// package-level stepUpThrottle itself, once, at the top (see
// resetTOTPStepUpThrottle).
func doLoginStepUp(challengeID, code string) (*httptest.ResponseRecorder, loginStepUpEnvelope) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{"challenge_id": challengeID, "code": code})
	req := httptest.NewRequest("POST", "/api/v4/auth/login/2fa", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	LoginTOTPStepUp(c)
	var resp loginStepUpEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// resetTOTPStepUpThrottle clears the shared stepUpThrottle counter for the
// test-harness client IP, so one test's failed attempts never bleed into the
// next (httptest.NewRequest gives every request the same synthetic IP).
func resetTOTPStepUpThrottle() {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x", nil)
	stepUpThrottle.Reset(c.ClientIP())
}

// TestLoginStepUpFullFlow drives password login → 402 challenge → correct
// TOTP code → token, for a TOTP-bound user.
func TestLoginStepUpFullFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetTOTPStepUpThrottle()
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepup-full", model.GENERAL, false)
	user := newReloadableUser(fixture)
	secret, _ := httpEnrollTOTP(t, user)

	_, first := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if first.Code != 402 || !first.Data.NeedSecondFactor || first.Data.ChallengeID == "" {
		t.Fatalf("password login for a TOTP-bound user must park at a 402 challenge, got %+v", first)
	}
	if first.Data.Token != "" {
		t.Fatal("a 402 step-up response must never carry a token")
	}
	if first.Data.MethodID != totp.BuiltinMethodID {
		t.Fatalf("method_id = %q, want %q", first.Data.MethodID, totp.BuiltinMethodID)
	}

	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	_, second := doLoginStepUp(first.Data.ChallengeID, code)
	if second.Code != 200 || second.Data.Token == "" {
		t.Fatalf("step-up with the correct code must mint a token, got %+v", second)
	}
}

// TestLoginStepUpWrongCodeIsRejectedAndThrottled proves a wrong code never
// mints a token, and repeated wrong attempts eventually 429 (the dedicated
// stepUpThrottle, independent of the password path's throttle).
func TestLoginStepUpWrongCodeIsRejectedAndThrottled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetTOTPStepUpThrottle()
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepup-wrongcode", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	_, first := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if first.Code != 402 || first.Data.ChallengeID == "" {
		t.Fatalf("expected a 402 challenge, got %+v", first)
	}

	var last loginStepUpEnvelope
	for i := 0; i < 10; i++ {
		_, last = doLoginStepUp(first.Data.ChallengeID, "000000")
		if last.Code == 429 {
			break
		}
	}
	if last.Code != 429 {
		t.Fatalf("repeated wrong codes must eventually 429, last=%+v", last)
	}
}

// TestLoginStepUpRecoveryCodeWorksOnce proves a recovery code completes the
// step-up and is then single-use.
func TestLoginStepUpRecoveryCodeWorksOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetTOTPStepUpThrottle()
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepup-recovery", model.GENERAL, false)
	user := newReloadableUser(fixture)
	_, codes := httpEnrollTOTP(t, user)

	_, first := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if first.Code != 402 {
		t.Fatalf("expected a 402 challenge, got %+v", first)
	}
	_, second := doLoginStepUp(first.Data.ChallengeID, codes[0])
	if second.Code != 200 || second.Data.Token == "" {
		t.Fatalf("step-up with a valid recovery code must mint a token, got %+v", second)
	}

	// The same recovery code cannot complete a SECOND login.
	_, thirdBegin := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if thirdBegin.Code != 402 {
		t.Fatalf("expected another 402 challenge, got %+v", thirdBegin)
	}
	_, fourth := doLoginStepUp(thirdBegin.Data.ChallengeID, codes[0])
	if fourth.Code == 200 {
		t.Fatal("a recovery code must not be reusable across logins")
	}
}

// TestLoginStepUpDegradesWhenBuiltinDisabled proves disabling builtin.totp
// makes password login mint a token DIRECTLY, even for a user who still has
// OtpSecret confirmed — the design's explicit "kill switch" requirement.
func TestLoginStepUpDegradesWhenBuiltinDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepup-disabled", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	// Sanity: enabled → still gated.
	_, gated := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if gated.Code != 402 {
		t.Fatalf("precondition: enabled builtin.totp must still gate login, got %+v", gated)
	}

	if err := extension.DefaultRegistry.Disable(totp.BuiltinMethodID); err != nil {
		t.Fatalf("disable builtin.totp: %v", err)
	}
	_, degraded := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if degraded.Code != 200 || degraded.Data.Token == "" {
		t.Fatalf("disabled builtin.totp must degrade login to no second factor, got %+v", degraded)
	}

	if err := extension.DefaultRegistry.Enable(totp.BuiltinMethodID); err != nil {
		t.Fatalf("re-enable builtin.totp: %v", err)
	}
	_, restored := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if restored.Code != 402 {
		t.Fatalf("re-enabled builtin.totp must gate login again, got %+v", restored)
	}
}

// TestLoginStepUpUserWithoutTOTPUnaffected proves a plain (non-enrolled) user
// logs in straight through, unaffected by the builtin.totp registration.
func TestLoginStepUpUserWithoutTOTPUnaffected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepup-nototp", model.GENERAL, false)

	_, resp := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("a user with no TOTP bound must log in directly, got %+v", resp)
	}
}

// TestLoginStepUpWrongCodeAuditsRealActor is the red-team I3 regression at
// the HTTP layer: a failed step-up attempt must be audited against the
// REAL account the challenge was bound to, not an anonymous (uid=0) actor —
// server/handles/login_stepup.go now resolves result.Username (populated by
// totp.Service.FinishLogin even on failure) back to a real actorUID.
func TestLoginStepUpWrongCodeAuditsRealActor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetTOTPStepUpThrottle()
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepup-auditactor", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	audit.Start()

	_, first := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if first.Code != 402 {
		audit.Stop()
		t.Fatalf("expected a 402 challenge, got %+v", first)
	}
	if _, resp := doLoginStepUp(first.Data.ChallengeID, "000000"); resp.Code == 200 {
		audit.Stop()
		t.Fatalf("wrong code must not succeed, got %+v", resp)
	}
	audit.Stop() // flushes everything buffered before returning

	logs, total, err := db.GetAuditLogs(db.AuditLogFilter{ActorUID: fixture.ID, ActionPrefix: "auth.login"}, 1, 10)
	if err != nil {
		t.Fatalf("query audit logs: %v", err)
	}
	if total == 0 {
		t.Fatal("failed step-up must be audited against the real actorUID, found none")
	}
	found := false
	for _, l := range logs {
		if l.Actor == fixture.Username && l.Result == "failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a failure-result audit row with Actor=%q, got %+v", fixture.Username, logs)
	}
}

// TestLoginPendingSecondFactorIsAudited is the red-team M12 regression: a
// password-correct-but-2FA-pending attempt gets its OWN audited fact
// ("auth.login.second_factor_required", success=true) distinct from a
// rejected-credential "auth.login" failure — an operator must be able to
// tell the two apart in the trail.
func TestLoginPendingSecondFactorIsAudited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepup-pendingaudit", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	audit.Start()
	_, resp := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	audit.Stop()
	if resp.Code != 402 {
		t.Fatalf("expected a 402 challenge, got %+v", resp)
	}

	logs, total, err := db.GetAuditLogs(db.AuditLogFilter{ActorUID: fixture.ID, ActionPrefix: "auth.login.second_factor_required"}, 1, 10)
	if err != nil {
		t.Fatalf("query audit logs: %v", err)
	}
	if total == 0 {
		t.Fatal("a password-correct/2FA-pending attempt must be audited under its own action")
	}
	found := false
	for _, l := range logs {
		if l.Actor == fixture.Username && l.Result == "success" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a success-result auth.login.second_factor_required row with Actor=%q, got %+v", fixture.Username, logs)
	}
}
