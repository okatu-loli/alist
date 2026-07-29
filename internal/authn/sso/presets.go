package sso

import "strings"

// Preset is a convenience template for a well-known OIDC provider, so an operator
// mostly only supplies a domain and client credentials. It never carries secrets:
// it pins the issuer shape, sensible default scopes, the claim mapping and a docs
// link. "generic" is the escape hatch for any spec-compliant OIDC IdP (Keycloak,
// Auth0, Google, ...), where the operator types the full issuer directly.
type Preset struct {
	Key            string     // stable id: "casdoor" | "clerk" | "authing" | "generic"
	Name           string     // human label for the admin UI
	IssuerTemplate string     // "{domain}" placeholder; empty means the issuer is entered verbatim
	DefaultScopes  []string   // scopes to prefill; openid is always effectively required
	Claims         ClaimNames // default claim-name mapping
	DocURL         string     // provider setup documentation
}

// BuildIssuer fills the preset's issuer template with the operator-supplied
// domain. For "generic" (empty template) it returns the domain as-is, treating it
// as the full issuer URL. A leading scheme in domain is respected; a bare host is
// left untouched (the caller/UI is responsible for prompting a full origin).
func (p Preset) BuildIssuer(domain string) string {
	domain = strings.TrimSpace(domain)
	domain = strings.TrimRight(domain, "/")
	if p.IssuerTemplate == "" {
		return domain
	}
	return strings.ReplaceAll(p.IssuerTemplate, "{domain}", domain)
}

// defaultClaims is the standard OIDC claim mapping shared by the presets; it
// matches ClaimNames.withDefaults but is stated explicitly so a preset can
// diverge later without touching the engine defaults.
var defaultClaims = ClaimNames{Email: "email", Username: "preferred_username", Name: "name"}

var presets = []Preset{
	{
		Key:            "casdoor",
		Name:           "Casdoor",
		IssuerTemplate: "https://{domain}",
		DefaultScopes:  []string{"openid", "profile", "email"},
		Claims:         defaultClaims,
		DocURL:         "https://casdoor.org/docs/how-to-connect/oidc",
	},
	{
		Key:            "clerk",
		Name:           "Clerk",
		IssuerTemplate: "https://{domain}",
		DefaultScopes:  []string{"openid", "profile", "email"},
		Claims:         defaultClaims,
		DocURL:         "https://clerk.com/docs/advanced-usage/clerk-idp",
	},
	{
		Key:            "authing",
		Name:           "Authing",
		IssuerTemplate: "https://{domain}/oidc",
		DefaultScopes:  []string{"openid", "profile", "email"},
		Claims:         defaultClaims,
		DocURL:         "https://docs.authing.cn/v2/concepts/oidc.html",
	},
	{
		Key:            "generic",
		Name:           "Generic OIDC",
		IssuerTemplate: "", // operator enters the full issuer URL
		DefaultScopes:  []string{"openid", "profile", "email"},
		Claims:         defaultClaims,
		DocURL:         "https://openid.net/specs/openid-connect-discovery-1_0.html",
	},
}

// Presets returns the built-in provider presets in display order.
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}

// LookupPreset returns the preset with the given key. The bool is false for an
// unknown key.
func LookupPreset(key string) (Preset, bool) {
	for _, p := range presets {
		if p.Key == key {
			return p, true
		}
	}
	return Preset{}, false
}
