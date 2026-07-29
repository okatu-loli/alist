package static

import (
	"strings"
	"testing"
)

// Tests for the customize_head/customize_body sanitizer (theming spec
// decision J0-1, POLICY REVERSED 2026-07 — see sanitizeCustomizeHTML's doc
// comment). Script content now genuinely executes: an inline <script> is
// auto-stamped with the CSP nonce placeholder (cspNoncePlaceholder, csp.go),
// an external <script src="..."> is left as-is (its execution is gated by the
// script_src_allowlist setting feeding into CSP script-src, not by this
// sanitizer). What this sanitizer still strips unconditionally: on*= inline
// event handlers, javascript: URLs, <noscript>, SVG SMIL animation elements,
// and any admin-supplied `nonce` attribute (never trusted — only the host's
// own placeholder, substituted with the live per-response nonce at serve
// time, is ever admitted by the CSP).

func TestSanitizeCustomizeHTMLKeepsInlineScriptAndStampsNonce(t *testing.T) {
	in := `<style>body{color:red}</style><script>alert(1)</script>`
	out := sanitizeCustomizeHTML(in)
	if !strings.Contains(out, "<script") {
		t.Fatalf("inline script tag must be preserved, got %q", out)
	}
	if !strings.Contains(out, "alert(1)") {
		t.Fatalf("script content must be preserved, got %q", out)
	}
	if !strings.Contains(out, `nonce="`+cspNoncePlaceholder+`"`) {
		t.Fatalf("inline script must be stamped with the CSP nonce placeholder, got %q", out)
	}
	if !strings.Contains(out, "<style>") || !strings.Contains(out, "color:red") {
		t.Fatalf("style content must be preserved, got %q", out)
	}
}

func TestSanitizeCustomizeHTMLLeavesExternalScriptSrcAloneWithNoNonce(t *testing.T) {
	in := `<script src="https://cdn.example.com/widget.js"></script>`
	out := sanitizeCustomizeHTML(in)
	if !strings.Contains(out, `src="https://cdn.example.com/widget.js"`) {
		t.Fatalf("external script src must be preserved, got %q", out)
	}
	if strings.Contains(out, "nonce=") {
		t.Fatalf("an external script must NOT get a nonce (would bypass the script_src_allowlist origin scoping), got %q", out)
	}
}

func TestSanitizeCustomizeHTMLStripsAdminSuppliedNonceOnScript(t *testing.T) {
	// An admin can't spoof/guess the live per-response nonce ahead of time —
	// any nonce they paste in themselves must be dropped, not preserved
	// alongside (or instead of) the host-stamped one.
	in := `<script nonce="totally-not-the-real-nonce">alert(1)</script>`
	out := sanitizeCustomizeHTML(in)
	if strings.Contains(out, "totally-not-the-real-nonce") {
		t.Fatalf("admin-supplied nonce must be stripped, got %q", out)
	}
	if !strings.Contains(out, `nonce="`+cspNoncePlaceholder+`"`) {
		t.Fatalf("the host nonce placeholder must still be stamped, got %q", out)
	}
}

func TestSanitizeCustomizeHTMLStripsAdminSuppliedNonceOnExternalScript(t *testing.T) {
	in := `<script src="https://cdn.example.com/x.js" nonce="spoofed"></script>`
	out := sanitizeCustomizeHTML(in)
	if strings.Contains(out, "spoofed") {
		t.Fatalf("admin-supplied nonce on an external script must be stripped, got %q", out)
	}
	if strings.Contains(out, "nonce=") {
		t.Fatalf("an external script must still get no nonce at all, got %q", out)
	}
	if !strings.Contains(out, `src="https://cdn.example.com/x.js"`) {
		t.Fatalf("external script src must be preserved, got %q", out)
	}
}

func TestSanitizeCustomizeHTMLStripsInlineEventHandlers(t *testing.T) {
	in := `<img src="/logo.svg" onerror="alert(1)" onload="evil()">`
	out := sanitizeCustomizeHTML(in)
	if strings.Contains(out, "onerror") || strings.Contains(out, "onload") {
		t.Fatalf("on*= attributes must be stripped, got %q", out)
	}
	if strings.Contains(out, "alert(1)") || strings.Contains(out, "evil()") {
		t.Fatalf("event handler payloads must be stripped, got %q", out)
	}
	if !strings.Contains(out, `src="/logo.svg"`) {
		t.Fatalf("non-event attributes must be preserved, got %q", out)
	}
}

func TestSanitizeCustomizeHTMLStripsJavascriptURLs(t *testing.T) {
	in := `<a href="javascript:alert(1)">click</a><link rel="stylesheet" href="/theme.css">`
	out := sanitizeCustomizeHTML(in)
	if strings.Contains(out, "javascript:") {
		t.Fatalf("javascript: URLs must be stripped, got %q", out)
	}
	if !strings.Contains(out, `href="/theme.css"`) {
		t.Fatalf("legitimate href must be preserved, got %q", out)
	}
}

func TestSanitizeCustomizeHTMLPreservesMetaLinkStyle(t *testing.T) {
	in := `<meta name="theme-color" content="#fff"><link rel="stylesheet" href="/custom.css"><style>.a{color:blue}</style>`
	out := sanitizeCustomizeHTML(in)
	for _, want := range []string{"theme-color", `href="/custom.css"`, "color:blue"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q preserved in %q", want, out)
		}
	}
}

func TestSanitizeCustomizeHTMLEmptyStaysEmpty(t *testing.T) {
	if out := sanitizeCustomizeHTML(""); out != "" {
		t.Fatalf("empty input must stay empty, got %q", out)
	}
	if out := sanitizeCustomizeHTML("   "); strings.TrimSpace(out) != "" {
		t.Fatalf("whitespace-only input must stay effectively empty, got %q", out)
	}
}

func TestSanitizeCustomizeHTMLKeepsNestedScriptAndStampsNonce(t *testing.T) {
	in := `<div><script>document.cookie</script><span>keep me</span></div>`
	out := sanitizeCustomizeHTML(in)
	if !strings.Contains(out, "document.cookie") {
		t.Fatalf("nested inline script content must be preserved, got %q", out)
	}
	if !strings.Contains(out, `nonce="`+cspNoncePlaceholder+`"`) {
		t.Fatalf("nested inline script must still get the nonce placeholder, got %q", out)
	}
	if !strings.Contains(out, "keep me") {
		t.Fatalf("sibling content must survive, got %q", out)
	}
}

// --- Red-team fix round 1 (2026-07-27): two real bypasses found by reading
// the golang.org/x/net/html source, PoC'd against sanitizeCustomizeHTML and
// fixed. Both regression tests below reproduce the exact PoC.

// PoC #1: <noscript> is parsed by the HTML5 tree-construction algorithm as a
// RAWTEXT element (same rule as <script>/<style>, per the "in body"/"in head
// noscript" insertion modes when the scripting flag is enabled — which is
// x/net/html's fixed assumption, matching every real browser's default). Its
// content therefore never becomes a real <script> ELEMENT node in the parsed
// tree — it lands as a single opaque TEXT node — so the walker's
// "strip <script> elements" rule never sees it, and the literal text
// `<script>alert(1)</script>` survives byte-for-byte in the sanitized output.
// Fixed by removing <noscript> wholesale: it serves no CSS/meta purpose here,
// so there is no need to reason about RAWTEXT re-parsing edge cases at all.
func TestSanitizeCustomizeHTMLStripsNoscriptWholesale(t *testing.T) {
	in := `<style>body{color:red}</style><noscript><script>alert(1)</script></noscript>`
	out := sanitizeCustomizeHTML(in)
	if strings.Contains(out, "noscript") {
		t.Fatalf("noscript must be removed entirely, got %q", out)
	}
	if strings.Contains(out, "alert(1)") || strings.Contains(out, "<script") {
		t.Fatalf("noscript's RAWTEXT payload must not survive, got %q", out)
	}
	if !strings.Contains(out, "color:red") {
		t.Fatalf("unrelated preceding content must survive, got %q", out)
	}
}

// PoC #2: SVG SMIL animation elements (<animate>, <set>, <animateTransform>,
// <animateMotion>, <animateColor>) can retarget an already-safe attribute
// (e.g. an <a href="/safe">'s href) to a javascript: URL at runtime via their
// to/values/from/by attributes — a classic SVG-based XSS primitive. The old
// dangerousURLAttrs set only covered href/src/action/formaction, so these four
// attribute names sailed through unscanned on every element, and in
// particular on the animation elements themselves.
// Fixed two ways (belt + suspenders): the SMIL animation elements are removed
// wholesale (CSS/meta customization never needs them), AND to/from/values/by
// are added to the scanned URL-attribute set as defense in depth in case a
// future change ever stops removing these elements outright.
func TestSanitizeCustomizeHTMLStripsSVGAnimateJavascriptURL(t *testing.T) {
	in := `<svg><a href="/safe"><animate attributeName="href" to="javascript:alert(1)"/></a></svg>`
	out := sanitizeCustomizeHTML(in)
	if strings.Contains(out, "animate") {
		t.Fatalf("SMIL <animate> must be removed entirely, got %q", out)
	}
	if strings.Contains(out, "javascript:") {
		t.Fatalf("javascript: payload must not survive anywhere in the output, got %q", out)
	}
	if !strings.Contains(out, `href="/safe"`) {
		t.Fatalf("the untouched anchor href must survive, got %q", out)
	}
}

func TestSanitizeCustomizeHTMLStripsAllSVGSMILAnimationElements(t *testing.T) {
	in := `<svg><a href="/safe">` +
		`<animate attributeName="href" to="javascript:alert(1)"/>` +
		`<set attributeName="href" to="javascript:alert(2)"/>` +
		`<animateTransform attributeName="href" to="javascript:alert(3)"/>` +
		`<animateMotion values="javascript:alert(4)"/>` +
		`<animateColor from="javascript:alert(5)" to="javascript:alert(6)"/>` +
		`</a></svg>`
	out := sanitizeCustomizeHTML(in)
	for _, tag := range []string{"animate", "set", "animateTransform", "animateMotion", "animateColor"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(tag)) {
			t.Fatalf("SMIL element %q must be removed entirely, got %q", tag, out)
		}
	}
	if strings.Contains(out, "javascript:") {
		t.Fatalf("no javascript: payload may survive, got %q", out)
	}
}

// Defense-in-depth check for the to/from/values/by URL-attribute scan itself,
// independent of the wholesale SMIL-element removal above: even a
// (hypothetical) element carrying one of these attribute names must still
// have a javascript: value stripped, not just pass through because it wasn't
// on the original href/src/action/formaction list.
func TestSanitizeCustomizeHTMLScansToFromValuesByForJavascriptURLs(t *testing.T) {
	in := `<div to="javascript:alert(1)" from="javascript:alert(2)" values="javascript:alert(3)" by="javascript:alert(4)"></div>`
	out := sanitizeCustomizeHTML(in)
	if strings.Contains(out, "javascript:") {
		t.Fatalf("to/from/values/by must be scanned for javascript: URLs, got %q", out)
	}
}
