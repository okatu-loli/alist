package ext

import "context"

// AuthProvider is a Kind=AuthProvider extension (SDK doc §1, WS-8): it turns an
// OAuth authorization-code dance into a durable token, with the code<->token
// exchange happening entirely INSIDE the plugin process — the plugin holds the
// vendor's client_id/secret and talks to the vendor's token endpoint itself.
// No external broker sits between the admin's AList instance and the storage
// vendor; that locality is WS-8's core requirement.
//
// AuthProvider is deliberately NOT embedded in StorageDriver: a plugin that
// only brokers OAuth for a network drive (or for the install wizard of a
// different plugin) declares Kind=AuthProvider on its own manifest and Extension
// value; a plugin that is both a storage driver and its own OAuth broker
// declares both Kinds and implements both interfaces on the same value.
type AuthProvider interface {
	Extension

	// AuthURL builds the vendor's authorization-page URL that the admin's
	// browser should be sent to. req.State is an opaque, host-generated
	// anti-CSRF token; the provider's only obligation is to make the vendor
	// echo it back unmodified as a query parameter on redirect (RFC 6749 §10.12)
	// — it never needs to interpret it.
	AuthURL(ctx context.Context, req AuthURLRequest) (string, error)

	// Exchange trades an authorization code for a token by calling the vendor's
	// token endpoint directly from inside the plugin process (using whatever
	// client_id/secret/etc the plugin was configured with). It never proxies
	// through a third-party broker — the exchange is local to this call.
	Exchange(ctx context.Context, req ExchangeRequest) (Token, error)

	// Refresh trades a refresh token for a fresh access token. Same locality
	// guarantee as Exchange: the vendor's token endpoint is called directly.
	Refresh(ctx context.Context, refreshToken string) (Token, error)
}

// AuthURLRequest carries the inputs AuthURL needs to build the vendor's
// authorization-page URL.
type AuthURLRequest struct {
	// State is the anti-CSRF token the provider must round-trip unmodified.
	State string `json:"state"`
	// RedirectURI is the host's own OAuth callback URL (points back at AList,
	// not at the provider or any broker). Exchange's RedirectURI must match it
	// exactly — most vendors require that.
	RedirectURI string `json:"redirect_uri"`
	// Scopes are the OAuth scopes requested, as declared by the install
	// wizard's oauth step (ext.WizardStep.Scopes).
	Scopes []string `json:"scopes,omitempty"`
}

// ExchangeRequest carries the inputs Exchange needs to trade an authorization
// code for a token.
type ExchangeRequest struct {
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
}

// Token is the provider-agnostic result of Exchange/Refresh: an opaque JSON
// blob (Raw) plus the few fields the host understands well enough to act on —
// surface it as a storage credential, know whether/when to refresh it. A
// provider may put vendor-specific fields (e.g. an account id) only in Raw; the
// host round-trips Raw verbatim as part of the resulting storage's Addition
// config.
type Token struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	ExpiresIn    int64          `json:"expires_in,omitempty"` // seconds; 0 = unknown/non-expiring
	TokenType    string         `json:"token_type,omitempty"`
	Raw          map[string]any `json:"raw,omitempty"`
}
