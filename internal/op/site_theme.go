package op

import (
	"encoding/json"
	"strings"

	"github.com/pkg/errors"
)

// SiteThemeMaxBytes caps the conf.SiteTheme setting value (a full frontend
// theme descriptor JSON — tokens/radius/font/css/branding/notification). The
// cap keeps the always-PUBLIC /api/public/settings response cheap regardless
// of what an admin pastes in (e.g. a base64-embedded font or a huge css
// block) — unlike a per-user local theme (only ever read by that one
// browser), this value is served to every unauthenticated visitor on every
// page load, and that route carries no gzip/Cache-Control-driven revalidation
// savings today (PublicSettings sets a short max-age — see server/handles/
// setting.go — but every cache MISS still pays the full byte count).
//
// 64KiB (not the 256KiB first shipped): a real hand-authored theme —
// tokens + a restricted css block + branding/notification — comes nowhere
// close to this even at generous size (tokens.css's whole custom-property
// set is a few KB of text; the css sanitizer already rejects url()/data: any
// further inflation is unusual authored CSS, not typical usage). site_theme
// also never carries bundled font BYTES (fonts[] with base64 payloads) — the
// loader/prepare gate only accepts font.sans, a family NAME, for a
// source:"user" theme (see src/features/appearance/siteTheme.ts) — so the
// "someone pastes a base64 font in" risk 256KiB was sized against doesn't
// even apply to this setting; 64KiB is a comfortable ceiling above genuine
// content with a much tighter blast radius on the anonymous endpoint.
const SiteThemeMaxBytes = 64 * 1024

// validateSiteTheme enforces conf.SiteTheme's VALUE-shape constraints before
// it is persisted (JSON validity + size cap). An empty (or whitespace-only)
// value is always valid — it means "use the built-in default theme".
//
// PRODUCT DECISION (reversed 2026-07): a site-wide theme's "js" field used to
// be refused outright here — a site theme applies to EVERY visitor including
// anonymous ones, with no per-visitor consent step like a personal theme's
// js-trust gate (spec §D5). That blanket refusal is overturned: a theme MAY
// now carry js. This validator does NOT enforce the confirmation gate that
// makes that safe — it is a pure value-shape check, reused by BOTH the
// generic bulk-save path (which refuses a js-carrying value outright,
// regardless of confirmation) and the dedicated confirmed-save endpoint
// (which allows it once js_confirmed is asserted). See conf.SiteTheme's doc
// comment for the full two-layer server-side enforcement (this was
// originally reasoned as a client-side-only gate — a HIGH-severity red-team
// finding, since this repo's RBAC supports a role scoped to ONLY
// setting:manage, which could otherwise hit the generic endpoint directly
// and run arbitrary JS for every visitor with zero review; "an admin with API
// access to this setting could run code anyway" does NOT hold once a
// non-admin, setting:manage-only role exists).
//
// What IS rejected outright by THIS function: a value that isn't valid JSON,
// and anything over SiteThemeMaxBytes.
func validateSiteTheme(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if len(value) > SiteThemeMaxBytes {
		return errors.Errorf("site_theme exceeds the %d byte limit (got %d bytes)", SiteThemeMaxBytes, len(value))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &obj); err != nil {
		return errors.WithMessage(err, "site_theme must be a valid JSON object")
	}
	return nil
}

// SiteThemeCarriesJS reports whether a raw conf.SiteTheme JSON value carries
// a non-blank top-level "js" field. Exported for server/handles/setting.go
// (the generic bulk-save endpoint refuses any such value outright) and
// server/handles/site_theme.go (the dedicated endpoint requires
// js_confirmed only when this is true). Tolerant of malformed JSON — returns
// false rather than erroring, since a malformed value is caught separately by
// validateSiteTheme's own JSON check; this helper only answers the narrower
// "does it carry js" question for otherwise-parseable input.
func SiteThemeCarriesJS(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	var obj struct {
		JS string `json:"js"`
	}
	if err := json.Unmarshal([]byte(value), &obj); err != nil {
		return false
	}
	return strings.TrimSpace(obj.JS) != ""
}
