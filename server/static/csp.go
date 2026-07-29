package static

import (
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"regexp"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// Content-Security-Policy for the app document (theming spec §5.5.4). It is the
// defense-in-depth backstop behind the theme CSS sanitizer: even if a remote
// url()/@import somehow slipped past the sanitizer, font-src/connect-src 'self'
// block the fetch, so exfiltration would need BOTH a sanitizer bug AND a CSP
// hole.
//
// style-src is 'self' 'unsafe-inline'. It is NOT nonce/hash gated — the SCRIPT
// nonce is the load-bearing relaxation (see below); style-src stays permissive so
// bundled UI libraries can inject their own <style> elements and set inline style
// attributes at runtime, which a nonce-only policy would block (and CSP3 IGNORES
// a nonce whenever 'unsafe-inline' is present, so the two cannot coexist). The
// real gate for theme CSS is the SANITIZER (it rejects remote url()/scope-escape
// at parse time, two red-team rounds deep), not CSP; CSP style-src is only a
// backstop, and XSS-injected styles would need script execution first, which
// script-src blocks (only 'self' + host-nonced scripts, never arbitrary inline).
//
// img-src/media-src/connect-src additionally allow https: (and blob:), because
// AList is a file browser: previews load the user's own storage content from
// arbitrary external CDNs (a 123pan/S3/etc raw_url is an off-origin https link),
// and viewers build blob: URLs from fetched bytes. Restricting these to 'self'
// silently broke every external-storage preview. This loosens the CSP BACKSTOP
// for theme CSS (a remote url() that slipped the sanitizer could now fetch an
// image) but NOT the primary gate: the theme sanitizer blocks remote url() at
// parse time (spec §5.5.2), which is where theme exfiltration is actually
// stopped. The anti-XSS / anti-clickjacking directives stay strict: script-src
// is 'self' + a HOST-injected per-response nonce (no 'unsafe-inline'), so only a
// <script nonce="…"> the host emits — reading the live, unguessable per-response
// nonce — runs; an injection-XSS <script> cannot know the nonce and stays
// blocked (spec §D5: this is what lets a TRUSTED theme's custom JS run while
// arbitrary inline stays denied). object-src/base-uri/form-action locked down;
// frame-ancestors is 'none' for EVERY response EXCEPT the one designated
// preview route (see isPreviewFramingRequest below) — it is NOT a blanket
// site-wide relaxation (that was a HIGH-severity clickjacking gap: a plugin
// holding a public_routes grant can serve arbitrary HTML from this SAME
// origin — ExtPublicPrefixDispatch/public_prefix — which a blanket
// frame-ancestors 'self' would then let frame ANY other same-origin page,
// including /@manage/*, since the query param that used to gate the
// preview-only relaxation was unauthenticated and checked nowhere per-route).
const (
	// cspNoncePlaceholder is replaced per response with the live nonce; the
	// directive list is otherwise constant.
	cspNoncePlaceholder = "__CSP_NONCE__"
	// cspTemplateBeforeScriptSrc/cspTemplateAfterScriptSrc/cspTemplateSuffix
	// sandwich the two directives whose value depends on the request/settings
	// (script-src's allowlist tail, and frame-ancestors) — see cspHeader.
	cspTemplateBeforeScriptSrc = "default-src 'self'; "
	// script-src's fixed head: the HOST-injected per-response nonce so a
	// <script nonce="__CSP_NONCE__"> the host adds at runtime — the mechanism
	// that runs a TRUSTED theme's custom JS (spec §D5) and, as of the
	// customize_head/body policy reversal, an inline customize_head/body
	// <script> too (see sanitize.go) — is admitted. cspHeader appends any
	// script_src_allowlist hosts (conf.ScriptSrcAllowlist) after this so an
	// EXTERNAL <script src="..."> can also be admitted, by origin rather than
	// nonce (a nonce would admit ANY origin, defeating the allowlist's whole
	// point — see sanitize.go's doc comment). Still NO 'unsafe-inline':
	// because CSP3 nonces are matched against the exact per-response value
	// (minted by newNonce, 128 unguessable bits, never reused), an
	// injection-XSS <script> cannot supply it and stays blocked regardless of
	// how many hosts the allowlist admits.
	cspScriptSrcHead          = "script-src 'self' 'wasm-unsafe-eval' 'nonce-" + cspNoncePlaceholder + "'"
	cspTemplateAfterScriptSrc = "; " +
		// style-src allows inline styles: rich UI libraries the app bundles
		// (artplayer, and others) inject their own <style> elements and set style
		// attributes at runtime, which a nonce-only policy blocks — and CSP3
		// IGNORES 'unsafe-inline' whenever a nonce is present, so the two cannot
		// coexist. Theme CSS injection is still gated at its PRIMARY boundary (the
		// sanitizer rejects remote url()/scope-escape at parse time, two red-team
		// rounds deep); CSP style-src was only the backstop. XSS-injected styles
		// need script execution first, which script-src blocks: it admits only
		// 'self' + host-nonced/allowlisted scripts, never arbitrary inline (no
		// 'unsafe-inline').
		"style-src 'self' 'unsafe-inline'; " +
		"font-src 'self'; " +
		"img-src 'self' data: blob: https:; " +
		"media-src 'self' blob: https:; " +
		"connect-src 'self' https:; " +
		// hls.js / mpegts.js run their transmuxers in a Web Worker spun up from a
		// blob: URL; without a worker-src it falls back to default-src 'self' and
		// the blob worker is blocked, so HLS/FLV/TS video never loads.
		"worker-src 'self' blob:; " +
		"object-src 'none'; " +
		"base-uri 'self'; "
	// frame-ancestors is injected between cspTemplateAfterScriptSrc and
	// cspTemplateSuffix by cspHeader (see below) — 'self' ONLY for the one
	// designated preview route, 'none' for every other response, including
	// /@manage/* even when it carries the same "?__preview=1" query string
	// (isPreviewFramingRequest pins the PATH, not just the query — see its
	// doc comment for the attack this closes).
	cspTemplateSuffix = "form-action 'self'"
)

// previewQueryKey/previewQueryValue mirror the frontend's preview contract
// (alist-v4-web src/theme/preview.ts PREVIEW_QUERY_KEY/PREVIEW_QUERY_VALUE) —
// kept in sync manually since the two are different repos/languages.
const (
	previewQueryKey   = "__preview"
	previewQueryValue = "1"
)

// newNonce mints a fresh, unguessable per-response nonce (128 bits, base64).
func newNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is catastrophic; a fixed value would defeat the
		// nonce, so return empty and let the caller serve without a working nonce.
		// Styles still apply (style-src is 'unsafe-inline'); only a TRUSTED theme's
		// custom <script>, which needs the nonce, would fail to run in that case.
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(b)
}

// cspHeader returns the CSP header value for a given nonce. allowSelfFraming
// selects the frame-ancestors value: 'self' ONLY for the designated preview
// route (see isPreviewFramingRequest — the only caller that should ever pass
// true), 'none' for everything else. Getting this wrong in the permissive
// direction is a clickjacking hole (see the const-block doc comment above),
// so every call site MUST derive it from isPreviewFramingRequest, never from
// the query string alone.
//
// scriptSrcAllowlist (conf.ScriptSrcAllowlist, parsed by
// parseScriptSrcAllowlist) is appended to the script-src directive, admitting
// an EXTERNAL <script src="..."> pasted into customize_head/customize_body
// (sanitize.go) from one of these origins — empty by default, so no
// third-party script domain is trusted until an admin explicitly adds one.
func cspHeader(nonce string, allowSelfFraming bool, scriptSrcAllowlist []string) string {
	frameAncestors := "frame-ancestors 'none'; "
	if allowSelfFraming {
		frameAncestors = "frame-ancestors 'self'; "
	}
	scriptSrc := cspScriptSrcHead
	for _, host := range scriptSrcAllowlist {
		scriptSrc += " " + host
	}
	tpl := cspTemplateBeforeScriptSrc + scriptSrc + cspTemplateAfterScriptSrc + frameAncestors + cspTemplateSuffix
	// The template carries the placeholder exactly once (script-src); ReplaceAll
	// stamps the live per-response nonce in and is robust if more nonce'd
	// directives are ever added.
	return strings.ReplaceAll(tpl, cspNoncePlaceholder, nonce)
}

// scriptSrcHostPattern matches a well-formed CSP host-source expression: an
// optional scheme ("https://"), a host (a bare wildcard "*", an optional
// leading "*." wildcard label, or ordinary dot-separated DNS labels), an
// optional ":port" (a number or "*"), and an optional "/path" tail. This is a
// real (if simplified) grammar check — not merely a character blocklist — so
// a red-team-style entry has to actually LOOK LIKE a host-source to survive,
// not just avoid a specific denylisted character set.
var scriptSrcHostPattern = regexp.MustCompile(
	`^(?:[a-zA-Z][a-zA-Z0-9+.-]*://)?` + // optional scheme
		`(?:\*|(?:\*\.)?[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*)` + // host
		`(?::(?:[0-9]+|\*))?` + // optional port
		`(?:/[a-zA-Z0-9._~%/-]*)?$`, // optional path
)

// parseScriptSrcAllowlist turns the raw conf.ScriptSrcAllowlist setting value
// (comma/newline separated) into the list of host-source tokens cspHeader
// folds into script-src. Defensive against CSP-header injection via the
// setting's own value (an admin-only PRIVATE setting, but still: this feeds
// directly into a response header): each entry must match
// scriptSrcHostPattern — a real host-source SHAPE check, not just "doesn't
// contain a denylisted character" — so anything containing whitespace, a
// control character (CR/LF included), a quote, a semicolon, or that simply
// isn't shaped like a host/origin at all is dropped rather than passed
// through. Splitting on comma/newline/CR happens first, so even a raw CRLF
// injection attempt is torn into separate tokens before this check ever runs
// (and whatever trailing "directive-looking" fragment results still has to
// independently pass the same host-shape check to survive).
func parseScriptSrcAllowlist(raw string) []string {
	var hosts []string
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		host := strings.TrimSpace(line)
		if host == "" {
			continue
		}
		if !scriptSrcHostPattern.MatchString(host) {
			utils.Log.Warnf("script_src_allowlist: dropping entry that isn't a well-formed host-source: %q", host)
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts
}

// isPreviewFramingRequest reports whether THIS response should carry the
// relaxed frame-ancestors 'self' (theming spec J3b: the theme editor's live
// preview loads the app in a same-origin <iframe src="/?__preview=1">).
//
// SECURITY (HIGH, fixed): this used to be a blanket site-wide relaxation —
// EVERY response got frame-ancestors 'self', including /@manage/*. Combined
// with (a) a plugin holding a public_routes grant being able to serve
// arbitrary HTML from this SAME origin (ExtPublicPrefixDispatch/
// public_prefix, so frame-ancestors 'self' already permits it to frame other
// same-origin pages) and (b) the frontend's isPreviewMode only checking an
// unauthenticated query string, a same-origin malicious/compromised plugin
// page could frame "/@manage/users?__preview=1" and get a full clickjacking
// canvas (real sidebar + delete/grant buttons, chrome hidden) — a path that
// was categorically impossible when frame-ancestors was 'none' everywhere.
//
// The fix pins BOTH the query string AND the exact path: only the bare root
// path (after trimming the deployment's base path) carrying exactly
// "__preview=1" qualifies. "/@manage/x?__preview=1" (or any other path) never
// does, regardless of query string — so even a same-origin framer gets 'none'
// for every route except this one, and the one route that IS relaxed
// carries no privileged admin UI of its own (it's the anonymous-by-default
// file browser root).
func isPreviewFramingRequest(requestPath, rawQuery string) bool {
	if trimBasePathForCSP(requestPath) != "/" {
		return false
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	return values.Get(previewQueryKey) == previewQueryValue
}

// trimBasePathForCSP strips the deployment's configured base path
// (conf.URL.Path, the prefix server/router.go mounts every route under via
// e.Group(conf.URL.Path)) from a request path, exactly like
// server/handles/ext_route.go's trimBasePath — duplicated here (rather than
// imported) because server/handles already imports server/static
// (handles/setting.go), so the reverse import would cycle. Keep the two in
// sync if the base-path trimming rule ever changes.
func trimBasePathForCSP(path string) string {
	if conf.URL == nil {
		// Not yet initialized — treat as the default root base path, a no-op
		// (mirrors trimBasePath's same guard).
		return path
	}
	base := conf.URL.Path
	if base == "" || base == "/" {
		return path
	}
	base = strings.TrimSuffix(base, "/")
	if path == base {
		return "/"
	}
	if rest, ok := strings.CutPrefix(path, base+"/"); ok {
		return "/" + rest
	}
	return path
}

// injectNonceMeta places <meta name="csp-nonce" content="…"> into the served
// HTML so the frontend (src/theme/nonce.ts) can read the live nonce and stamp it
// onto the <style> elements it injects at runtime. Inserted just before </head>;
// if no </head> is found (unexpected) the HTML is returned unchanged and the
// frontend simply degrades to "no nonce" (dev behavior).
func injectNonceMeta(html, nonce string) string {
	meta := `<meta name="csp-nonce" content="` + nonce + `">`
	if i := strings.Index(html, "</head>"); i >= 0 {
		return html[:i] + meta + html[i:]
	}
	return html
}
