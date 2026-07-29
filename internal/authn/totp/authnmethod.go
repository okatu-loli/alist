package totp

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

// BuiltinMethodID is the AuthNMethod id under which the built-in TOTP second
// factor is registered. It is the id the extension registry addresses (and
// the id the manageable Feature entry in internal/bootstrap/core_features.go
// shares, exactly like builtin.passkey — see that file's registration note).
const BuiltinMethodID = "builtin.totp"

// authNMethod adapts Service to the ext.AuthNMethod contract so TOTP step-up
// flows through the SAME extension-registry seam any other login method
// does: MethodInfo reports it as a SECOND FACTOR, so the anonymous
// /auth/method/:id/begin|finish endpoints refuse it as a standalone login
// (server/handles/authn.go) — it only ever runs from the login orchestrator's
// step-up path (server/handles/auth.go's loginHash), after a password has
// already succeeded.
//
// Like every AuthNMethod it only PROVES possession of the second factor:
// Finish returns a verified username, never a token. The core alone mints the
// session.
type authNMethod struct {
	svc *Service
}

func newAuthNMethod(svc *Service) *authNMethod { return &authNMethod{svc: svc} }

// --- ext.Extension ---

func (a *authNMethod) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    BuiltinMethodID,
		Name:  map[string]string{"en": "Authenticator App (TOTP)", "zh": "身份验证器(TOTP)"},
		Kinds: []ext.Kind{ext.KindAuthNMethod},
	}
}

func (a *authNMethod) Init(ctx context.Context, h ext.Host) error { return nil }
func (a *authNMethod) Shutdown(ctx context.Context) error         { return nil }

// --- ext.AuthNMethod ---

// MethodInfo classifies the built-in TOTP method as a SECOND FACTOR: it never
// resolves identity on its own, only step-up after a password succeeds.
func (a *authNMethod) MethodInfo() ext.AuthMethodInfo {
	return ext.AuthMethodInfo{
		ID:      BuiltinMethodID,
		Kind:    ext.AuthSecondFactor,
		Display: map[string]string{"en": "Authenticator App", "zh": "身份验证器"},
	}
}

// Begin resolves the host-issued principal (minted by the login
// orchestrator's IssuePrincipal right after a password check succeeds) into a
// fresh login-challenge id. It carries no client-visible Data — the login
// page already knows to render a 6-digit code input; there is no ceremony
// payload to hand back, unlike WebAuthn's creation options.
func (a *authNMethod) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	challengeID, err := a.svc.BeginLogin(string(principal))
	if err != nil {
		return ext.AuthNChallenge{}, err
	}
	return ext.AuthNChallenge{ChallengeID: challengeID}, nil
}

// Finish verifies the client's code (TOTP or a recovery code) against the
// challenge's bound user and returns the resolved username. A wrong/replayed
// code, a stale (password-changed-since) challenge, and a since-unbound TOTP
// secret are all a clean Verified=false — the host counts each as one
// ordinary failed step-up attempt without learning which case it was.
//
// Username is populated on BOTH outcomes whenever FinishLogin resolved the
// challenge to a real user (red-team I3): the host's audit trail needs the
// actual account a failed attempt targeted, not a blank actor — this mirrors
// how authn.go's generic AuthNMethodFinish already treats a Username
// alongside Verified=false as valid input to audit.Event. Only a challenge
// that never resolved to any user (unknown/expired id) reports an empty
// username, since there is no account to attribute it to.
func (a *authNMethod) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	username, verified := a.svc.FinishLogin(req.ChallengeID, req.Params["code"])
	return ext.AuthNResult{Username: username, Verified: verified}, nil
}

// RegisterBuiltin registers the built-in TOTP second factor as a TRUSTED
// in-process AuthNMethod, using the process-wide Default service so a setup
// begun/confirmed via the self-service HTTP handlers (server/handles/totp.go)
// and a login step-up ceremony driven by the login orchestrator share the
// same stores. Trust is conferred HERE by the host (mirrors
// passkey.RegisterBuiltin) — though for a second_factor method it has no
// admin-login-carveout effect (that gate only ever applies to a
// PRIMARY/FEDERATED method minting an admin session outright); it is set for
// consistency with every other host-authored built-in.
func RegisterBuiltin() {
	extension.RegisterTrustedAuthNMethodInstance(BuiltinMethodID, newAuthNMethod(Default))
}
