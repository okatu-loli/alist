package handles

import (
	"strconv"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// TestAdminCancel2FAResetAllowsLoginWithoutStepUp is the red-team C1
// regression: a user locked out of both their authenticator AND every
// recovery code has no self-service way back in (TOTPDelete always demands
// proof of possession). An admin's cancel_2fa clears OtpSecret + every
// recovery code with no proof, and the user can then log in with password
// alone — no 402 step-up.
func TestAdminCancel2FAResetAllowsLoginWithoutStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetTOTPStepUpThrottle()
	registerTOTPBuiltinFeature(t)
	admin := mustAuthNUser(t, "totp-admin-actor", model.ADMIN, false)
	fixture := mustAuthNUser(t, "totp-admin-reset-target", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	// Precondition: still gated.
	if _, gated := doLoginRaw(fixture.Username, "pw-"+fixture.Username); gated.Code != 402 {
		t.Fatalf("precondition: TOTP-bound user must still be gated, got %+v", gated)
	}

	w, c := authedCtx(t, admin, nil, gin.Params{{Key: "id", Value: strconv.Itoa(int(fixture.ID))}})
	AdminCancel2FA(c)
	if decodeManage(w).Code != 200 {
		t.Fatalf("admin cancel_2fa should succeed: %s", w.Body.String())
	}

	reloaded := user.get(t)
	if reloaded.OtpSecret != "" {
		t.Fatal("OtpSecret must be cleared by admin cancel_2fa")
	}
	total, remaining, err := op.RecoveryCodeStatus(reloaded.ID)
	if err != nil {
		t.Fatalf("recovery code status: %v", err)
	}
	if total != 0 || remaining != 0 {
		t.Fatalf("recovery codes must be cleared by admin cancel_2fa, got total=%d remaining=%d", total, remaining)
	}

	// The user can now log in with password alone.
	if _, resp := doLoginRaw(fixture.Username, "pw-"+fixture.Username); resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("post-reset login must succeed directly with no step-up, got %+v", resp)
	}
}
