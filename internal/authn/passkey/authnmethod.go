package passkey

import (
	"context"
	"encoding/json"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

// BuiltinMethodID is the AuthNMethod id under which the built-in passkey login is
// registered. It is the id the login page addresses (POST
// /api/v4/auth/method/builtin.passkey/{begin,finish}) and the id the host trusts.
const BuiltinMethodID = "builtin.passkey"

// authNMethod adapts the passkey Service to the ext.AuthNMethod contract so
// passwordless login flows through the SAME host orchestration as any other
// login method (per-IP throttle, disabled-user gate, challenge single-use,
// core-only token issuance). It is a PRIMARY method — a passwordless credential
// that resolves identity on its own — and, registered through the host's trusted
// built-in path, it is the one method permitted to serve admin logins.
//
// Like every AuthNMethod it only PROVES identity: Finish returns a verified
// username, never a token. The core alone mints the session.
type authNMethod struct {
	svc *Service
}

func newAuthNMethod(svc *Service) *authNMethod { return &authNMethod{svc: svc} }

// --- ext.Extension ---

func (a *authNMethod) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    BuiltinMethodID,
		Name:  map[string]string{"en": "Passkey", "zh": "通行密钥"},
		Kinds: []ext.Kind{ext.KindAuthNMethod},
	}
}

func (a *authNMethod) Init(ctx context.Context, h ext.Host) error { return nil }
func (a *authNMethod) Shutdown(ctx context.Context) error         { return nil }

// --- ext.AuthNMethod ---

// MethodInfo classifies the built-in passkey as a PRIMARY login method: it
// replaces the password as the initial authenticator and never auto-provisions
// (the host requires the resolved user to already exist).
func (a *authNMethod) MethodInfo() ext.AuthMethodInfo {
	return ext.AuthMethodInfo{
		ID:      BuiltinMethodID,
		Kind:    ext.AuthPrimary,
		Display: map[string]string{"en": "Sign in with a passkey", "zh": "使用通行密钥登录"},
	}
}

// Begin starts a discoverable-login ceremony. The host has already computed its
// public API URL and handed it back as req.RedirectURI, so the RP origin can be
// derived from it when settings/site_url do not pin the RP. The creation-request
// options are returned as the challenge Data (JSON bytes) for the browser.
func (a *authNMethod) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	base := deriveBaseURL(req.RedirectURI)
	id, options, err := a.svc.BeginLogin(base)
	if err != nil {
		return ext.AuthNChallenge{}, err
	}
	data, err := json.Marshal(options)
	if err != nil {
		return ext.AuthNChallenge{}, err
	}
	return ext.AuthNChallenge{ChallengeID: id, Data: data}, nil
}

// Finish verifies the browser's assertion (carried as the response Data bytes)
// and returns the resolved username. Any verification failure — a replayed or
// expired challenge, a bad/forged assertion, a clone-signalling counter, or a
// credential-ownership mismatch — is reported as a clean Verified=false rejection
// with a nil error, so the host counts it against the login throttle and answers
// invalid_credentials without leaking which specific check failed.
func (a *authNMethod) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	user, err := a.svc.FinishLogin(req.ChallengeID, req.Data)
	if err != nil {
		return ext.AuthNResult{Verified: false}, nil
	}
	return ext.AuthNResult{Username: user.Username, Verified: true}, nil
}

// deriveBaseURL reduces the host-supplied redirect URI (…/api/v4/auth/method/
// builtin.passkey/callback) to its origin so the RP can be derived from it as a
// last resort. Returns "" when it cannot be parsed; resolveRP then relies on
// settings/site_url.
func deriveBaseURL(redirectURI string) string {
	o, err := originOf(redirectURI)
	if err != nil {
		return ""
	}
	return o
}

// RegisterBuiltin registers the built-in passkey login as a TRUSTED in-process
// AuthNMethod, using the process-wide Default service so its login ceremonies
// share the challenge store with the HTTP registration/management endpoints.
// Trust is conferred HERE, by the host, via the trusted registration path — the
// method never self-asserts it — which is what lets the built-in passkey (and no
// subprocess/third-party method) serve admin logins.
func RegisterBuiltin() {
	extension.RegisterTrustedAuthNMethodInstance(BuiltinMethodID, newAuthNMethod(Default))
}
