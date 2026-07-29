package thememarket

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// marketplaceRequestTimeout bounds a SINGLE catalog fetch or theme download
// (M1 hardening, opus final-review round). An admin-triggered install must
// not be able to hang the request indefinitely against a slow or hostile
// source — c.Request.Context() alone doesn't bound this (an admin's own HTTP
// client could hold the connection open far longer than is reasonable for a
// same-process outbound fetch).
const marketplaceRequestTimeout = 20 * time.Second

// isBlockedHost reports whether ip must never be reached by a marketplace
// fetch: loopback, RFC1918/ULA private, link-local (this is what covers the
// cloud-metadata address 169.254.169.254 — AWS/GCP/Azure all serve instance
// credentials there), or unspecified. Mirrors internal/extension/hostcap's
// isInternalIP, but with NO opt-in: hostcap lets a plugin reach an internal
// target when its OWN admin-reviewed Grant.Network explicitly names it; a
// theme-marketplace source's CATALOG CONTENT (the url a catalog entry points
// at) is not admin-reviewed line-by-line the way a Grant is — an admin
// importing a source is trusting its curation, not vetting every URL it
// might later serve — so there is no opt-in here at all: every marketplace
// fetch refuses every internal address, unconditionally, in production.
func isBlockedHost(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// allowInternalHostsForTests exists ONLY so this package's (and
// server/handles') own tests — which use httptest.NewServer, i.e. 127.0.0.1 —
// can exercise FetchCatalog/DownloadTheme without standing up a public-
// internet fixture. Production code NEVER calls AllowInternalHostsForTests;
// there is no config flag, admin setting, or env var that reaches this — the
// only writers are _test.go files.
var allowInternalHostsForTests = false

// AllowInternalHostsForTests toggles the marketplace HTTP client's internal-
// host block. TEST-ONLY — see allowInternalHostsForTests's doc comment.
func AllowInternalHostsForTests(allow bool) {
	allowInternalHostsForTests = allow
}

// marketplaceHTTPClient is a DEDICATED, hardened client for every outbound
// marketplace fetch — deliberately NOT the shared internal/net.HttpClient(),
// which has no SSRF guard at all and is tuned for storage-driver traffic an
// admin explicitly configured target-by-target (a very different trust
// model: there, the admin typed in the exact host; here, the admin only
// picked a SOURCE, and the source's catalog can point anywhere). Blocks
// internal targets at the dialer, AFTER DNS resolution (so a hostname that
// resolves to an internal IP is refused exactly like a literal internal IP
// would be — this is what closes DNS rebinding), and re-checks on every
// redirect hop via CheckRedirect.
var marketplaceHTTPClient = buildMarketplaceHTTPClient()

func buildMarketplaceHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: marketplaceRequestTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			hostOnly, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(hostOnly); ip != nil && isBlockedHost(ip) && !allowInternalHostsForTests {
				return fmt.Errorf("thememarket: refusing to reach an internal/private address")
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: marketplaceRequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("thememarket: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("thememarket: refusing a redirect to a non-http(s) url")
			}
			return nil
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

// NetworkError wraps a network/transport-level failure (DNS, dial, TLS,
// timeout, redirect refusal, non-2xx status) from FetchCatalog/DownloadTheme.
// Handlers should log the full underlying error server-side and show only a
// generic message to the admin UI (M6 hardening) — the underlying text can
// embed internal details (dial target IPs/ports, DNS resolver errors, proxy
// hints) that shouldn't reach the client, unlike a VALIDATION error (bad
// JSON, sha256 mismatch, missing signature), which is safe and actionable to
// show verbatim.
type NetworkError struct {
	Op  string // "fetch catalog" | "download theme"
	Err error
}

func (e *NetworkError) Error() string { return fmt.Sprintf("thememarket: %s: %v", e.Op, e.Err) }
func (e *NetworkError) Unwrap() error { return e.Err }
