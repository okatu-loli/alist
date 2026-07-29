// Package passkey is the pure WebAuthn engine backing alist's built-in
// passwordless passkey login. It owns credential ceremonies (registration and
// discoverable login), the short-lived challenge store, and Relying Party
// configuration. It performs no HTTP and no persistence: callers feed it the
// response body bytes and persist the resulting model.Passkey themselves.
package passkey

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"
)

// RP is the Relying Party configuration a WebAuthn instance is built from.
//   - ID is the RP ID: the effective domain (host only, no scheme or port), e.g.
//     "example.com". It must be a registrable suffix of every origin.
//   - DisplayName is a human-readable site name shown by some authenticators.
//   - Origins is the set of fully-qualified origins (scheme + host + port)
//     permitted to run ceremonies, e.g. "https://example.com".
type RP struct {
	ID          string
	DisplayName string
	Origins     []string
}

// New validates the Relying Party configuration and builds a *webauthn.WebAuthn.
// It rejects an empty RP ID, an empty origin list, any unparseable origin, and —
// crucially — any origin for which the RP ID is not a registrable suffix. The
// last check is the anti-cross-origin guardrail: a credential scoped to RP ID
// "example.com" must never be usable from "evil.com".
func New(rp RP) (*webauthn.WebAuthn, error) {
	if err := rp.validate(); err != nil {
		return nil, err
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rp.ID,
		RPDisplayName: rp.DisplayName,
		RPOrigins:     rp.Origins,
	})
}

func (rp RP) validate() error {
	if rp.ID == "" {
		return fmt.Errorf("passkey: RP ID is empty")
	}
	if len(rp.Origins) == 0 {
		return fmt.Errorf("passkey: RP origins is empty")
	}
	for _, origin := range rp.Origins {
		host, err := originHost(origin)
		if err != nil {
			return fmt.Errorf("passkey: invalid origin %q: %w", origin, err)
		}
		if !isRegistrableSuffix(rp.ID, host) {
			return fmt.Errorf("passkey: RP ID %q is not a registrable suffix of origin %q (host %q)", rp.ID, origin, host)
		}
	}
	return nil
}

// DeriveRPID extracts the RP ID (host, no scheme or port) from a site URL such
// as "https://cloud.example.com:8443". It is the default used when the operator
// has not configured webauthn_rp_id explicitly.
func DeriveRPID(siteURL string) (string, error) {
	return originHost(siteURL)
}

// originHost parses an origin/URL and returns its lowercase host without port. A
// bare host without scheme ("example.com") is accepted too.
func originHost(origin string) (string, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return "", fmt.Errorf("empty")
	}
	if !strings.Contains(origin, "://") {
		// Bare host (possibly with port): give it a scheme so url.Parse fills Host.
		origin = "https://" + origin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("no host in %q", origin)
	}
	return strings.ToLower(host), nil
}

// isRegistrableSuffix reports whether rpID is a valid RP ID for a ceremony
// running on host: WebAuthn requires the RP ID to equal the origin's effective
// domain or be a registrable (dot-aligned) parent of it. "example.com" is valid
// for host "app.example.com" and "example.com", but not for "notexample.com" or
// "com".
func isRegistrableSuffix(rpID, host string) bool {
	rpID = strings.ToLower(rpID)
	host = strings.ToLower(host)
	if rpID == host {
		return true
	}
	return strings.HasSuffix(host, "."+rpID)
}
