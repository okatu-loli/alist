package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/authn/totp"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
	otptotp "github.com/pquerna/otp/totp"
)

// This file drives the GENERIC second-factor step-up completion endpoint
// (server/handles/authn.go's AuthNMethodStepUpFinish, POST
// /auth/method/:id/stepup/finish — Phase F generalization: TOTP's
// /auth/login/2fa was the first, no-longer-only, caller of the underlying
// finishSecondFactor dispatch). It proves the endpoint works end to end
// through the SAME builtin.totp method login_stepup_test.go already exercises
// via the TOTP-specific alias, and that it correctly rejects a primary method
// (which has no step-up ceremony) and an unknown method id.

type stepUpFinishEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		DeviceKey    string `json:"device_key"`
	} `json:"data"`
}

func doAuthNStepUpFinish(id, challengeID string, params map[string]string) (*httptest.ResponseRecorder, stepUpFinishEnvelope) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{"challenge_id": challengeID, "params": params})
	req := httptest.NewRequest("POST", "/api/v4/auth/method/"+id+"/stepup/finish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: id}}
	stepUpThrottle.Reset(c.ClientIP())
	AuthNMethodStepUpFinish(c)
	var resp stepUpFinishEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// TestAuthNMethodStepUpFinishFullFlowTOTP drives password login → 402
// challenge → the GENERIC endpoint (not /auth/login/2fa) with the correct TOTP
// code → token, proving the generic dispatch works for the one wired
// second_factor method.
func TestAuthNMethodStepUpFinishFullFlowTOTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetTOTPStepUpThrottle()
	registerTOTPBuiltinFeature(t)
	fixture := mustAuthNUser(t, "stepupfinish-totp", model.GENERAL, false)
	user := newReloadableUser(fixture)
	secret, _ := httpEnrollTOTP(t, user)

	_, first := doLoginRaw(fixture.Username, "pw-"+fixture.Username)
	if first.Code != 402 || first.Data.ChallengeID == "" {
		t.Fatalf("password login for a TOTP-bound user must park at a 402 challenge, got %+v", first)
	}
	if first.Data.MethodID != totp.BuiltinMethodID {
		t.Fatalf("method_id = %q, want %q", first.Data.MethodID, totp.BuiltinMethodID)
	}

	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	_, second := doAuthNStepUpFinish(first.Data.MethodID, first.Data.ChallengeID, map[string]string{"code": code})
	if second.Code != 200 || second.Data.Token == "" {
		t.Fatalf("generic step-up finish with the correct code must mint a token, got %+v", second)
	}
}

// TestAuthNMethodStepUpFinishRejectsPrimaryMethod proves the generic endpoint
// is the mirror image of AuthNMethodBegin/Finish: a primary method has no
// step-up ceremony and must be refused with 400, never accidentally completed.
func TestAuthNMethodStepUpFinishRejectsPrimaryMethod(t *testing.T) {
	registerStubAuthN(t, &stubAuthN{id: "m.stepup.primary", kind: ext.AuthPrimary, verified: true, username: "irrelevant"})

	_, resp := doAuthNStepUpFinish("m.stepup.primary", "chal", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a primary method must not be completable at the step-up endpoint: %+v", resp)
	}
}

// TestAuthNMethodStepUpFinishRejectsFederatedMethod is the federated-kind twin
// of the primary-kind rejection above.
func TestAuthNMethodStepUpFinishRejectsFederatedMethod(t *testing.T) {
	registerStubAuthN(t, &stubAuthN{id: "m.stepup.federated", kind: ext.AuthFederated, verified: true, username: "irrelevant"})

	_, resp := doAuthNStepUpFinish("m.stepup.federated", "chal", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a federated method must not be completable at the step-up endpoint: %+v", resp)
	}
}

// TestAuthNMethodStepUpFinishUnknownMethodDenied is a clean 404, never a
// nil-deref, for an id nothing registered.
func TestAuthNMethodStepUpFinishUnknownMethodDenied(t *testing.T) {
	w, resp := doAuthNStepUpFinish("does.not.exist.stepup", "chal", nil)
	if resp.Code != 404 || resp.Data.Token != "" {
		t.Fatalf("unknown method must 404: code=%d body=%s", resp.Code, w.Body.String())
	}
}
