// Package sso is the pure federated-login engine backing alist's built-in
// single sign-on. It owns the OAuth2 authorization-code + OIDC id_token flow,
// the short-lived state store, and provider presets. It performs no HTTP
// handling and no persistence of its own: callers drive Begin (BuildAuthURL) and
// the callback (ExchangeAndVerify), then resolve the returned FederatedIdentity
// to an alist user themselves.
//
// The FederatedProvider abstraction is deliberately protocol-neutral so a future
// SAML implementation can plug in beside the OIDC one and feed the same identity
// resolution / token issuance downstream.
package sso

import "context"

// FederatedIdentity is the normalized identity an external IdP asserts about the
// end user, produced after a federated login is fully verified. It is the single
// hand-off type every FederatedProvider (OIDC today, SAML later) must yield.
//
//   - Issuer/Subject are the stable, non-forgeable link key: alist links users by
//     "<provider_key>:<Subject>", never by Email alone (email is spoofable).
//   - Email is only trustworthy for first-login linking when EmailVerified is true.
//   - Username/Name are cosmetic profile hints for provisioning.
type FederatedIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Username      string
	Name          string
}

// FederatedProvider is one configured external identity provider. It builds the
// browser-facing authorization redirect and, on callback, exchanges the code and
// verifies the result into a FederatedIdentity.
//
// Implementations MUST fully validate the assertion (signature, issuer, audience,
// expiry, and the caller-supplied nonce) inside ExchangeAndVerify before
// returning; a non-nil error means the login is rejected and no identity is
// trustworthy. The three single-use secrets — state (CSRF), nonce (id_token
// replay), and the PKCE verifier (code interception) — are minted by the caller,
// parked in a StateStore, and threaded back in here.
type FederatedProvider interface {
	// BuildAuthURL returns the fully-formed authorization endpoint URL the user
	// agent should be redirected to. state, nonce and pkceChallenge are the
	// caller-minted single-use values; the caller must persist state→{nonce,
	// pkceVerifier,...} so the callback can reconcile them.
	BuildAuthURL(state, nonce, pkceChallenge string) (string, error)

	// ExchangeAndVerify redeems the authorization code at the token endpoint
	// (proving possession of pkceVerifier), verifies the returned assertion, and
	// checks its nonce equals expectedNonce. It returns the verified identity or
	// an error if any check fails.
	ExchangeAndVerify(ctx context.Context, code, pkceVerifier, expectedNonce string) (FederatedIdentity, error)
}
