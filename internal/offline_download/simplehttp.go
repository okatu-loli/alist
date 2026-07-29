package offline_download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	stdpath "path"
	"strings"
	"syscall"
	"time"

	"github.com/alist-org/alist/v3/pkg/utils"
)

// errSSRFRejected is returned when a target resolves to a disallowed (internal)
// address. It is produced from the dialer Control hook so it surfaces on the
// very first hop that touches an internal IP, including a redirect target.
var errSSRFRejected = errors.New("offline_download: refusing to connect to an internal/disallowed address (SSRF protection)")

// allowLoopbackForTest, when true, exempts loopback addresses from the SSRF
// denylist. It exists ONLY so the happy-path/redirect tests can reach an
// httptest server (which always listens on 127.0.0.1). It MUST remain false in
// production — it is never flipped outside *_test.go. Loopback is otherwise a
// prime SSRF target (local admin APIs, sidecars), so this is intentionally the
// single narrow escape hatch and nothing else.
var allowLoopbackForTest bool

const (
	fetchDialTimeout   = 30 * time.Second
	fetchTotalTimeout  = 0 // no overall client deadline: downloads can be large/slow; per-dial timeout still applies.
	fetchMaxRedirects  = 10
	defaultFetchedName = "download"
)

// cgnat is the RFC 6598 shared address space (100.64.0.0/10). net.IP has no
// stdlib predicate for it, so we carry the parsed net explicitly.
var cgnat = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// disallowed reports whether ip is one the fetcher must never connect to. It
// mirrors hostcap.isInternalIP (loopback/private/link-local/unspecified) and
// additionally denies CGNAT shared space and any multicast address. Applied to
// the RESOLVED IP at dial time, so it defeats DNS rebinding and public->internal
// redirects. IPv4 and IPv6 are both covered by the net.IP predicates.
func disallowed(ip net.IP) bool {
	if allowLoopbackForTest && ip.IsLoopback() {
		return false // TEST ONLY — see allowLoopbackForTest doc.
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		cgnat.Contains(ip)
}

// ssrfControl is a net.Dialer.Control hook. It runs after DNS resolution and
// immediately before connect, so address is already "resolvedIP:port". Because
// the http client makes a fresh connection for each redirect hop, this hook
// re-validates every hop — the anti-rebinding / anti-redirect-to-internal
// checkpoint. Removing it (or validating only the initial URL) reopens the
// public-URL-302-to-internal bypass.
func ssrfControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || disallowed(ip) {
		return errSSRFRejected
	}
	return nil
}

// Fetch performs an SSRF-safe, streaming HTTP(S) GET of rawURL. It returns the
// live response body (NOT read into memory), the reported size (Content-Length,
// or -1 when unknown), and a derived bare filename. On any error the response
// body, if opened, is closed before returning.
func Fetch(ctx context.Context, rawURL string) (rc io.ReadCloser, size int64, name string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, "", fmt.Errorf("offline_download: invalid url: %w", err)
	}
	// scheme whitelist: only http/https. file/ftp/gopher/data/... are refused.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, 0, "", fmt.Errorf("offline_download: unsupported scheme %q (only http/https)", u.Scheme)
	}

	dialer := &net.Dialer{
		Timeout: fetchDialTimeout,
		Control: ssrfControl,
	}
	client := &http.Client{
		Timeout: fetchTotalTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= fetchMaxRedirects {
				return fmt.Errorf("offline_download: too many redirects (>%d)", fetchMaxRedirects)
			}
			// Defense in depth: reject non-http(s) redirect targets up front.
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("offline_download: redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
			// Note: the actual IP-level SSRF check happens in ssrfControl at
			// dial time for the new connection, which is what makes per-hop
			// re-validation (and rebinding defense) sound.
		},
		Transport: &http.Transport{
			// DialContext via the SSRF-controlled dialer. Defense in depth: the
			// dial itself is gated even if CheckRedirect logic changes.
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   fetchDialTimeout,
			ResponseHeaderTimeout: fetchDialTimeout,
			ForceAttemptHTTP2:     true,
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, 0, "", fmt.Errorf("offline_download: unexpected status %s", resp.Status)
	}

	name = deriveFileName(resp, u)
	return resp.Body, resp.ContentLength, name, nil
}

// fileNameFromURL derives a best-effort bare filename from the URL alone (its
// path's last segment, else the "download" fallback), without any network
// access. DownloadTask.Run uses it to supply a target name to op.PutURL when
// the caller left FileName empty -- PutURL needs a destination name up front and
// can't wait for a response. The authoritative name (which may come from a
// Content-Disposition header) is still derived by Fetch/deriveFileName on the
// SimpleHttp fallback path.
func fileNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return defaultFetchedName
	}
	if base := stdpath.Base(u.Path); base != "" && base != "/" && base != "." {
		if s := sanitizeFileName(base); s != "" {
			return s
		}
	}
	return defaultFetchedName
}

// deriveFileName picks a filename from, in order: the Content-Disposition
// filename, the URL path's last segment, then the "download" fallback. The
// result is always run through sanitizeFileName so it is a bare filename.
func deriveFileName(resp *http.Response, u *url.URL) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := params["filename"]; fn != "" {
				if s := sanitizeFileName(fn); s != "" {
					return s
				}
			}
		}
	}
	if base := stdpath.Base(u.Path); base != "" && base != "/" && base != "." {
		if s := sanitizeFileName(base); s != "" {
			return s
		}
	}
	return defaultFetchedName
}

// sanitizeFileName reduces an arbitrary string to a bare filename: no directory
// separators, no "..", no path escape. It normalizes both slash flavors, then
// takes the cleaned base, guaranteeing the result cannot write outside the
// target directory.
func sanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	// Cleaned, rooted path -> its final element is a plain name (".." collapses).
	base := stdpath.Base(utils.FixAndCleanPath(name))
	if base == "/" || base == "." || base == ".." {
		return ""
	}
	return base
}
