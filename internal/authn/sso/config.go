package sso

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// MethodIDPrefix namespaces every configured SSO provider under its own
// AuthNMethod id ("sso:<key>"), so each provider is a distinct login button and
// a distinct method the host registers. A provider's link key in User.SsoID uses
// the bare key ("<key>:<subject>"), never this prefix.
const MethodIDPrefix = "sso:"

// MethodID is the AuthNMethod id under which the provider named key is
// registered and addressed (login button, begin/callback routes).
func MethodID(key string) string { return MethodIDPrefix + key }

// KeyFromMethodID recovers the provider key from an "sso:<key>" method id. The
// bool is false for an id that is not an SSO method id.
func KeyFromMethodID(id string) (string, bool) {
	if strings.HasPrefix(id, MethodIDPrefix) {
		key := id[len(MethodIDPrefix):]
		if key != "" {
			return key, true
		}
	}
	return "", false
}

// providerKeyRe constrains a provider key to a short, url- and id-safe token.
// The key is embedded in a method id, a route path segment, and the stored
// User.SsoID link key ("<key>:<subject>"), so it must never carry a ':' (which
// would confuse the link-key split) or path/whitespace characters.
var providerKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ProviderConfig is one operator-configured OIDC identity provider. It is the
// persisted, admin-editable shape (stored as a JSON list in the sso_providers
// setting); the pure engine's OIDCConfig is derived from it per request with the
// request-derived RedirectURL. ClientSecret is a write-only credential: the
// admin API never echoes it back.
type ProviderConfig struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Preset       string   `json:"preset,omitempty"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	// Claim name overrides; empty falls back to the OIDC standard names.
	ClaimEmail    string `json:"claim_email,omitempty"`
	ClaimUsername string `json:"claim_username,omitempty"`
	ClaimName     string `json:"claim_name,omitempty"`
	Enabled       bool   `json:"enabled"`
	// RedirectURL, when non-empty, pins the exact callback URL registered at the
	// IdP (for reverse-proxy setups whose public origin the request cannot be
	// trusted to reveal). Empty means derive it from the request per call.
	RedirectURL string `json:"redirect_url,omitempty"`
}

// Validate checks the operator-supplied fields required to drive a login. It
// does no network I/O (discovery happens lazily). RedirectURL is not validated
// here because it is normally derived per request.
func (pc ProviderConfig) Validate() error {
	if !providerKeyRe.MatchString(pc.Key) {
		return fmt.Errorf("sso: provider key %q must match [a-z0-9][a-z0-9_-]* (no ':' or spaces)", pc.Key)
	}
	if strings.TrimSpace(pc.Name) == "" {
		return fmt.Errorf("sso: provider %q needs a display name", pc.Key)
	}
	if strings.TrimSpace(pc.Issuer) == "" {
		return fmt.Errorf("sso: provider %q needs an issuer", pc.Key)
	}
	if strings.TrimSpace(pc.ClientID) == "" {
		return fmt.Errorf("sso: provider %q needs a client_id", pc.Key)
	}
	if pc.Preset != "" {
		if _, ok := LookupPreset(pc.Preset); !ok {
			return fmt.Errorf("sso: provider %q references unknown preset %q", pc.Key, pc.Preset)
		}
	}
	return nil
}

func (pc ProviderConfig) claimNames() ClaimNames {
	return ClaimNames{Email: pc.ClaimEmail, Username: pc.ClaimUsername, Name: pc.ClaimName}
}

// toOIDCConfig projects the persisted provider onto the engine's OIDCConfig,
// binding the caller-supplied redirectURL (request-derived unless pinned).
func (pc ProviderConfig) toOIDCConfig(redirectURL string) OIDCConfig {
	return OIDCConfig{
		Issuer:       strings.TrimSpace(pc.Issuer),
		ClientID:     strings.TrimSpace(pc.ClientID),
		ClientSecret: pc.ClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       pc.Scopes,
		Claims:       pc.claimNames(),
	}
}

// fingerprint is a stable digest of every field that affects the constructed
// OIDCProvider, so the provider cache can detect a config edit (or a changed
// request-derived redirect URL) and rebuild instead of serving a stale instance.
func (pc ProviderConfig) fingerprint(redirectURL string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00",
		pc.Issuer, pc.ClientID, pc.ClientSecret, redirectURL,
		strings.Join(pc.Scopes, ","), pc.ClaimEmail, pc.ClaimUsername, pc.ClaimName)
	return hex.EncodeToString(h.Sum(nil))
}

// httpClientForTests, when set, is injected into every constructed OIDCProvider
// so tests can point discovery/token/JWKS at a loopback mock IdP without a proxy
// in the way. Production leaves it nil (ambient default transport). It is a test
// seam only; the value is set once from a test's setup and never concurrently
// with the login paths under test.
var httpClientForTests *http.Client

// SetHTTPClient installs client as the transport every future OIDCProvider is
// built with (test seam; see httpClientForTests). Passing nil restores the
// ambient default. It also clears the provider cache so already-built providers
// are rebuilt with the new transport.
func SetHTTPClient(client *http.Client) {
	providerCacheMu.Lock()
	defer providerCacheMu.Unlock()
	httpClientForTests = client
	providerCache = map[string]providerCacheEntry{}
}

type providerCacheEntry struct {
	fingerprint string
	provider    *OIDCProvider
}

var (
	providerCacheMu sync.Mutex
	providerCache   = map[string]providerCacheEntry{}
)

// BuildProvider returns an *OIDCProvider for pc bound to redirectURL, reusing a
// cached instance (and its already-completed discovery) while pc and redirectURL
// are unchanged, and transparently rebuilding when either changes. It is safe
// for concurrent use. Both the login-begin and the callback path go through it,
// so a single flow's authorize and token-exchange steps share one provider (and
// therefore the same discovered endpoints and redirect_uri).
func BuildProvider(pc ProviderConfig, redirectURL string) (*OIDCProvider, error) {
	fp := pc.fingerprint(redirectURL)
	providerCacheMu.Lock()
	defer providerCacheMu.Unlock()
	if e, ok := providerCache[pc.Key]; ok && e.fingerprint == fp {
		return e.provider, nil
	}
	var opts []Option
	if httpClientForTests != nil {
		opts = append(opts, WithHTTPClient(httpClientForTests))
	}
	prov, err := NewOIDCProvider(pc.toOIDCConfig(redirectURL), opts...)
	if err != nil {
		return nil, err
	}
	providerCache[pc.Key] = providerCacheEntry{fingerprint: fp, provider: prov}
	return prov, nil
}

// InvalidateProvider drops any cached provider for key, forcing the next
// BuildProvider to reconstruct it (and re-run discovery). Called when a provider
// is edited or removed.
func InvalidateProvider(key string) {
	providerCacheMu.Lock()
	defer providerCacheMu.Unlock()
	delete(providerCache, key)
}
