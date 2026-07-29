package totp

import (
	"context"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/op"
	otptotp "github.com/pquerna/otp/totp"
)

// TestMethodInfoIsSecondFactor pins the classification the login orchestrator
// (server/handles/authn.go) relies on to refuse this method as a standalone
// login at the anonymous /auth/method/:id/begin|finish endpoints.
func TestMethodInfoIsSecondFactor(t *testing.T) {
	m := newAuthNMethod(NewService())
	info := m.MethodInfo()
	if info.Kind != ext.AuthSecondFactor {
		t.Fatalf("MethodInfo().Kind = %v, want ext.AuthSecondFactor", info.Kind)
	}
	if info.ID != BuiltinMethodID {
		t.Fatalf("MethodInfo().ID = %q, want %q", info.ID, BuiltinMethodID)
	}
}

// TestAuthNMethodBeginFinishRoundTrip drives the adapter exactly as the login
// orchestrator would: mint a principal for an already-password-verified user,
// Begin a challenge, then Finish it with the user's current TOTP code.
func TestAuthNMethodBeginFinishRoundTrip(t *testing.T) {
	svc := NewService()
	m := newAuthNMethod(svc)
	user := newTestUser(t)

	secret, _, err := svc.BeginSetup(user)
	if err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	code, _ := otptotp.GenerateCode(secret, time.Now())
	if _, err := svc.ConfirmSetup(user, code); err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		t.Fatalf("persist secret: %v", err)
	}

	principal := ext.Principal(svc.IssuePrincipal(user))
	challenge, err := m.Begin(context.Background(), principal, ext.AuthNRequest{})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if challenge.ChallengeID == "" {
		t.Fatal("Begin must return a non-empty challenge id")
	}

	loginCode, _ := otptotp.GenerateCode(secret, time.Now())
	result, err := m.Finish(context.Background(), ext.AuthNResponse{
		ChallengeID: challenge.ChallengeID,
		Params:      map[string]string{"code": loginCode},
	})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !result.Verified || result.Username != user.Username {
		t.Fatalf("Finish result = %+v, want Verified=true Username=%q", result, user.Username)
	}
}

// TestAuthNMethodFinishWrongCodeIsCleanRejection proves a wrong code never
// surfaces as an error — only Verified=false — so the host's throttle
// accounting (which branches on Verified, not err) works.
func TestAuthNMethodFinishWrongCodeIsCleanRejection(t *testing.T) {
	svc := NewService()
	m := newAuthNMethod(svc)
	user := newTestUser(t)
	secret, _, _ := svc.BeginSetup(user)
	code, _ := otptotp.GenerateCode(secret, time.Now())
	if _, err := svc.ConfirmSetup(user, code); err != nil {
		t.Fatalf("confirm setup: %v", err)
	}
	user.OtpSecret = secret
	if err := op.UpdateUser(user); err != nil {
		t.Fatalf("persist secret: %v", err)
	}

	principal := ext.Principal(svc.IssuePrincipal(user))
	challenge, err := m.Begin(context.Background(), principal, ext.AuthNRequest{})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	result, err := m.Finish(context.Background(), ext.AuthNResponse{
		ChallengeID: challenge.ChallengeID,
		Params:      map[string]string{"code": "000000"},
	})
	if err != nil {
		t.Fatalf("Finish must not error on a wrong code, got %v", err)
	}
	if result.Verified {
		t.Fatal("wrong code must not verify")
	}
}

// TestRegisterBuiltinResolvesTrusted proves RegisterBuiltin wires the adapter
// into the extension registry as a trusted in-process instance, resolvable by
// its id — the same registration seam builtin.passkey uses.
func TestRegisterBuiltinResolvesTrusted(t *testing.T) {
	t.Cleanup(func() { extension.RegisterTrustedAuthNMethodInstance(BuiltinMethodID, nil) })

	RegisterBuiltin()

	m, release, err := extension.DefaultRegistry.GetAuthNMethod(BuiltinMethodID)
	if err != nil || m == nil {
		t.Fatalf("GetAuthNMethod(%q) = (%v, %v), want a resolved method", BuiltinMethodID, m, err)
	}
	release()
	if !extension.DefaultRegistry.AuthNMethodTrusted(BuiltinMethodID) {
		t.Fatal("builtin TOTP must be registered as a trusted instance")
	}
	if m.MethodInfo().Kind != ext.AuthSecondFactor {
		t.Fatal("resolved method must report AuthSecondFactor")
	}
}
