package sso

import (
	"context"
	"fmt"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

// DefaultStateStore is the process-wide store of in-flight federated logins. The
// login-begin path (adapter.Begin / StartFlow) parks a State here under the
// opaque OAuth2 state id, and the browser callback (server/handles) Takes it
// exactly once. Sharing one store across both is what makes state single-use and
// nonce/PKCE reconciliation possible.
var DefaultStateStore = NewStateStore()

// StateTTL bounds how long a parked login may sit before its state expires. It
// spans the user's detour to the IdP and back; a callback later than this is
// rejected as replay/stale.
const StateTTL = 10 * time.Minute

// StartFlow begins one federated login: it builds (or reuses) the provider bound
// to redirectURL, mints the three single-use secrets (state, nonce, PKCE),
// parks them in DefaultStateStore under state, and returns the IdP authorization
// URL the browser must be sent to. linkUserID is 0 for a plain login and the
// initiating user's id for an account-linking flow (the callback binds the sub
// to that user rather than resolving by identity).
// StartFlow also returns the opaque state token it minted (== the ?state= param
// the callback looks up). A caller that must bind the flow to the initiating
// browser — the account link flow, to defeat link CSRF — sets it as an HttpOnly
// cookie the callback then requires to match; login callers ignore it.
//
// clientID is the initiating browser's Client-Id (empty is tolerated): parked in
// the state so the callback — which has no Client-Id header of its own — mints
// the session under the same device key the frontend's later XHRs will present.
func StartFlow(pc ProviderConfig, redirectURL string, remember bool, linkUserID uint, clientID string) (authURL, state string, err error) {
	prov, err := BuildProvider(pc, redirectURL)
	if err != nil {
		return "", "", err
	}
	state, err = GenerateState()
	if err != nil {
		return "", "", err
	}
	nonce, err := GenerateNonce()
	if err != nil {
		return "", "", err
	}
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return "", "", err
	}
	authURL, err = prov.BuildAuthURL(state, nonce, challenge)
	if err != nil {
		return "", "", err
	}
	DefaultStateStore.Put(state, State{
		ProviderKey:  pc.Key,
		Nonce:        nonce,
		PKCEVerifier: verifier,
		Remember:     remember,
		LinkUserID:   linkUserID,
		ClientID:     clientID,
		CreatedAt:    time.Now(),
	}, StateTTL)
	return authURL, state, nil
}

// providerMethod adapts one configured OIDC provider to the ext.AuthNMethod
// contract, so an SSO login button flows through the SAME host machinery
// (GetAuthNMethod resolution, the anonymous begin endpoint) as any other login
// method. It is a FEDERATED method: an unknown identity may auto-provision, but
// only behind the host's default-off gate and never as admin.
//
// Crucially, an SSO login does NOT complete through the generic Finish — the
// external IdP redirects the browser to a dedicated callback route that exchanges
// the code, verifies the id_token and resolves identity (server/handles/sso.go).
// Finish therefore always errors: there is no in-band challenge/response an SSO
// login could complete without the browser round-trip.
type providerMethod struct {
	pc ProviderConfig
}

func (m *providerMethod) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    MethodID(m.pc.Key),
		Name:  map[string]string{"en": m.pc.Name, "zh": m.pc.Name},
		Kinds: []ext.Kind{ext.KindAuthNMethod},
	}
}

func (m *providerMethod) Init(ctx context.Context, h ext.Host) error { return nil }
func (m *providerMethod) Shutdown(ctx context.Context) error         { return nil }

// MethodInfo classifies the provider as a FEDERATED login method and labels its
// login button with the operator-chosen provider name.
func (m *providerMethod) MethodInfo() ext.AuthMethodInfo {
	return ext.AuthMethodInfo{
		ID:      MethodID(m.pc.Key),
		Kind:    ext.AuthFederated,
		Display: map[string]string{"en": m.pc.Name, "zh": m.pc.Name},
	}
}

// Begin starts a plain (non-linking) login: it hands StartFlow the host-supplied
// callback URL (req.RedirectURI) and returns the IdP authorization URL as the
// challenge RedirectURL. The optional params["remember"] == "true" carries the
// "remember me" choice through the redirect, since the callback — not a Finish —
// completes the login and there is no later place to supply it. params["client_id"]
// is the host-injected browser Client-Id (see AuthNMethodBegin), parked so the
// callback mints the session under the frontend's device key.
func (m *providerMethod) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	authURL, _, err := StartFlow(m.pc, req.RedirectURI, req.Params["remember"] == "true", 0, req.Params["client_id"])
	if err != nil {
		return ext.AuthNChallenge{}, err
	}
	return ext.AuthNChallenge{RedirectURL: authURL}, nil
}

// Finish is never the SSO completion path: the browser callback route is. It
// exists only to satisfy the interface and always rejects, so a caller cannot
// smuggle an SSO login through the generic finish endpoint.
func (m *providerMethod) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	return ext.AuthNResult{}, fmt.Errorf("sso: federated login completes via the OIDC callback, not finish")
}

// RegisterProviderMethod registers (or replaces) the AuthNMethod for pc under its
// "sso:<key>" id via the host's TRUSTED built-in path. Trust here mirrors the
// built-in passkey: it is a host-side registration fact, never method-reported.
// It is a NECESSARY-only condition for admin login — the sufficient constraint
// (the admin has already linked this exact sub) is enforced independently in the
// callback's identity resolution, so a trusted-but-unlinked admin is still
// refused. Registering trusted is harmless for the generic finish path because
// this method's Finish always errors.
func RegisterProviderMethod(pc ProviderConfig) {
	extension.RegisterTrustedAuthNMethodInstance(MethodID(pc.Key), &providerMethod{pc: pc})
}

// DeregisterProviderMethod removes the AuthNMethod registered for key (a provider
// that was deleted or disabled) and drops its cached OIDC provider.
func DeregisterProviderMethod(key string) {
	extension.RegisterTrustedAuthNMethodInstance(MethodID(key), nil)
	InvalidateProvider(key)
}
