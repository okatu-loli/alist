package static

import (
	"strings"

	"github.com/alist-org/alist/v3/pkg/utils"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// dangerousURLAttrs lists attributes whose value can itself carry a
// javascript: pseudo-URL and trigger script execution without ever needing a
// <script> tag (e.g. <a href="javascript:...">, <form action="javascript:...">).
// Filtered as defense-in-depth alongside <script> and on*="" handlers.
//
// to/from/values/by are SVG SMIL animation attributes (<animate to="...">,
// <set to="...">, etc.) that can retarget an already-safe attribute (e.g. an
// anchor's href) to a javascript: URL at runtime — a documented SVG XSS
// vector. smilAnimationTags below already removes the elements that carry
// these wholesale, so this entry is belt-and-suspenders: it still catches a
// javascript: value in these attribute names on whatever element they end up
// on, independent of that element-level removal.
var dangerousURLAttrs = map[string]bool{
	"href":       true,
	"src":        true,
	"action":     true,
	"formaction": true,
	"to":         true,
	"from":       true,
	"values":     true,
	"by":         true,
}

// smilAnimationTags are SVG SMIL animation elements — <animate>, <set>,
// <animateTransform>, <animateMotion>, <animateColor> — removed wholesale
// (not merely attribute-scrubbed) because they exist specifically to mutate
// another element's attributes over time, which is exactly the primitive a
// static CSS/meta customization has no legitimate use for, and scrubbing
// their to/values/from/by attributes alone (see dangerousURLAttrs) would still
// leave a live animation primitive an admin never needed. Matched by lower-
// cased tag name within the SVG namespace only (see sanitizeCustomizeHTML),
// so an unrelated custom element named e.g. <set> outside an <svg> is left
// alone. golang.org/x/net/html preserves the spec-mandated SVG foreign-content
// camelCase for these (verified: animateTransform/animateMotion/animateColor
// come through with their real casing in Node.Data, not lowercased), but this
// map is keyed lowercase and matching lowercases n.Data first, so casing
// doesn't matter either way.
var smilAnimationTags = map[string]bool{
	"animate":          true,
	"set":              true,
	"animatetransform": true,
	"animatemotion":    true,
	"animatecolor":     true,
}

// sanitizeCustomizeHTML enforces the customize_head/customize_body contract
// (theming spec §J0-1, POLICY REVERSED 2026-07 — see conf.CustomizeHead's doc
// comment). Script content now genuinely executes:
//   - An inline <script> (no `src` attribute) is auto-stamped with the
//     per-response CSP nonce placeholder (cspNoncePlaceholder, csp.go — the
//     SAME token the CSP header itself carries, substituted for the live
//     nonce at request time by static.go's noRoute handler), so it runs under
//     the existing nonce-gated script-src with no 'unsafe-inline' relaxation
//     needed. Any nonce attribute the admin's OWN markup carried is stripped
//     first — an admin can't spoof/guess a nonce, and only the host-stamped
//     one is ever trusted.
//   - An external <script src="..."> is left alone (no nonce added — CSP
//     nonces admit ANY origin once present, which would defeat the point of a
//     domain allowlist); it runs only if its origin is in the
//     script_src_allowlist setting, which csp.go folds into the CSP
//     script-src directive dynamically. Only its OWN nonce attribute (if any)
//     is stripped, for the same spoofing reason as above.
//   - <noscript> elements are still removed entirely (tag + contents) — their
//     content is parsed as RAWTEXT (opaque text, not real elements), so this
//     sidesteps a re-parsing edge case rather than trying to safely
//     re-sanitize opaque text; see the inline comment on the noscript check.
//   - SVG SMIL animation elements (<animate>, <set>, <animateTransform>,
//     <animateMotion>, <animateColor>) are still removed entirely — they can
//     retarget an already-safe attribute to a javascript: URL at runtime, and
//     neither they nor a javascript: URL can meaningfully carry a nonce, so
//     there's no tradeoff in continuing to strip them unconditionally.
//   - on*="" attributes (onclick, onerror, onload, ...) are still removed
//     from every element, for the same reason.
//   - javascript: URLs in href/src/action/formaction/to/from/values/by are
//     still removed.
//   - everything else — <style>, <link>, <meta>, and arbitrary structural
//     markup admins wrap their content in — passes through untouched.
//
// This is NOT a general-purpose XSS sanitizer (it does not attempt to cover
// every documented HTML-based script vector, e.g. srcdoc on <iframe> or CSS
// expression() in old IE) — it enforces exactly the two things this feature
// asks for: scripts genuinely run (nonce'd inline / allowlisted external),
// while the specific handful of attack-only primitives that can't carry a
// nonce (on*=, javascript:, SVG SMIL retargeting) stay stripped regardless.
//
// Limitation: round-tripping through golang.org/x/net/html's parser/renderer
// normalizes markup (attribute quoting, void-element form, whitespace) — the
// output is not guaranteed byte-identical to the input, only equivalent minus
// the stripped surface. Malformed input is handled the same permissive way a
// browser's own HTML5 parser recovers from it (ParseFragment never errors on
// ill-formed markup in practice; a defensive error path still exists below in
// case that ever changes).
func sanitizeCustomizeHTML(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	// Parse as a body-context fragment: <style>/<link>/<meta>/<script> are all
	// valid "in body" per the HTML5 tree-construction algorithm, so this
	// accepts the same shapes admins actually paste into these settings
	// (which land inside <head> or just before </body> — see static.go)
	// without needing a real document context.
	context := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(raw), context)
	if err != nil {
		utils.Log.Warnf("customize html sanitizer: failed to parse customize_head/customize_body, dropping content: %v", err)
		return ""
	}

	stripped := false

	// keep reports whether n should stay in the tree; it also mutates n's
	// attributes/children in place.
	var keep func(n *html.Node) bool
	keep = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.DataAtom == atom.Noscript {
			// <noscript>: the HTML5 tree-construction algorithm parses its
			// content as RAWTEXT (same rule family as <script>/<style>) whenever
			// the scripting flag is enabled — which is what x/net/html always
			// assumes, matching every real browser's default — so anything
			// pasted inside <noscript> never becomes real element nodes; it
			// lands as one opaque TEXT child that the attribute/element walk
			// below never inspects (red-team PoC:
			// TestSanitizeCustomizeHTMLStripsNoscriptWholesale). <noscript> has
			// no use case here (scripting is always enabled in every real
			// browser this app targets), so removing it wholesale sidesteps any
			// RAWTEXT-reparsing edge case entirely rather than trying to safely
			// re-sanitize opaque text.
			stripped = true
			return false
		}
		if n.Type == html.ElementNode && n.Namespace == "svg" && smilAnimationTags[strings.ToLower(n.Data)] {
			// SVG SMIL animation elements (<animate>, <set>, <animateTransform>,
			// <animateMotion>, <animateColor>) can retarget an already-safe
			// attribute (e.g. an <a href="/safe">'s href) to a javascript: URL
			// at runtime via to/values/from/by — a documented SVG XSS vector
			// (red-team PoC: TestSanitizeCustomizeHTMLStripsSVGAnimateJavascriptURL).
			// Removed wholesale rather than merely attribute-scrubbed: nothing
			// in a CSS/meta customization needs a live attribute-animation
			// primitive. Scoped to the SVG namespace so an unrelated custom
			// element named e.g. <set> elsewhere is left alone.
			stripped = true
			return false
		}
		if n.Type == html.ElementNode && len(n.Attr) > 0 {
			kept := n.Attr[:0]
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				switch {
				case strings.HasPrefix(key, "on"):
					stripped = true
					continue
				case dangerousURLAttrs[key] && isJavascriptURL(a.Val):
					stripped = true
					continue
				case key == "nonce":
					// An admin's OWN nonce attribute is never trusted — only
					// the host-stamped one added below (inline <script> only)
					// is ever admitted by the live CSP. Dropped unconditionally
					// (on any element, not just <script>) so nothing here can
					// even look like it's pre-authorized.
					continue
				}
				kept = append(kept, a)
			}
			n.Attr = kept
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Script {
			// Script execution policy (module doc, product decision reversal):
			// an INLINE <script> (no src) gets the host nonce placeholder so it
			// runs under the existing nonce-gated script-src; an EXTERNAL
			// <script src="..."> gets none — CSP nonces admit any origin once
			// present, which would silently bypass the script_src_allowlist's
			// whole point of scoping external execution to specific domains.
			hasSrc := false
			for _, a := range n.Attr {
				if strings.ToLower(a.Key) == "src" {
					hasSrc = true
					break
				}
			}
			if !hasSrc {
				n.Attr = append(n.Attr, html.Attribute{Key: "nonce", Val: cspNoncePlaceholder})
			}
		}
		for child := n.FirstChild; child != nil; {
			next := child.NextSibling
			if !keep(child) {
				n.RemoveChild(child)
			}
			child = next
		}
		return true
	}

	var out strings.Builder
	for _, n := range nodes {
		if !keep(n) {
			continue
		}
		if err := html.Render(&out, n); err != nil {
			utils.Log.Warnf("customize html sanitizer: failed to render sanitized content: %v", err)
		}
	}
	if stripped {
		utils.Log.Warnf("customize html sanitizer: removed noscript/SVG-SMIL/inline-event-handler/javascript: content from customize_head or customize_body (script tags themselves are now allowed — see sanitizeCustomizeHTML's doc comment)")
	}
	return out.String()
}

// isJavascriptURL reports whether v is (or, after trimming ignorable
// characters, resolves to) a javascript: pseudo-URL. It does not attempt a
// full URL parse — this is a defense-in-depth check layered on top of the CSP
// script-src restriction, not the primary gate.
func isJavascriptURL(v string) bool {
	// Browsers tolerate stray control characters/whitespace inside the scheme
	// portion of a URL; strip them before matching so `java\tscript:` style
	// obfuscation doesn't slip through.
	var b strings.Builder
	for _, r := range v {
		if r <= 0x20 {
			continue
		}
		b.WriteRune(r)
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(b.String())), "javascript:")
}
