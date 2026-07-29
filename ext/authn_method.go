package ext

import "context"

// AuthNMethod adds a way to authenticate a user beyond the core's built-in
// username/password path (design §2.2). It is the extension point behind SSO /
// OIDC, LDAP, TOTP and WebAuthn.
//
// The security contract is the whole point of this interface: the plugin only
// PROVES an identity — it returns "this identity was verified, here is the
// username/provision hint" — and the HOST alone decides whether to mint a
// session token, for whom, and with what privileges (design §0, §2.2). A plugin
// can never sign a token itself, and the host never unconditionally trusts a
// returned username: federated auto-provisioning is gated by an explicit,
// default-off policy, and no AuthNMethod path may mint an admin session.
//
// AuthNMethod is deliberately DISTINCT from AuthProvider (see manifest.go's Kind
// comments): AuthProvider brokers a storage vendor's OAuth token exchange (it
// produces a storage *credential*); AuthNMethod authenticates an AList *user*
// (it produces, at most, a host-issued session). They share no code path.
type AuthNMethod interface {
	Extension

	// MethodInfo reports how the host should classify and present this method.
	// It is the authoritative source for AuthKind: the login-flow orchestrator
	// consults it to route (primary/federated on the login page; second_factor
	// only after a password succeeds) and to refuse using a second_factor method
	// as a standalone login.
	MethodInfo() AuthMethodInfo

	// Begin starts a challenge: a WebAuthn options blob, an OIDC redirect URL, an
	// LDAP bind prompt, a TOTP setup URI. principal is empty for a fresh login
	// (primary/federated) and non-empty for step-up (a second factor on an
	// already-identified user); it is an opaque host-issued handle the plugin
	// cannot forge or introspect (design §3).
	Begin(ctx context.Context, principal Principal, req AuthNRequest) (AuthNChallenge, error)

	// Finish verifies the client's response and returns the resolved identity.
	// Returning Verified=false (with a nil error) is the normal "credentials
	// rejected" outcome; a non-nil error is reserved for operational failures
	// (IdP unreachable, malformed response). Finish must never be treated by the
	// host as authorization — only as an identity assertion the host then
	// independently gates.
	Finish(ctx context.Context, req AuthNResponse) (AuthNResult, error)
}

// Principal is an opaque, host-issued identity handle. Across the plugin
// boundary it is only a random string: a plugin cannot construct one, nor read
// a role/permission out of it. The host holds the handle→identity mapping. It
// appears here so a second_factor method's Begin can be scoped to the
// already-identified user without exposing that user's real identity to the
// plugin (design §3).
type Principal string

// AuthKind classifies where in the login flow a method participates.
type AuthKind uint8

const (
	// AuthPrimary replaces the password as the initial authenticator (LDAP, SSO
	// acting as the sole factor). The plugin is the trusted authenticator; per
	// design §5/§6.4 a primary method is native + signed + audited only, and its
	// consent screen is marked "can assert login identity (account-takeover
	// capability)".
	AuthPrimary AuthKind = iota
	// AuthSecondFactor is a step-up factor run AFTER a password succeeds (TOTP,
	// WebAuthn). Its Begin receives the already-identified Principal; it proves
	// possession, it does not resolve identity.
	AuthSecondFactor
	// AuthFederated is an external IdP (OIDC) that may auto-provision a new user
	// — but only behind the host's default-off policy gate, and never as an
	// admin (design §2.2, §6.1).
	AuthFederated
)

// AuthMethodInfo is the method's self-description.
type AuthMethodInfo struct {
	// ID identifies the method within the host's AuthNMethod registry
	// (extension.DefaultRegistry.GetAuthNMethod) — this is the single-id
	// addressing contract (design decision, Phase F): an extension that
	// declares login_methods in its Contributions MUST use its own manifest id
	// as this ID, and ValidateManifest enforces it at load time, so a
	// contributed method is always reachable at /auth/method/<manifest-id>/...
	// with no separate per-method id to disambiguate. One extension therefore
	// exposes at most one login_methods-contributed AuthNMethod.
	//
	// The one structural exception is the built-in SSO bridge
	// (internal/authn/sso): it registers one synthetic method per configured
	// IdP under "sso:<key>" directly via RegisterTrustedAuthNMethodInstance, and
	// never goes through a real Manifest or Contributions.LoginMethods — its own
	// PublicSSOProviders list serves the login page instead of the aggregated
	// login_methods contribution, so this constraint never applies to it.
	ID   string   `json:"id"`
	Kind AuthKind `json:"kind"`
	// Display is the i18n label the login button / step-up prompt renders.
	Display map[string]string `json:"display,omitempty"`
}

// AuthNRequest carries the host-supplied inputs to Begin.
type AuthNRequest struct {
	// RedirectURI is the host's own callback URL (points back at AList), for
	// federated flows whose IdP redirects the browser back. Mirrors
	// AuthURLRequest.RedirectURI.
	RedirectURI string `json:"redirect_uri,omitempty"`
	// State is an opaque, host-generated anti-CSRF token the plugin round-trips
	// unmodified through the external handshake (RFC 6749 §10.12); the plugin
	// never needs to interpret it.
	State string `json:"state,omitempty"`
	// Params carries method-specific client inputs the user supplied on the
	// login page (e.g. a typed LDAP username) as an opaque string map. Bytes and
	// credentials that should not be logged belong here, not in dedicated fields.
	Params map[string]string `json:"params,omitempty"`
}

// AuthNChallenge is what the host relays back to the client to continue (or
// complete) the handshake.
type AuthNChallenge struct {
	// ChallengeID correlates this Begin with the eventual Finish. The plugin may
	// set it to carry its own handshake state; if left empty the host mints one.
	ChallengeID string `json:"challenge_id,omitempty"`
	// RedirectURL, when set, is where the host sends the browser (federated /
	// OIDC authorization URL).
	RedirectURL string `json:"redirect_url,omitempty"`
	// Data is an opaque, method-specific payload (WebAuthn creation options, a
	// TOTP provisioning URI) the client-side component consumes, carried as JSON
	// bytes.
	Data []byte `json:"data,omitempty"`
}

// AuthNResponse carries the client's reply to a challenge back into Finish.
type AuthNResponse struct {
	// ChallengeID echoes the challenge this response answers.
	ChallengeID string `json:"challenge_id,omitempty"`
	// Params carries the client's response fields (OIDC code+state, a TOTP code,
	// an LDAP password, a WebAuthn assertion) as an opaque map.
	Params map[string]string `json:"params,omitempty"`
	// Data is an opaque, method-specific response payload as JSON bytes.
	Data []byte `json:"data,omitempty"`
}

// AuthNResult is the identity assertion Finish returns. It is an assertion, not
// an authorization: the host independently gates on it (existing/disabled user,
// admin cap, federated auto-provision policy) before any token is minted.
type AuthNResult struct {
	// Username is the resolved AList user the plugin asserts. For a primary
	// method the host requires this user to already exist; for a federated
	// method a non-existent user triggers the (default-off) auto-provision path.
	Username string `json:"username,omitempty"`
	// Verified is the plugin's assertion that the credential checked out. A
	// false value (with nil error) is a clean rejection; the host mints nothing.
	Verified bool `json:"verified"`
	// AutoProvision, when non-nil on a federated result, carries attributes for
	// creating/updating the user — honored ONLY when the host's auto-provision
	// policy is enabled, and never to grant admin.
	AutoProvision *ProvisionHint `json:"auto_provision,omitempty"`
}

// ProvisionHint is the attribute set a federated method suggests for a
// newly-provisioned user. The host decides which fields it honors; none of them
// can request a role or elevate privileges — the host assigns the configured
// minimal role and refuses to build an admin.
type ProvisionHint struct {
	DisplayName string            `json:"display_name,omitempty"`
	Email       string            `json:"email,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}
