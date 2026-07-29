package op

import (
	"encoding/json"
	"strings"

	"github.com/alist-org/alist/v3/internal/thememarket"
	"github.com/pkg/errors"
)

// MarketThemesMaxBytes caps the conf.MarketThemes setting value (a JSON
// array of thememarket.InstalledTheme). Sized for a curated handful of
// admin-installed marketplace themes rather than a single one (unlike
// SiteThemeMaxBytes, which bounds exactly one descriptor). Lowered from an
// initial 2 MiB to 512 KiB (opus final-review, I1): even though this data now
// rides its OWN dedicated, ETag-cacheable endpoint (GET /public/themes —
// server/handles/theme_market.go's PublicMarketThemes, not the generic
// /api/public/settings blob), the FIRST anonymous fetch after any change
// still pays the full byte count, so the cap stays deliberately conservative
// as defense in depth rather than relying solely on the endpoint split.
const MarketThemesMaxBytes = 512 << 10 // 512 KiB

// validateMarketThemes enforces conf.MarketThemes's safety constraints before
// it is persisted. An empty (or whitespace-only, or "[]") value is always
// valid — no themes installed.
//
// A "js" field inside an entry's Theme payload is NOT rejected here — see
// conf.MarketThemes's doc comment: a marketplace theme carrying custom JS is
// allowed to be STORED, it is simply never auto-enabled; running it still
// requires the same per-user js-trust gate every other theme source goes
// through (site_theme's js now ALSO reaches this same per-browser jsTrust
// gate — auto-granted at load time in every visitor's browser, since a site
// theme has no per-visitor review step to separately gate on — see
// site_theme.go's validateSiteTheme doc comment for that policy reversal).
// What IS enforced here:
//   - the overall size cap (MarketThemesMaxBytes)
//   - the value parses as a JSON array of thememarket.InstalledTheme
//   - every entry has a non-empty id and a Theme payload that is itself
//     valid JSON (not merely opaque bytes)
//   - no duplicate ids (the install/uninstall handlers dedupe by id; a
//     duplicate reaching here would mean two entries silently shadow each
//     other depending on iteration order — reject instead)
func validateMarketThemes(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "[]" {
		return nil
	}
	if len(value) > MarketThemesMaxBytes {
		return errors.Errorf("market_installed_themes exceeds the %d byte limit (got %d bytes)", MarketThemesMaxBytes, len(value))
	}
	var list []thememarket.InstalledTheme
	if err := json.Unmarshal([]byte(value), &list); err != nil {
		return errors.WithMessage(err, "market_installed_themes must be a valid JSON array")
	}
	seen := make(map[string]bool, len(list))
	seenIdentity := make(map[string]bool, len(list))
	for i, item := range list {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return errors.Errorf("market_installed_themes[%d]: missing id", i)
		}
		if seen[id] {
			return errors.Errorf("market_installed_themes[%d]: duplicate id %q", i, id)
		}
		seen[id] = true
		// I5: the real install identity is (source, entry_id) — enforce its
		// uniqueness here too, at the storage layer, not just in the admin
		// install/uninstall handlers' own bookkeeping.
		identity := item.Source + "\x00" + item.EntryID
		if seenIdentity[identity] {
			return errors.Errorf("market_installed_themes[%d]: duplicate (source, entry_id) identity", i)
		}
		seenIdentity[identity] = true
		if len(item.Theme) == 0 {
			return errors.Errorf("market_installed_themes[%d]: missing theme payload", i)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item.Theme, &obj); err != nil {
			return errors.Errorf("market_installed_themes[%d]: theme payload is not valid JSON: %v", i, err)
		}
	}
	return nil
}
