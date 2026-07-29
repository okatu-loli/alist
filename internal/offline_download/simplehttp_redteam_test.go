package offline_download

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchRejectsSSRF_Redteam extends the Wave 1 SSRF table with the trickier
// vectors a red team reaches for once the obvious ones are covered:
//   - IPv6 internal literals: ULA (fc00::/7), link-local (fe80::/10), loopback.
//   - IPv4-mapped IPv6 (::ffff:10.0.0.1) — must NOT bypass the v4 predicates.
//   - scheme case/obfuscation: HTTP:// and hTTp:// must still be treated as http
//     and gated (url.Parse lowercases the scheme, so the whitelist and the dial
//     both apply).
//   - userinfo host confusion: http://evil.com@10.0.0.1/ dials the REAL host
//     (10.0.0.1), not the userinfo, so it must be rejected.
//
// Every entry must fail. The load-bearing part is that the internal-IP ones fail
// with errSSRFRejected specifically (proving the dial-time Control hook fired on
// the resolved IP), not merely "some error". Deleting disallowed()'s coverage of
// any family, or the ssrfControl hook, turns this red.
func TestFetchRejectsSSRF_Redteam(t *testing.T) {
	// These resolve to an internal IP and must be refused by the Control hook.
	internal := []string{
		"http://[fc00::1]/x",          // IPv6 ULA
		"http://[fe80::1]/x",          // IPv6 link-local unicast
		"http://[::1]/x",              // IPv6 loopback
		"http://[::ffff:10.0.0.1]/x",  // IPv4-mapped IPv6 -> 10.0.0.1
		"http://[::ffff:127.0.0.1]/x", // IPv4-mapped loopback
		"HTTP://10.0.0.1/x",           // uppercase scheme
		"hTTp://10.0.0.1/x",           // mixed-case scheme
		"http://evil.com@10.0.0.1/x",  // userinfo confusion; real host is 10.0.0.1
		"http://10.0.0.1:5245/admin",  // internal service port
		"http://[fc00::1]:8080/x",     // ULA with explicit port
	}
	for _, u := range internal {
		rc, _, _, err := Fetch(context.Background(), u)
		if rc != nil {
			rc.Close()
		}
		if err == nil {
			t.Errorf("Fetch(%q) must be rejected", u)
			continue
		}
		if !errors.Is(err, errSSRFRejected) {
			t.Errorf("Fetch(%q): expected errSSRFRejected (dial-time IP check), got %v", u, err)
		}
	}

	// Non-http(s) schemes are refused up front (never dialed), so they fail but
	// not necessarily with errSSRFRejected.
	badScheme := []string{
		"file:///etc/passwd",
		"gopher://10.0.0.1/x",
		"ftp://10.0.0.1/x",
		"data:text/plain,x",
		"HTTPS+unix://x/y",
	}
	for _, u := range badScheme {
		rc, _, _, err := Fetch(context.Background(), u)
		if rc != nil {
			rc.Close()
		}
		if err == nil {
			t.Errorf("Fetch(%q) must be rejected (non-http scheme)", u)
		}
	}
}

// TestFetchRejectsMultiHopRedirectToInternal proves the per-hop re-validation is
// sound across a CHAIN of redirects, not just a single hop: front -> mid ->
// 10.0.0.1. front and mid are loopback (test-allowed) public-equivalents; the
// final hop targets an internal address and must be refused with errSSRFRejected
// at dial time. This is the "public URL 302s through an innocent hop into the
// internal network" attack. Removing the dial-time Control hook (or checking
// only the initial URL) makes the final internal dial succeed the check and the
// specific-error assertion fails.
func TestFetchRejectsMultiHopRedirectToInternal(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true // reach the loopback httptest front/mid servers
	t.Cleanup(func() { allowLoopbackForTest = prev })

	// mid redirects to an internal address.
	mid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1:1/final", http.StatusFound)
	}))
	defer mid.Close()

	// front redirects to mid.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mid.URL, http.StatusFound)
	}))
	defer front.Close()

	rc, _, _, err := Fetch(context.Background(), front.URL)
	if rc != nil {
		rc.Close()
	}
	if err == nil {
		t.Fatal("Fetch must reject a multi-hop redirect chain that ends at an internal address")
	}
	if !errors.Is(err, errSSRFRejected) {
		t.Errorf("expected errSSRFRejected on the final internal hop, got: %v", err)
	}
}

// TestFetchSanitizesFilename_Redteam pins that hostile filename sources — every
// Content-Disposition variant plus the RFC 5987 extended form — are reduced to a
// bare, in-directory filename. The name Fetch returns can never contain a path
// separator or a ".." escape, so op.Put can only ever write inside the target
// directory. Deleting sanitizeFileName's FixAndCleanPath/Base collapse turns
// this red.
func TestFetchSanitizesFilename_Redteam(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true
	t.Cleanup(func() { allowLoopbackForTest = prev })

	cases := []struct {
		cd   string // Content-Disposition header
		want string
	}{
		{`attachment; filename="../../../../etc/passwd"`, "passwd"},
		{`attachment; filename="/etc/shadow"`, "shadow"},
		{`attachment; filename="a/b/c"`, "c"},
		{`attachment; filename="....//....//evil"`, "evil"},
		// RFC 5987 extended form, percent-decoded to ../evil by mime.ParseMediaType.
		{`attachment; filename*=UTF-8''%2e%2e%2fevil`, "evil"},
		{`attachment; filename*=UTF-8''%2e%2e%2f%2e%2e%2fpasswd`, "passwd"},
	}
	for _, tc := range cases {
		cd := tc.cd
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Disposition", cd)
			io.WriteString(w, "x")
		}))
		rc, _, name, err := Fetch(context.Background(), srv.URL)
		if err != nil {
			srv.Close()
			t.Fatalf("Fetch(%q) failed: %v", cd, err)
		}
		rc.Close()
		srv.Close()
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			t.Errorf("CD %q -> name %q must be a bare filename (no separators/traversal)", cd, name)
		}
		if name != tc.want {
			t.Errorf("CD %q -> name = %q, want %q", cd, name, tc.want)
		}
	}
}

// TestFetchSanitizesURLPathName pins that a traversal-laden URL path (no
// Content-Disposition) is likewise reduced to a bare filename.
func TestFetchSanitizesURLPathName(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true
	t.Cleanup(func() { allowLoopbackForTest = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "x") // no Content-Disposition -> name derives from URL path
	}))
	defer srv.Close()

	// A path with traversal segments; the request still lands on the server (Go
	// cleans the request path), and the derived name must be a bare filename.
	rc, _, name, err := Fetch(context.Background(), srv.URL+"/..%2f..%2fpasswd")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	defer rc.Close()
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		t.Errorf("URL-derived name %q must be a bare filename", name)
	}
}
