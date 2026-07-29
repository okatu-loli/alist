package sso

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ClaimNames maps alist's identity fields onto the id_token claim names an IdP
// actually emits. Most OIDC providers use the standard names (the zero value
// fills them in via withDefaults), but the mapping is configurable per provider
// for the ones that don't. The email_verified companion claim is always read
// from the standard "email_verified" and is not remapped.
type ClaimNames struct {
	Email    string // default "email"
	Username string // default "preferred_username"
	Name     string // default "name"
}

func (c ClaimNames) withDefaults() ClaimNames {
	if c.Email == "" {
		c.Email = "email"
	}
	if c.Username == "" {
		c.Username = "preferred_username"
	}
	if c.Name == "" {
		c.Name = "name"
	}
	return c
}

// OIDCConfig is everything needed to drive one OIDC provider. Endpoints and the
// signing keys are NOT listed here: they are discovered from Issuer's
// .well-known/openid-configuration on first use.
type OIDCConfig struct {
	Issuer       string   // e.g. "https://door.example.com" (must match discovery doc's issuer)
	ClientID     string   // OAuth2 client id; also the expected id_token audience
	ClientSecret string   // OAuth2 client secret
	RedirectURL  string   // the exact callback URL registered at the IdP
	Scopes       []string // default {openid, profile, email}; openid is force-included
	Claims       ClaimNames
}

// OIDCProvider is the OIDC implementation of FederatedProvider. Discovery
// (endpoints + JWKS) is performed lazily on first use and cached, so a provider
// can be constructed without network access and only reaches out to the IdP when
// a login actually happens. Safe for concurrent use.
type OIDCProvider struct {
	cfg        OIDCConfig
	httpClient *http.Client // optional; used for discovery, token exchange and JWKS

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth2   *oauth2.Config
}

// Option customizes an OIDCProvider at construction.
type Option func(*OIDCProvider)

// WithHTTPClient makes the provider use client for discovery, token exchange and
// JWKS fetches instead of the ambient default. Primarily for tests (pointing at a
// mock IdP) and for pinning a proxy-free transport.
func WithHTTPClient(client *http.Client) Option {
	return func(p *OIDCProvider) { p.httpClient = client }
}

// NewOIDCProvider validates cfg and returns a ready (but not yet discovered)
// provider. It does no network I/O; discovery happens on the first BuildAuthURL
// or ExchangeAndVerify.
func NewOIDCProvider(cfg OIDCConfig, opts ...Option) (*OIDCProvider, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, fmt.Errorf("sso: OIDC issuer is empty")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, fmt.Errorf("sso: OIDC client_id is empty")
	}
	if strings.TrimSpace(cfg.RedirectURL) == "" {
		return nil, fmt.Errorf("sso: OIDC redirect_url is empty")
	}
	cfg.Claims = cfg.Claims.withDefaults()
	cfg.Scopes = ensureOpenID(cfg.Scopes)
	p := &OIDCProvider{cfg: cfg}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// ensureOpenID returns scopes with the mandatory "openid" scope present exactly
// once, defaulting an empty list to the conventional openid/profile/email set.
func ensureOpenID(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{oidc.ScopeOpenID, "profile", "email"}
	}
	for _, s := range scopes {
		if s == oidc.ScopeOpenID {
			return scopes
		}
	}
	return append([]string{oidc.ScopeOpenID}, scopes...)
}

// clientContext returns ctx carrying the provider's HTTP client (if any), so
// go-oidc and oauth2 both route discovery, JWKS and token requests through it.
// go-oidc.ClientContext stores under the same key oauth2 reads, so one call
// covers every downstream request.
func (p *OIDCProvider) clientContext(ctx context.Context) context.Context {
	if p.httpClient != nil {
		return oidc.ClientContext(ctx, p.httpClient)
	}
	return ctx
}

// ensure performs (once) the OIDC discovery against the issuer and builds the
// verifier and oauth2 config from the discovered endpoints and JWKS. Subsequent
// calls are no-ops. The verifier is built with the client-carrying context so
// its lazy JWKS fetch uses the same transport.
func (p *OIDCProvider) ensure(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return nil
	}
	dctx := p.clientContext(ctx)
	prov, err := oidc.NewProvider(dctx, p.cfg.Issuer)
	if err != nil {
		return fmt.Errorf("sso: OIDC discovery for %q failed: %w", p.cfg.Issuer, err)
	}
	p.provider = prov
	p.verifier = prov.VerifierContext(dctx, &oidc.Config{ClientID: p.cfg.ClientID})
	p.oauth2 = &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  p.cfg.RedirectURL,
		Scopes:       p.cfg.Scopes,
	}
	return nil
}

// BuildAuthURL implements FederatedProvider. It triggers discovery if needed and
// returns the authorization URL carrying response_type=code, client_id,
// redirect_uri, scope, state, nonce and the S256 PKCE challenge.
func (p *OIDCProvider) BuildAuthURL(state, nonce, pkceChallenge string) (string, error) {
	if err := p.ensure(context.Background()); err != nil {
		return "", err
	}
	return p.oauth2.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", pkceChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	), nil
}

// ExchangeAndVerify implements FederatedProvider. It redeems code at the token
// endpoint (presenting pkceVerifier), verifies the returned id_token's signature,
// issuer, audience and expiry via go-oidc, then explicitly checks the nonce claim
// equals expectedNonce before mapping claims to a FederatedIdentity. Any failed
// check yields an error and no identity.
func (p *OIDCProvider) ExchangeAndVerify(ctx context.Context, code, pkceVerifier, expectedNonce string) (FederatedIdentity, error) {
	if err := p.ensure(ctx); err != nil {
		return FederatedIdentity{}, err
	}
	rctx := p.clientContext(ctx)

	tok, err := p.oauth2.Exchange(rctx, code, oauth2.SetAuthURLParam("code_verifier", pkceVerifier))
	if err != nil {
		return FederatedIdentity{}, fmt.Errorf("sso: token exchange failed: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return FederatedIdentity{}, fmt.Errorf("sso: token response has no id_token")
	}

	idToken, err := p.verifier.Verify(rctx, rawIDToken)
	if err != nil {
		return FederatedIdentity{}, fmt.Errorf("sso: id_token verification failed: %w", err)
	}
	// Explicit nonce check: go-oidc verifies signature/iss/aud/exp but leaves the
	// nonce to the caller. This is the id_token replay defense — the token must
	// carry the exact nonce we minted for this state.
	if idToken.Nonce != expectedNonce {
		return FederatedIdentity{}, fmt.Errorf("sso: id_token nonce mismatch")
	}

	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return FederatedIdentity{}, fmt.Errorf("sso: cannot parse id_token claims: %w", err)
	}
	return FederatedIdentity{
		Issuer:        idToken.Issuer,
		Subject:       idToken.Subject,
		Email:         stringClaim(claims, p.cfg.Claims.Email),
		EmailVerified: boolClaim(claims, "email_verified"),
		Username:      stringClaim(claims, p.cfg.Claims.Username),
		Name:          stringClaim(claims, p.cfg.Claims.Name),
	}, nil
}

// stringClaim extracts a string-valued claim by name, returning "" if absent or
// not a string.
func stringClaim(m map[string]json.RawMessage, key string) string {
	if key == "" {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// boolClaim extracts a boolean-valued claim by name. It tolerates providers that
// encode email_verified as the string "true"/"false" instead of a JSON boolean.
func boolClaim(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "true"
	}
	return false
}
