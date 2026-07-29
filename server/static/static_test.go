package static

import (
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
)

// v4FrontendIndexFixture mirrors the real v4 frontend template
// (alist-v4-web/index.html, and its built public/dist/index.html copy this
// repo embeds): a bare favicon link, a literal (non-"Loading...") title, no
// "main_color: undefined" JS var, and the two customize placeholders. None of
// the three v3-era literals static.go used to replace (a jsd.nn.ci logo URL,
// "Loading...", "main_color: undefined") appear anywhere in it.
const v4FrontendIndexFixture = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <link rel="icon" type="image/svg+xml" href="/favicon.svg" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>alist-v4-web</title>
    <!-- customize head -->
    <script type="module" crossorigin src="/assets/index-BCp0NAvK.js"></script>
    <link rel="stylesheet" crossorigin href="/assets/index-DS1U_Qk_.css">
  </head>
  <body>
    <div id="root"></div>
    <!-- customize body -->
  </body>
</html>
`

func withRawIndexHtml(t *testing.T, html string, fn func()) {
	t.Helper()
	prevRaw, prevIndex, prevManage := conf.RawIndexHtml, conf.IndexHtml, conf.ManageHtml
	conf.RawIndexHtml = html
	t.Cleanup(func() {
		conf.RawIndexHtml, conf.IndexHtml, conf.ManageHtml = prevRaw, prevIndex, prevManage
	})
	fn()
}

// J0-1 (POLICY REVERSED 2026-07): a customize_head containing an inline
// <script> now DOES survive into the served (front-end) IndexHtml — stamped
// with the CSP nonce PLACEHOLDER (the live per-response nonce is substituted
// in later, at request time, by Static's noRoute handler — conf.IndexHtml is
// built once here, well before any single request's nonce exists). CSS/meta
// content in customize_body is preserved exactly as before.
func TestApplyCustomizeHTMLKeepsScriptNoncedInIndexHtml(t *testing.T) {
	withRawIndexHtml(t, v4FrontendIndexFixture, func() {
		applyCustomizeHTML(
			`<script>alert('xss')</script><style>body{background:#000}</style>`,
			`<style>.footer{color:#fff}</style>`,
		)
		if !strings.Contains(conf.IndexHtml, "alert('xss')") {
			t.Fatalf("customize_head inline script content must be preserved in IndexHtml:\n%s", conf.IndexHtml)
		}
		if !strings.Contains(conf.IndexHtml, `nonce="`+cspNoncePlaceholder+`"`) {
			t.Fatalf("customize_head inline script must carry the CSP nonce placeholder in IndexHtml:\n%s", conf.IndexHtml)
		}
		if !strings.Contains(conf.IndexHtml, "background:#000") {
			t.Fatalf("customize_head style must be preserved in IndexHtml:\n%s", conf.IndexHtml)
		}
		if !strings.Contains(conf.IndexHtml, "color:#fff") {
			t.Fatalf("customize_body style must be preserved in IndexHtml:\n%s", conf.IndexHtml)
		}
	})
}

// J0-2: conf.ManageHtml (served for /@manage) must never receive
// customize_head/customize_body content, sanitized or not — the admin panel
// is intentionally unaffected by site customization.
func TestApplyCustomizeHTMLNeverInjectsIntoManageHtml(t *testing.T) {
	withRawIndexHtml(t, v4FrontendIndexFixture, func() {
		applyCustomizeHTML(`<style>body{background:#000}</style>`, `<style>.footer{color:#fff}</style>`)
		if strings.Contains(conf.ManageHtml, "background:#000") || strings.Contains(conf.ManageHtml, "color:#fff") {
			t.Fatalf("ManageHtml must not receive customize content:\n%s", conf.ManageHtml)
		}
		if !strings.Contains(conf.ManageHtml, "<!-- customize head -->") || !strings.Contains(conf.ManageHtml, "<!-- customize body -->") {
			t.Fatalf("ManageHtml must keep the raw customize placeholders untouched:\n%s", conf.ManageHtml)
		}
	})
}

// J0-3: with no customize content configured (the common case), removing the
// dead favicon/"Loading..."/main_color replacements must leave the produced
// IndexHtml/ManageHtml identical to the raw v4 template but for the customize
// placeholders being blanked — i.e. the favicon link, title, and script tag
// the old replaceMap1 targeted (and never matched, in v4) are byte-for-byte
// unchanged.
func TestApplyCustomizeHTMLIsNoOpOnV4TemplateWhenCustomizeEmpty(t *testing.T) {
	withRawIndexHtml(t, v4FrontendIndexFixture, func() {
		applyCustomizeHTML("", "")
		want := strings.NewReplacer(
			"<!-- customize head -->", "",
			"<!-- customize body -->", "",
		).Replace(v4FrontendIndexFixture)
		if conf.IndexHtml != want {
			t.Fatalf("IndexHtml changed beyond blanking the customize placeholders.\ngot:\n%s\nwant:\n%s", conf.IndexHtml, want)
		}
		// Specifically: none of the old dead-literal targets were touched.
		for _, want := range []string{
			`href="/favicon.svg"`,
			"<title>alist-v4-web</title>",
			`src="/assets/index-BCp0NAvK.js"`,
		} {
			if !strings.Contains(conf.IndexHtml, want) {
				t.Fatalf("expected %q untouched in IndexHtml:\n%s", want, conf.IndexHtml)
			}
		}
	})
}
