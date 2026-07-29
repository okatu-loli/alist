package static

import (
	"net/url"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
)

func TestNewNonceIsFreshAndSized(t *testing.T) {
	a, b := newNonce(), newNonce()
	if a == "" || b == "" {
		t.Fatal("nonce must not be empty")
	}
	if a == b {
		t.Fatal("nonces must differ per call")
	}
	// 16 random bytes → 22 chars of raw (unpadded) base64.
	if len(a) != 22 {
		t.Fatalf("unexpected nonce length %d (%q)", len(a), a)
	}
}

func TestCSPHeaderCarriesNonceAndLeakClosingDirectives(t *testing.T) {
	h := cspHeader("N0nc3", false, nil)
	// style-src allows inline styles (rich UI libs inject their own <style>); the
	// theme sanitizer, not CSP, is the primary gate for theme CSS.
	if !strings.Contains(h, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("style-src must allow inline styles for bundled UI libs: %q", h)
	}
	// script-src carries the HOST-injected per-response nonce (so a <script
	// nonce="…"> the host adds to run a TRUSTED theme's JS is admitted) but NO
	// 'unsafe-inline': an injection-XSS <script> cannot know the unguessable
	// per-response nonce, so it stays blocked. The nonce is the ONLY relaxation.
	if !strings.Contains(h, "script-src 'self' 'wasm-unsafe-eval' 'nonce-N0nc3'") {
		t.Fatalf("script-src must carry the per-response nonce: %q", h)
	}
	if strings.Contains(h, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("script-src must never allow arbitrary inline: %q", h)
	}
	// The dead empty-<style> hash was removed (M-3): style-src is plain
	// 'unsafe-inline', with no leftover sha256 hash token.
	if strings.Contains(h, "sha256-") {
		t.Fatalf("CSP must not carry a leftover style hash: %q", h)
	}
	// The anti-XSS / anti-clickjacking directives stay locked to 'self'/'none'.
	for _, want := range []string{
		"default-src 'self'",
		"font-src 'self'",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("CSP missing %q in %q", want, h)
		}
	}
	// frame-ancestors defaults to 'none' (allowSelfFraming=false): every
	// response EXCEPT the one designated preview route must refuse to be
	// framed at all, full stop — see isPreviewFramingRequest's doc comment
	// for the clickjacking gap a blanket 'self' used to open.
	if !strings.Contains(h, "frame-ancestors 'none'") {
		t.Fatalf("frame-ancestors must default to 'none': %q", h)
	}
	if strings.Contains(h, "frame-ancestors 'self'") {
		t.Fatalf("frame-ancestors must not be 'self' when allowSelfFraming is false: %q", h)
	}
	// img/media/connect additionally allow external https (+ blob) so the file
	// browser can preview the user's storage content from external CDNs.
	for _, want := range []string{
		"img-src 'self' data: blob: https:",
		"media-src 'self' blob: https:",
		"connect-src 'self' https:",
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("CSP missing %q in %q", want, h)
		}
	}
}

func TestInjectNonceMetaBeforeHead(t *testing.T) {
	html := "<html><head><title>x</title></head><body></body></html>"
	out := injectNonceMeta(html, "abc")
	want := `<meta name="csp-nonce" content="abc">`
	if !strings.Contains(out, want) {
		t.Fatalf("meta not injected: %q", out)
	}
	if strings.Index(out, want) > strings.Index(out, "</head>") {
		t.Fatalf("meta must come before </head>: %q", out)
	}
}

func TestInjectNonceMetaNoHeadIsUnchanged(t *testing.T) {
	html := "<div>no head here</div>"
	if got := injectNonceMeta(html, "abc"); got != html {
		t.Fatalf("html without </head> must be returned unchanged, got %q", got)
	}
}

func TestCSPHeaderAllowSelfFramingProducesSelf(t *testing.T) {
	h := cspHeader("N0nc3", true, nil)
	if !strings.Contains(h, "frame-ancestors 'self'") {
		t.Fatalf("allowSelfFraming=true must produce frame-ancestors 'self': %q", h)
	}
	if strings.Contains(h, "frame-ancestors 'none'") {
		t.Fatalf("must not carry 'none' when allowSelfFraming is true: %q", h)
	}
	// Every other directive is unaffected by allowSelfFraming.
	if !strings.Contains(h, "script-src 'self' 'wasm-unsafe-eval' 'nonce-N0nc3'") {
		t.Fatalf("script-src must be unaffected: %q", h)
	}
}

// TestCSPHeaderFoldsScriptSrcAllowlistIntoScriptSrc: script_src_allowlist
// hosts (conf.ScriptSrcAllowlist) must be admitted into script-src, so an
// external <script src="..."> pasted into customize_head/customize_body from
// one of these origins actually loads.
func TestCSPHeaderFoldsScriptSrcAllowlistIntoScriptSrc(t *testing.T) {
	h := cspHeader("N0nc3", false, []string{"cdn.example.com", "https://*.example.org"})
	if !strings.Contains(h, "script-src 'self' 'wasm-unsafe-eval' 'nonce-N0nc3' cdn.example.com https://*.example.org;") {
		t.Fatalf("script-src must fold in the allowlist hosts (in order): %q", h)
	}
	// Every other directive is unaffected.
	if !strings.Contains(h, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("style-src must be unaffected by the script-src allowlist: %q", h)
	}
}

func TestCSPHeaderWithEmptyAllowlistMatchesNoAllowlist(t *testing.T) {
	withNil := cspHeader("N0nc3", false, nil)
	withEmpty := cspHeader("N0nc3", false, []string{})
	if withNil != withEmpty {
		t.Fatalf("nil and empty allowlist must produce identical headers: %q vs %q", withNil, withEmpty)
	}
	if !strings.Contains(withNil, "script-src 'self' 'wasm-unsafe-eval' 'nonce-N0nc3';") {
		t.Fatalf("with no allowlist, script-src must carry no extra hosts: %q", withNil)
	}
}

// TestParseScriptSrcAllowlist covers the comma/newline splitting, the
// header-injection defense (whitespace/quote/semicolon-bearing entries are
// dropped, not passed through into a response header), and the host-SHAPE
// validation (scriptSrcHostPattern) — a real grammar check, not merely "free
// of denylisted characters", so garbage that happens to avoid every
// denylisted char (no spaces/quotes/semicolons) but doesn't actually look
// like a host-source is still rejected.
func TestParseScriptSrcAllowlist(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \n  ", nil},
		{"single host", "cdn.example.com", []string{"cdn.example.com"}},
		{"comma separated", "cdn.example.com,analytics.example.org", []string{"cdn.example.com", "analytics.example.org"}},
		{"newline separated with blank lines", "cdn.example.com\n\nanalytics.example.org\n", []string{"cdn.example.com", "analytics.example.org"}},
		{"trims surrounding whitespace per entry", " cdn.example.com , https://*.example.org ", []string{"cdn.example.com", "https://*.example.org"}},
		{"a bare wildcard host is a valid CSP host-source", "*", []string{"*"}},
		{"a host with an explicit port", "cdn.example.com:8443", []string{"cdn.example.com:8443"}},
		{"drops an entry containing a space (header-injection defense)", "cdn.example.com,evil.com fake-directive:1", []string{"cdn.example.com"}},
		{"drops an entry containing a semicolon", "cdn.example.com,evil.com;script-src *", []string{"cdn.example.com"}},
		{"drops an entry containing a quote", `cdn.example.com,evil.com"`, []string{"cdn.example.com"}},
		// A trailing/leading tab is trimmed by TrimSpace like any whitespace —
		// this embeds the control char in the MIDDLE of the token so it's the
		// shape check (not trimming) that must catch it.
		{"drops an entry containing an embedded control character", "cdn.example.com,evil.com\tworse", []string{"cdn.example.com"}},
		// None of these contain a denylisted character (no space/quote/
		// semicolon/control char) — only the host-SHAPE check catches them,
		// proving parseScriptSrcAllowlist is more than a character blocklist.
		{"drops a CSP keyword token (no denylisted chars, but not host-shaped)", "cdn.example.com,'unsafe-inline'", []string{"cdn.example.com"}},
		{"drops a scheme with no real host", "cdn.example.com,data:", []string{"cdn.example.com"}},
		{"drops a javascript: pseudo-URL", "cdn.example.com,javascript:alert(1)", []string{"cdn.example.com"}},
		{"drops a non-host token with disallowed punctuation", "cdn.example.com,not_a-host!!", []string{"cdn.example.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseScriptSrcAllowlist(c.raw)
			if len(got) != len(c.want) {
				t.Fatalf("parseScriptSrcAllowlist(%q) = %v, want %v", c.raw, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("parseScriptSrcAllowlist(%q) = %v, want %v", c.raw, got, c.want)
				}
			}
		})
	}
}

// TestIsPreviewFramingRequest is the regression test for the HIGH-severity
// clickjacking gap: only the bare root path with EXACTLY "__preview=1"
// qualifies for the relaxed frame-ancestors — never /@manage/* (or any other
// route) even carrying the identical query string, and never a near-miss
// query value.
func TestIsPreviewFramingRequest(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		query string
		want  bool
	}{
		{"root with the exact preview query", "/", "__preview=1", true},
		{"root with the preview query plus other params", "/", "foo=bar&__preview=1", true},
		{"root with no query at all", "/", "", false},
		{"root with a near-miss query value", "/", "__preview=0", false},
		{"root with the wrong query key", "/", "preview=1", false},
		// The actual attack this closes: an admin route carrying the exact
		// same query string as the legitimate preview iframe must NOT get
		// the relaxed header — otherwise a same-origin plugin public_routes
		// page could frame it for a clickjacking canvas (real sidebar +
		// delete/grant buttons, chrome hidden by the frontend's naive
		// query-only gate). The path is what must save this, since the
		// query string alone is attacker-controlled and unauthenticated.
		{"admin route with the identical preview query", "/@manage/users", "__preview=1", false},
		{"admin route with no query", "/@manage/users", "", false},
		{"any non-root browse path with the preview query", "/some/folder", "__preview=1", false},
		{"account page with the preview query", "/@account", "__preview=1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isPreviewFramingRequest(c.path, c.query)
			if got != c.want {
				t.Fatalf("isPreviewFramingRequest(%q, %q) = %v, want %v", c.path, c.query, got, c.want)
			}
		})
	}
}

func TestTrimBasePathForCSP(t *testing.T) {
	// conf.URL is nil in this test binary (never bootstrapped) — the no-op
	// guard path.
	if got := trimBasePathForCSP("/@manage/users"); got != "/@manage/users" {
		t.Fatalf("nil conf.URL must be a no-op, got %q", got)
	}
	if got := trimBasePathForCSP("/"); got != "/" {
		t.Fatalf("nil conf.URL must be a no-op, got %q", got)
	}
}

// TestTrimBasePathForCSPWithSubPathDeployment covers a reverse-proxy
// sub-path deployment (conf.URL.Path == "/alist"), mirroring
// server/handles/ext_route_test.go's coverage of the sibling trimBasePath —
// the preview-route gate must keep working (and keep refusing /@manage/*)
// under a non-root base path too.
func TestTrimBasePathForCSPWithSubPathDeployment(t *testing.T) {
	prev := conf.URL
	conf.URL = &url.URL{Path: "/alist"}
	t.Cleanup(func() { conf.URL = prev })

	if got := trimBasePathForCSP("/alist"); got != "/" {
		t.Fatalf("bare base path must trim to root, got %q", got)
	}
	if got := trimBasePathForCSP("/alist/"); got != "/" {
		t.Fatalf("base path + trailing slash must trim to root, got %q", got)
	}
	if got := trimBasePathForCSP("/alist/@manage/users"); got != "/@manage/users" {
		t.Fatalf("base path prefix must be stripped, got %q", got)
	}
	// A path that merely starts with the same characters but diverges at the
	// next segment is NOT a match (mirrors trimBasePath's own contract).
	if got := trimBasePathForCSP("/alistx/@manage"); got != "/alistx/@manage" {
		t.Fatalf("a non-segment-boundary prefix must be left unchanged, got %q", got)
	}

	if !isPreviewFramingRequest("/alist", "__preview=1") {
		t.Fatal("the preview route under a sub-path deployment must still qualify")
	}
	if isPreviewFramingRequest("/alist/@manage/users", "__preview=1") {
		t.Fatal("an admin route under a sub-path deployment must still be refused")
	}
}
