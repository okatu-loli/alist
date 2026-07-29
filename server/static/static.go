package static

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/public"
	"github.com/gin-gonic/gin"
)

var static fs.FS

func initStatic() {
	if conf.Conf.DistDir == "" {
		dist, err := fs.Sub(public.Public, "dist")
		if err != nil {
			utils.Log.Fatalf("failed to read dist dir")
		}
		static = dist
		return
	}
	static = os.DirFS(conf.Conf.DistDir)
}

func initIndex() {
	indexFile, err := static.Open("index.html")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			utils.Log.Fatalf("index.html not exist, you may forget to put dist of frontend to public/dist")
		}
		utils.Log.Fatalf("failed to read index.html: %v", err)
	}
	defer func() {
		_ = indexFile.Close()
	}()
	index, err := io.ReadAll(indexFile)
	if err != nil {
		utils.Log.Fatalf("failed to read dist/index.html")
	}
	conf.RawIndexHtml = string(index)
	siteConfig := getSiteConfig()
	replaceMap := map[string]string{
		"cdn: undefined":       fmt.Sprintf("cdn: '%s'", siteConfig.Cdn),
		"base_path: undefined": fmt.Sprintf("base_path: '%s'", siteConfig.BasePath),
	}
	for k, v := range replaceMap {
		conf.RawIndexHtml = strings.Replace(conf.RawIndexHtml, k, v, 1)
	}
	UpdateIndex()
}

func UpdateIndex() {
	// NOTE: favicon/title/main_color used to be patched into index.html here
	// via string replacement (matching literal placeholders the old v3
	// frontend template shipped: a jsd.nn.ci logo URL, a "Loading..." title,
	// and a "main_color: undefined" JS var). The v4 frontend template
	// (alist-v4-web/index.html) carries none of those strings — it links
	// /favicon.svg directly and has no such JS var — so those replacements were
	// silent no-ops. Removed rather than kept as dead code; the settings
	// themselves (conf.Favicon/conf.SiteTitle/conf.MainColor) are still read
	// directly by other call sites (server/handles/helper.go's favicon
	// redirect, the TOTP/passkey display name) and by the frontend via the
	// public settings API, so nothing here loses functionality.
	customizeHead := setting.GetStr(conf.CustomizeHead)
	customizeBody := setting.GetStr(conf.CustomizeBody)
	applyCustomizeHTML(customizeHead, customizeBody)
}

// applyCustomizeHTML rebuilds conf.ManageHtml/conf.IndexHtml from the parsed
// conf.RawIndexHtml, sanitizing and injecting customizeHead/customizeBody.
// Split out from UpdateIndex so the templating logic is unit-testable without
// a live settings store (setting.GetStr needs op's DB-backed cache).
func applyCustomizeHTML(customizeHead, customizeBody string) {
	conf.ManageHtml = conf.RawIndexHtml
	conf.IndexHtml = conf.ManageHtml
	// customize_head/customize_body are CSS/meta-only (theming spec §J0-1):
	// script-src in csp.go has no 'unsafe-inline' and is 'self'-scoped, so any
	// <script>/inline-event content here is already inert in the browser.
	// Sanitize before injection so the served markup matches that contract
	// instead of leaving inert-but-executable-looking script content in place.
	replaceMap2 := map[string]string{
		"<!-- customize head -->": sanitizeCustomizeHTML(customizeHead),
		"<!-- customize body -->": sanitizeCustomizeHTML(customizeBody),
	}
	for k, v := range replaceMap2 {
		conf.IndexHtml = strings.Replace(conf.IndexHtml, k, v, 1)
	}
}

func Static(r *gin.RouterGroup, noRoute func(handlers ...gin.HandlerFunc)) {
	initStatic()
	initIndex()
	folders := []string{"assets", "images", "streamer", "static"}
	r.Use(func(c *gin.Context) {
		for i := range folders {
			if strings.HasPrefix(c.Request.RequestURI, fmt.Sprintf("/%s/", folders[i])) {
				c.Header("Cache-Control", "public, max-age=15552000")
			}
		}
	})
	for i, folder := range folders {
		sub, err := fs.Sub(static, folder)
		if err != nil {
			utils.Log.Fatalf("can't find folder: %s", folder)
		}
		r.StaticFS(fmt.Sprintf("/%s/", folders[i]), http.FS(sub))
	}

	// Serve the browser-tab favicon that index.html links as "/favicon.svg"
	// directly from the embedded dist. The folder mounts above only cover
	// assets/images/streamer/static, so a top-level file like favicon.svg
	// otherwise falls through to the SPA noRoute handler and returns
	// index.html — the tab icon then never resolves. (/favicon.ico keeps its
	// existing redirect route registered in server/router.go.)
	if f, err := static.Open("favicon.svg"); err == nil {
		_ = f.Close()
		r.StaticFileFS("/favicon.svg", "favicon.svg", http.FS(static))
	}

	noRoute(func(c *gin.Context) {
		if c.Request.Method != "GET" && c.Request.Method != "POST" {
			c.Status(405)
			return
		}
		html := conf.IndexHtml
		if strings.HasPrefix(c.Request.URL.Path, "/@manage") {
			// Intentional (theming spec §J0-2): conf.ManageHtml is a copy of
			// conf.RawIndexHtml taken BEFORE the customize_head/customize_body
			// placeholders are substituted (see UpdateIndex above), so the admin
			// panel never receives site-customization content. The admin panel is
			// the management surface, not the customized front-end site, and
			// should not be affected by what an admin puts in these settings.
			html = conf.ManageHtml
		}
		// Per-response CSP (theming spec §5.5.4): mint a fresh nonce, expose it to
		// the frontend via a <meta> tag, and authorize it in style-src. A minting
		// failure (empty nonce) still serves the page — the sanitizer is the
		// primary gate; the app just falls back to no working style nonce.
		nonce := newNonce()
		// frame-ancestors is relaxed to 'self' ONLY for the one designated
		// preview route (root path + "?__preview=1" — see
		// isPreviewFramingRequest's doc comment for the clickjacking gap this
		// closes); every other response, /@manage/* very much included, keeps
		// 'none' even if it carries the identical query string.
		allowSelfFraming := isPreviewFramingRequest(c.Request.URL.Path, c.Request.URL.RawQuery)
		// script_src_allowlist admits an EXTERNAL <script src="..."> pasted into
		// customize_head/customize_body (sanitize.go) by origin. Read live (not
		// baked into conf.IndexHtml at UpdateIndex time) since it's a response
		// HEADER, not embedded markup — same reasoning as the nonce itself.
		scriptSrcAllowlist := parseScriptSrcAllowlist(setting.GetStr(conf.ScriptSrcAllowlist))
		c.Header("Content-Security-Policy", cspHeader(nonce, allowSelfFraming, scriptSrcAllowlist))
		// customize_head/customize_body may carry an INLINE <script> already
		// stamped with the nonce PLACEHOLDER at sanitize time (sanitize.go) —
		// conf.IndexHtml is built once (UpdateIndex), well before any single
		// request's real nonce exists, so that placeholder is substituted here,
		// per request, exactly like the CSP header's own nonce. Must happen
		// BEFORE injectNonceMeta, which inserts the live value directly (not the
		// placeholder) and would otherwise leave the <script> nonce stale.
		html = strings.ReplaceAll(html, cspNoncePlaceholder, nonce)
		html = injectNonceMeta(html, nonce)

		c.Header("Content-Type", "text/html")
		c.Status(200)
		_, _ = c.Writer.WriteString(html)
		c.Writer.Flush()
		c.Writer.WriteHeaderNow()
	})
}
