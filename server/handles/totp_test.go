package handles

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/authn/totp"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	otptotp "github.com/pquerna/otp/totp"
)

// This file drives the authenticated TOTP self-management handlers
// (server/handles/totp.go, Phase C: internal TOTP second factor) end to end
// through the real HTTP handlers, mirroring passkey_manage_test.go's shape.

type totpBeginResp struct {
	Code int `json:"code"`
	Data struct {
		Secret     string `json:"secret"`
		OtpAuthURI string `json:"otpauth_uri"`
	} `json:"data"`
}

type totpConfirmResp struct {
	Code int `json:"code"`
	Data struct {
		RecoveryCodes []string `json:"recovery_codes"`
	} `json:"data"`
}

// reloadableUser is a tiny helper so a fixture's id is created once but every
// handler call in a test reloads a fresh *model.User — mirroring
// passkey_manage_test.go's repeated `reloaded, _ := op.GetUserById(user.ID)`
// (each mutating handler call needs a freshly-reloaded user, since
// currentManageableUser itself reloads from the DB rather than trusting a
// caller-held pointer that may now be stale).
type reloadableUser struct{ id uint }

func (r reloadableUser) get(t *testing.T) *model.User {
	t.Helper()
	u, err := op.GetUserById(r.id)
	if err != nil {
		t.Fatalf("reload user %d: %v", r.id, err)
	}
	return u
}

func newReloadableUser(u *model.User) reloadableUser {
	return reloadableUser{id: u.ID}
}

// httpEnrollTOTP drives begin+confirm through the real HTTP handlers for
// user, returning the confirmed secret and the one-time recovery-code batch.
func httpEnrollTOTP(t *testing.T, user reloadableUser) (secret string, recoveryCodes []string) {
	t.Helper()

	bw, bc := authedCtx(t, user.get(t), nil, nil)
	TOTPBegin(bc)
	var begin totpBeginResp
	if err := json.Unmarshal(bw.Body.Bytes(), &begin); err != nil {
		t.Fatalf("decode begin: %v", err)
	}
	if begin.Code != 200 || begin.Data.Secret == "" {
		t.Fatalf("begin should return a pending secret: %s", bw.Body.String())
	}

	code, err := otptotp.GenerateCode(begin.Data.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	cw, cc := authedCtx(t, user.get(t), map[string]any{"code": code}, nil)
	TOTPConfirm(cc)
	var confirm totpConfirmResp
	if err := json.Unmarshal(cw.Body.Bytes(), &confirm); err != nil {
		t.Fatalf("decode confirm: %v", err)
	}
	if confirm.Code != 200 || len(confirm.Data.RecoveryCodes) != op.RecoveryCodeCount {
		t.Fatalf("confirm should return %d recovery codes: %s", op.RecoveryCodeCount, cw.Body.String())
	}
	return begin.Data.Secret, confirm.Data.RecoveryCodes
}

// TestTOTPEnrollUnbindCycle drives begin→confirm→delete through the real HTTP
// handlers: confirm persists OtpSecret and mints recovery codes; delete
// (proven with a fresh TOTP code) clears both.
type totpStatusResp struct {
	Code int `json:"code"`
	Data struct {
		Enabled                bool `json:"enabled"`
		RecoveryCodesRemaining int  `json:"recovery_codes_remaining"`
		RecoveryCodesTotal     int  `json:"recovery_codes_total"`
	} `json:"data"`
}

// TestTOTPStatusReflectsEnrollmentAndConsumption is the M10 regression: status
// reports disabled/zero before enrollment, enabled/10-of-10 right after
// confirm, and the remaining count drops after a recovery code is consumed
// (via an admin reset — cheapest way to consume one without a full login).
func TestTOTPStatusReflectsEnrollmentAndConsumption(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-status", model.GENERAL, false)
	user := newReloadableUser(fixture)

	sw, sc := authedCtx(t, user.get(t), nil, nil)
	TOTPStatus(sc)
	var before totpStatusResp
	_ = json.Unmarshal(sw.Body.Bytes(), &before)
	if before.Code != 200 || before.Data.Enabled {
		t.Fatalf("status before enrollment must report disabled, got %+v", before)
	}

	_, codes := httpEnrollTOTP(t, user)

	sw2, sc2 := authedCtx(t, user.get(t), nil, nil)
	TOTPStatus(sc2)
	var after totpStatusResp
	_ = json.Unmarshal(sw2.Body.Bytes(), &after)
	if !after.Data.Enabled || after.Data.RecoveryCodesRemaining != op.RecoveryCodeCount || after.Data.RecoveryCodesTotal != op.RecoveryCodeCount {
		t.Fatalf("status after enrollment must report enabled + full recovery batch, got %+v", after)
	}

	if err := op.ConsumeRecoveryCode(fixture.ID, codes[0]); err != nil {
		t.Fatalf("consume one recovery code: %v", err)
	}
	sw3, sc3 := authedCtx(t, user.get(t), nil, nil)
	TOTPStatus(sc3)
	var afterConsume totpStatusResp
	_ = json.Unmarshal(sw3.Body.Bytes(), &afterConsume)
	if afterConsume.Data.RecoveryCodesRemaining != op.RecoveryCodeCount-1 || afterConsume.Data.RecoveryCodesTotal != op.RecoveryCodeCount {
		t.Fatalf("status after consuming one code = %+v, want remaining=%d total=%d",
			afterConsume, op.RecoveryCodeCount-1, op.RecoveryCodeCount)
	}
}

func TestTOTPEnrollUnbindCycle(t *testing.T) {
	pinWebauthnRP(t) // reuses the same gin.SetMode(gin.TestMode) as passkey tests
	fixture := mustAuthNUser(t, "totp-cycle", model.GENERAL, false)
	user := newReloadableUser(fixture)

	secret, codes := httpEnrollTOTP(t, user)
	if len(codes) == 0 {
		t.Fatal("expected recovery codes")
	}

	reloaded := user.get(t)
	if reloaded.OtpSecret != secret {
		t.Fatalf("OtpSecret not persisted: got %q want %q", reloaded.OtpSecret, secret)
	}

	// Unbind with a fresh current code.
	code, err := otptotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	dw, dc := authedCtx(t, user.get(t), map[string]any{"code": code}, nil)
	TOTPDelete(dc)
	if decodeManage(dw).Code != 200 {
		t.Fatalf("delete should succeed: %s", dw.Body.String())
	}

	final := user.get(t)
	if final.OtpSecret != "" {
		t.Fatalf("OtpSecret must be cleared after unbind, got %q", final.OtpSecret)
	}
	total, remaining, err := op.RecoveryCodeStatus(final.ID)
	if err != nil {
		t.Fatalf("recovery code status: %v", err)
	}
	if total != 0 || remaining != 0 {
		t.Fatalf("recovery codes must be cleared after unbind, got total=%d remaining=%d", total, remaining)
	}
}

// TestTOTPConfirmWrongCodeRejected proves a wrong code never enables TOTP.
func TestTOTPConfirmWrongCodeRejected(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-wrongcode", model.GENERAL, false)
	user := newReloadableUser(fixture)

	bw, bc := authedCtx(t, user.get(t), nil, nil)
	TOTPBegin(bc)
	var begin totpBeginResp
	_ = json.Unmarshal(bw.Body.Bytes(), &begin)
	if begin.Code != 200 {
		t.Fatalf("begin should succeed: %s", bw.Body.String())
	}

	cw, cc := authedCtx(t, user.get(t), map[string]any{"code": "000000"}, nil)
	TOTPConfirm(cc)
	if decodeManage(cw).Code == 200 {
		t.Fatalf("wrong code must not confirm: %s", cw.Body.String())
	}
	if reloaded := user.get(t); reloaded.OtpSecret != "" {
		t.Fatal("OtpSecret must not be set after a failed confirm")
	}
}

// TestTOTPDeleteWithWrongProofRejected proves unbind refuses without a valid
// code or password, and the secret survives the attempt.
func TestTOTPDeleteWithWrongProofRejected(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-wrongproof", model.GENERAL, false)
	user := newReloadableUser(fixture)
	secret, _ := httpEnrollTOTP(t, user)

	dw, dc := authedCtx(t, user.get(t), map[string]any{"code": "000000"}, nil)
	TOTPDelete(dc)
	if decodeManage(dw).Code == 200 {
		t.Fatalf("wrong code must not unbind: %s", dw.Body.String())
	}
	if reloaded := user.get(t); reloaded.OtpSecret != secret {
		t.Fatal("OtpSecret must survive a failed unbind attempt")
	}
}

// TestTOTPDeleteWrongPasswordButCorrectCodeStillSucceeds is the red-team M11
// regression: BOTH fields may be submitted, and either one verifying is
// enough — a wrong password must not short-circuit a valid code the caller
// also supplied.
func TestTOTPDeleteWrongPasswordButCorrectCodeStillSucceeds(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-m11-fallback", model.GENERAL, false)
	user := newReloadableUser(fixture)
	_, codes := httpEnrollTOTP(t, user)

	dw, dc := authedCtx(t, user.get(t), map[string]any{
		"password": "definitely-the-wrong-password",
		"code":     codes[0],
	}, nil)
	TOTPDelete(dc)
	if decodeManage(dw).Code != 200 {
		t.Fatalf("a wrong password alongside a VALID code must still unbind, got %s", dw.Body.String())
	}
	if reloaded := user.get(t); reloaded.OtpSecret != "" {
		t.Fatal("OtpSecret must be cleared once the code verified, despite the wrong password")
	}
}

// TestTOTPDeleteWithPassword proves the password fallback works for unbind.
func TestTOTPDeleteWithPassword(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-pwdelete", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	dw, dc := authedCtx(t, user.get(t), map[string]any{"password": "pw-totp-pwdelete"}, nil)
	TOTPDelete(dc)
	if decodeManage(dw).Code != 200 {
		t.Fatalf("delete with correct password should succeed: %s", dw.Body.String())
	}
	if reloaded := user.get(t); reloaded.OtpSecret != "" {
		t.Fatal("OtpSecret must be cleared after password-verified unbind")
	}
}

// TestTOTPRecoveryCodeUnbindSucceeds proves unbinding with a recovery code
// works (VerifyBoundCode's recovery-code fallback).
func TestTOTPRecoveryCodeUnbindSucceeds(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-recoveryunbind", model.GENERAL, false)
	user := newReloadableUser(fixture)
	_, codes := httpEnrollTOTP(t, user)

	dw, dc := authedCtx(t, user.get(t), map[string]any{"code": codes[0]}, nil)
	TOTPDelete(dc)
	if decodeManage(dw).Code != 200 {
		t.Fatalf("delete with a valid recovery code should succeed: %s", dw.Body.String())
	}
}

// TestTOTPRegenerateRecoveryCodesInvalidatesOld proves regenerate replaces the
// whole batch: an old code no longer works as an unbind proof, a new one does.
func TestTOTPRegenerateRecoveryCodesInvalidatesOld(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-regen", model.GENERAL, false)
	user := newReloadableUser(fixture)
	_, oldCodes := httpEnrollTOTP(t, user)

	rw, rc := authedCtx(t, user.get(t), nil, nil)
	TOTPRegenerateRecoveryCodes(rc)
	var regen totpConfirmResp
	if err := json.Unmarshal(rw.Body.Bytes(), &regen); err != nil {
		t.Fatalf("decode regenerate: %v", err)
	}
	if regen.Code != 200 || len(regen.Data.RecoveryCodes) != op.RecoveryCodeCount {
		t.Fatalf("regenerate should return a fresh full batch: %s", rw.Body.String())
	}
	if regen.Data.RecoveryCodes[0] == oldCodes[0] {
		t.Fatal("regenerated batch must not reuse the old codes")
	}

	// Old code no longer verifies as an unbind proof.
	dw, dc := authedCtx(t, user.get(t), map[string]any{"code": oldCodes[0]}, nil)
	TOTPDelete(dc)
	if decodeManage(dw).Code == 200 {
		t.Fatal("an old, regenerated-away recovery code must not unbind")
	}

	// New code does.
	dw2, dc2 := authedCtx(t, user.get(t), map[string]any{"code": regen.Data.RecoveryCodes[0]}, nil)
	TOTPDelete(dc2)
	if decodeManage(dw2).Code != 200 {
		t.Fatalf("a current recovery code must unbind: %s", dw2.Body.String())
	}
}

// TestTOTPGuestForbidden pins the guest refusal across the whole surface.
func TestTOTPGuestForbidden(t *testing.T) {
	guest := &model.User{Username: "guest-totp", Role: model.Roles{model.GUEST}}
	bw, bc := authedCtx(t, guest, nil, nil)
	TOTPBegin(bc)
	if decodeManage(bw).Code != 403 {
		t.Fatalf("guest begin must be 403, got body=%s", bw.Body.String())
	}
}

// TestTOTPBeginRefusesAlreadyEnabled proves a second begin (without unbinding)
// is refused.
func TestTOTPBeginRefusesAlreadyEnabled(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-doubleenroll", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	bw, bc := authedCtx(t, user.get(t), nil, nil)
	TOTPBegin(bc)
	if decodeManage(bw).Code == 200 {
		t.Fatalf("begin must refuse an already-enrolled user: %s", bw.Body.String())
	}
}

// TestTOTPConfirmSetupUsesSharedStore is a wiring sanity check: the handler
// and the process-wide totp.Default service must be the SAME store — a
// ConfirmSetup called directly on Default must see the pending setup the
// handler just began (if the handler used a private Service instance, this
// would miss with ErrSetupNotFound).
func TestTOTPConfirmSetupUsesSharedStore(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-wiring", model.GENERAL, false)
	user := newReloadableUser(fixture)

	bw, bc := authedCtx(t, user.get(t), nil, nil)
	TOTPBegin(bc)
	var begin totpBeginResp
	_ = json.Unmarshal(bw.Body.Bytes(), &begin)
	if begin.Code != 200 {
		t.Fatalf("begin should succeed: %s", bw.Body.String())
	}

	code, _ := otptotp.GenerateCode(begin.Data.Secret, time.Now())
	if _, err := totp.Default.ConfirmSetup(user.get(t), code); err != nil {
		t.Fatalf("Default.ConfirmSetup must see the handler's pending setup: %v", err)
	}
}

// TestTOTPConfirmThrottledAfterRepeatedWrongCodes is the red-team I4
// regression: repeated wrong codes against /me/2fa/totp/confirm for the SAME
// user eventually 429, via totpSelfThrottle keyed on user.ID (not client IP —
// there is no IP in these direct-handler-call tests at all).
func TestTOTPConfirmThrottledAfterRepeatedWrongCodes(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-selfthrottle-confirm", model.GENERAL, false)
	user := newReloadableUser(fixture)

	bw, bc := authedCtx(t, user.get(t), nil, nil)
	TOTPBegin(bc)
	var begin totpBeginResp
	_ = json.Unmarshal(bw.Body.Bytes(), &begin)
	if begin.Code != 200 {
		t.Fatalf("begin should succeed: %s", bw.Body.String())
	}

	var lastCode int
	for i := 0; i < 10; i++ {
		cw, cc := authedCtx(t, user.get(t), map[string]any{"code": "000000"}, nil)
		TOTPConfirm(cc)
		lastCode = decodeManage(cw).Code
		if lastCode == 429 {
			break
		}
	}
	if lastCode != 429 {
		t.Fatalf("repeated wrong confirm codes must eventually 429, last code=%d", lastCode)
	}
}

// TestTOTPDeleteThrottledAfterRepeatedWrongProof is the I4 regression for the
// unbind endpoint: repeated wrong proofs eventually 429, and — because
// regenerate shares the SAME per-user counter — regenerate is blocked too
// while the user is throttled.
func TestTOTPDeleteThrottledAfterRepeatedWrongProof(t *testing.T) {
	pinWebauthnRP(t)
	fixture := mustAuthNUser(t, "totp-selfthrottle-delete", model.GENERAL, false)
	user := newReloadableUser(fixture)
	httpEnrollTOTP(t, user)

	var lastCode int
	for i := 0; i < 10; i++ {
		dw, dc := authedCtx(t, user.get(t), map[string]any{"code": "000000"}, nil)
		TOTPDelete(dc)
		lastCode = decodeManage(dw).Code
		if lastCode == 429 {
			break
		}
	}
	if lastCode != 429 {
		t.Fatalf("repeated wrong unbind proofs must eventually 429, last code=%d", lastCode)
	}

	rw, rc := authedCtx(t, user.get(t), nil, nil)
	TOTPRegenerateRecoveryCodes(rc)
	if decodeManage(rw).Code != 429 {
		t.Fatalf("regenerate must also be blocked while the shared per-user throttle is tripped, got %s", rw.Body.String())
	}
}
