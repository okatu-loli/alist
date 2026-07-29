// Theme marketplace admin API (theming spec §J5, final wave; hardened per
// the opus final-review round — see internal/thememarket's and this file's
// inline comments for the specific findings each piece of code addresses).
// Mirrors the plugin_sources.go admin surface (list/add/delete a source) plus
// adds the catalog/install/uninstall verbs a theme (a declarative JSON
// descriptor, no gRPC subprocess like a plugin) actually needs — no manifest,
// no execution mode selection, no risk interstitial: the one meaningful risk
// (custom JS running for a visitor) is gated by TWO layers now: (1) only a
// VERIFIED (signed) source's themes may carry js into storage at all (I2 —
// an untrusted/unsigned source's js is refused outright at install, not
// merely "stored but unprivileged"); (2) even a verified theme's js is never
// auto-enabled — the EXISTING per-theme js-trust mechanism on the frontend
// still gates actually running it, exactly like an imported user theme (see
// src/features/appearance/marketThemes.ts's loader, C1 remediation).
package handles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/thememarket"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// themeSourcesMu / themeInstalledMu serialize the read-modify-write of the
// theme_sources / market_installed_themes settings respectively — same
// concern sourcesMu (plugin_sources.go) guards: op.SaveSettingItem is a
// whole-row upsert, so two concurrent admin requests reading-then-writing
// without a lock can silently clobber each other.
var (
	themeSourcesMu   sync.Mutex
	themeInstalledMu sync.Mutex
)

// currentThemeSources reads the live theme_sources setting.
func currentThemeSources() []thememarket.Source {
	ss, err := thememarket.ParseSources(setting.GetStr(conf.ThemeSources))
	if err != nil {
		return nil
	}
	return ss
}

// currentInstalledThemes reads the live market_installed_themes setting.
func currentInstalledThemes() []thememarket.InstalledTheme {
	ts, err := thememarket.ParseInstalledThemes(setting.GetStr(conf.MarketThemes))
	if err != nil {
		return nil
	}
	return ts
}

// saveThemeSources persists the source list to the theme_sources setting.
// Seeded from the existing item (preserving Help/Index) same as
// plugin_sources.go's saveSources — op.SaveSettingItem is a whole-row upsert.
func saveThemeSources(ss []thememarket.Source) error {
	b, err := json.Marshal(ss)
	if err != nil {
		return err
	}
	return upsertSettingValue(conf.ThemeSources, string(b), conf.TypeText, model.GLOBAL, model.PRIVATE)
}

// saveInstalledThemes persists the installed-theme list to the
// market_installed_themes setting. PRIVATE (I1 remediation): this no longer
// rides the generic, uncached /api/public/settings blob — see
// PublicMarketThemes below, the dedicated ETag'd endpoint that is now the
// ONLY anonymous read path for this data.
func saveInstalledThemes(ts []thememarket.InstalledTheme) error {
	if ts == nil {
		ts = []thememarket.InstalledTheme{}
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	return upsertSettingValue(conf.MarketThemes, string(b), conf.TypeText, model.STYLE, model.PRIVATE)
}

// upsertSettingValue seeds from the existing setting item (if any) so Help/
// Index survive, falling back to the given shape for a not-yet-seeded key.
func upsertSettingValue(key, value, typ string, group, flag int) error {
	item, err := op.GetSettingItemByKey(key)
	if err != nil || item == nil {
		item = &model.SettingItem{Key: key, Type: typ, Group: group, Flag: flag}
	} else {
		clone := *item
		item = &clone
	}
	item.Value = value
	return op.SaveSettingItem(item)
}

// ThemeSourceView is one entry of the theme-marketplace sources list.
type ThemeSourceView struct {
	thememarket.Source
	// Trusted reports whether this source has a configured pubkey (i.e. it
	// CAN sign entries — whether any given entry actually verifies is a
	// per-entry concern, see ThemeCatalogEntryView).
	Trusted bool `json:"trusted"`
}

// ListThemeMarketSources lists every admin-imported theme-marketplace
// source. Admin only.
// @Summary List theme marketplace sources
// @Description Lists every admin-imported theme-marketplace source (name, index URL, whether it has a signing key). Admin only.
// @Tags admin_theme_market
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[[]ThemeSourceView]
// @Router /v4/admin/theme-market/sources [get]
func ListThemeMarketSources(c *gin.Context) {
	common.SuccessResp(c, themeSourceViews())
}

func themeSourceViews() []ThemeSourceView {
	sources := currentThemeSources()
	views := make([]ThemeSourceView, 0, len(sources))
	for _, s := range sources {
		_, hasKey, _ := s.Key()
		views = append(views, ThemeSourceView{Source: s, Trusted: hasKey})
	}
	return views
}

// AddThemeMarketSource imports a theme-marketplace source.
// @Summary Add theme marketplace source
// @Description Imports a theme-marketplace source: a catalog index URL, optionally with the ed25519 public key its catalog entries are signed with. A source with no key is not rejected — its themes simply install as "untrusted" rather than "verified". Admin only.
// @Tags admin_theme_market
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body thememarket.Source true "source to import"
// @Success 200 {object} common.Resp[[]ThemeSourceView]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/theme-market/sources [post]
func AddThemeMarketSource(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	var src thememarket.Source
	if err := c.ShouldBindJSON(&src); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := src.Validate(); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	themeSourcesMu.Lock()
	current := currentThemeSources()
	for _, s := range current {
		if strings.EqualFold(s.Name, src.Name) {
			themeSourcesMu.Unlock()
			common.ErrorStrResp(c, "a source with this name already exists", 400)
			return
		}
	}
	err := saveThemeSources(append(current, src))
	themeSourcesMu.Unlock()
	if err != nil {
		audit.Event(c, user.ID, user.Username, "theme_market.source.add", "source:"+src.Name, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "theme_market.source.add", "source:"+src.Name, true)
	common.SuccessResp(c, themeSourceViews())
}

// DeleteThemeMarketSource removes an imported theme-marketplace source.
// Already-installed themes attributed to it are kept as-is (their stored
// Tier does not retroactively change) — mirrors plugin_sources.go's
// "installed plugins are kept" behaviour for the same reason: the source
// list only governs future catalog fetches/installs, not past ones. Matches
// by EqualFold (M4 — consistent with AddThemeMarketSource's duplicate-name
// check; before this fix a source added as "MyRepo" couldn't be deleted by
// its own case-folded listing if a caller passed a differently-cased name).
// @Summary Remove theme marketplace source
// @Description Removes an imported theme-marketplace source by name (case-insensitive). Already-installed themes from it are kept. Admin only.
// @Tags admin_theme_market
// @Security ApiKeyAuth
// @Produce json
// @Param name path string true "source name"
// @Success 200 {object} common.Resp[[]ThemeSourceView]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/theme-market/sources/{name} [delete]
func DeleteThemeMarketSource(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	name := c.Param("name")
	themeSourcesMu.Lock()
	current := currentThemeSources()
	kept := make([]thememarket.Source, 0, len(current))
	for _, s := range current {
		if !strings.EqualFold(s.Name, name) {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(current) {
		themeSourcesMu.Unlock()
		common.ErrorStrResp(c, "source not found", 404)
		return
	}
	err := saveThemeSources(kept)
	themeSourcesMu.Unlock()
	if err != nil {
		audit.Event(c, user.ID, user.Username, "theme_market.source.remove", "source:"+name, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "theme_market.source.remove", "source:"+name, true)
	common.SuccessResp(c, themeSourceViews())
}

// ThemeCatalogEntryView is one catalog listing annotated with this server's
// trust determination and installed-state, for the marketplace UI to render
// without re-deriving any of it client-side.
type ThemeCatalogEntryView struct {
	thememarket.CatalogEntry
	Tier             string `json:"tier"`
	Blocked          bool   `json:"blocked"`          // true when Reason is a hard rejection (bad signature)
	Reason           string `json:"reason,omitempty"` // set when Blocked
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version,omitempty"`
	UpdateAvailable  bool   `json:"update_available"`
}

// GetThemeMarketCatalog fetches a source's catalog (index.json) and
// annotates each entry with its trust tier and installed/update state.
// @Summary Fetch theme marketplace catalog
// @Description Fetches a theme-marketplace source's catalog and annotates each entry with its trust tier (verified/untrusted), whether it fails signature verification (blocked), and whether it is already installed / has an update available. Admin only.
// @Tags admin_theme_market
// @Security ApiKeyAuth
// @Produce json
// @Param name path string true "source name"
// @Success 200 {object} common.Resp[[]ThemeCatalogEntryView]
// @Failure 404 {object} common.Resp[any]
// @Failure 502 {object} common.Resp[any]
// @Router /v4/admin/theme-market/sources/{name}/catalog [get]
func GetThemeMarketCatalog(c *gin.Context) {
	name := c.Param("name")
	source, ok := findThemeSource(name)
	if !ok {
		common.ErrorStrResp(c, "source not found", 404)
		return
	}
	entries, err := thememarket.FetchCatalog(c.Request.Context(), source.IndexURL)
	if err != nil {
		respondNetworkOrGeneric(c, "fetch the source's catalog", err, 502)
		return
	}
	// I5: keyed by (source, catalog entry id) — NOT the installed record's
	// downloaded descriptor id. Those are different namespaces; keying by
	// descriptor id here made every entry from a NEW source whose catalog id
	// happened to differ from any installed descriptor id show "Install"
	// forever even when it truly was already installed, while a MALICIOUS
	// catalog could deliberately declare a descriptor id matching something
	// already installed from another source and get treated as "the same
	// theme, an update" — exactly the takeover this fixes.
	installed := installedByIdentity(currentInstalledThemes())
	views := make([]ThemeCatalogEntryView, 0, len(entries))
	for _, e := range entries {
		tier, verr := thememarket.VerifyEntry(source, e)
		view := ThemeCatalogEntryView{CatalogEntry: e}
		if verr != nil {
			view.Blocked = true
			view.Reason = verr.Error()
		} else {
			view.Tier = tier
		}
		if inst, ok := installed[installedIdentityKey(source.Name, e.ID)]; ok {
			view.Installed = true
			view.InstalledVersion = inst.Version
			view.UpdateAvailable = versionIsNewer(e.Version, inst.Version)
		}
		views = append(views, view)
	}
	common.SuccessResp(c, views)
}

func findThemeSource(name string) (thememarket.Source, bool) {
	for _, s := range currentThemeSources() {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return thememarket.Source{}, false
}

// installedIdentityKey is the (source, catalog entry id) identity every
// installed record is keyed by (I5) — the theme descriptor's OWN "id" field
// is display-only and is never part of this key.
func installedIdentityKey(source, entryID string) string {
	return source + "\x00" + entryID
}

func installedByIdentity(list []thememarket.InstalledTheme) map[string]thememarket.InstalledTheme {
	m := make(map[string]thememarket.InstalledTheme, len(list))
	for _, t := range list {
		m[installedIdentityKey(t.Source, t.EntryID)] = t
	}
	return m
}

// findInstalledByIdentity looks up an installed record by its (source,
// entry id) identity — the PRIMARY lookup key (I5).
func findInstalledByIdentity(list []thememarket.InstalledTheme, source, entryID string) (thememarket.InstalledTheme, bool) {
	for _, t := range list {
		if t.Source == source && t.EntryID == entryID {
			return t, true
		}
	}
	return thememarket.InstalledTheme{}, false
}

// findInstalledByDescriptorID looks up an installed record by its DOWNLOADED
// descriptor's own "id" field, regardless of (source, entry id) identity.
// Used ONLY to detect a cross-source id collision (I5) — never as a primary
// lookup, since two different (source, entry) identities can legitimately
// carry the same descriptor id only by admin-confirmed force-replace.
func findInstalledByDescriptorID(list []thememarket.InstalledTheme, descriptorID string) (thememarket.InstalledTheme, bool) {
	for _, t := range list {
		if t.ID == descriptorID {
			return t, true
		}
	}
	return thememarket.InstalledTheme{}, false
}

// versionIsNewer reports whether catalogVersion is newer than
// installedVersion, using semver comparison (internal/extension.CompareVersions)
// when both parse as plain MAJOR.MINOR.PATCH (I3) — falling back to "any
// difference counts" only when they're not comparable, since not every
// marketplace entry necessarily uses strict semver.
func versionIsNewer(catalogVersion, installedVersion string) bool {
	cmp, err := extension.CompareVersions(catalogVersion, installedVersion)
	if err != nil {
		return catalogVersion != installedVersion
	}
	return cmp > 0
}

// versionOlder reports whether a is an older version than b when both are
// comparable semver (comparable=true); used by the anti-rollback guard
// below. When not comparable, the caller skips the guard rather than block
// on an indeterminate ordering.
func versionOlder(a, b string) (older bool, comparable bool) {
	cmp, err := extension.CompareVersions(a, b)
	if err != nil {
		return false, false
	}
	return cmp < 0, true
}

// respondNetworkOrGeneric handles a thememarket network-layer failure
// (*thememarket.NetworkError) by logging the full detail server-side and
// returning only a generic message to the client (M6 — the underlying error
// text can embed dial targets/IPs/DNS errors that shouldn't reach the admin
// UI); any OTHER error (a validation failure — bad JSON, sha256 mismatch,
// missing signature, non-http(s) url) is safe to show verbatim and is
// returned as-is with fallbackCode.
func respondNetworkOrGeneric(c *gin.Context, action string, err error, fallbackCode int) {
	var netErr *thememarket.NetworkError
	if errors.As(err, &netErr) {
		log.Errorf("theme_market: %s failed: %v", action, err)
		common.ErrorStrResp(c, fmt.Sprintf("could not %s; see server logs for details", action), 502)
		return
	}
	common.ErrorResp(c, err, fallbackCode)
}

// ThemeMarketInstallReq identifies which catalog entry to install (or
// update — the same verb serves both, see InstallThemeMarketTheme).
type ThemeMarketInstallReq struct {
	Source string `json:"source" binding:"required"`
	// ID is the CATALOG ENTRY's own id (thememarket.CatalogEntry.ID). The
	// identity this install/update targets is (Source, ID) — NOT whatever
	// the downloaded theme descriptor's own "id" field turns out to be (I5):
	// those are two different namespaces that happened to collide in this
	// feature's original tests, which is exactly what let a same-descriptor-
	// id-but-different-source entry silently overwrite an already-installed,
	// already-verified theme.
	ID string `json:"id" binding:"required"`
	// Force explicitly confirms an action that is otherwise refused for
	// safety: replacing an installed theme whose DOWNLOADED descriptor id
	// collides with a DIFFERENT (source, entry) identity's, or reinstalling
	// an OLDER version over a newer one already installed under the SAME
	// identity (a rollback). Defaults false — both cases fail closed
	// (409) without it.
	Force bool `json:"force"`
}

// InstallThemeMarketTheme downloads, verifies, and installs one catalog
// entry — or, if the SAME (source, entry id) identity is already installed,
// updates it in place (this doubles as the "update" verb the marketplace
// UI's per-card "update" action calls). Admin only.
// @Summary Install (or update) a theme marketplace theme
// @Description Downloads a catalog entry's theme by source+id, verifies its sha256 (always) and signature (when the source has a key), and stores it in the installed-themes list — visible to every visitor's picker like site_theme. A theme carrying custom JS is refused unless the source is verified-tier. Re-running this for an already-installed (source, id) identity updates it in place; refuses a version DOWNGRADE or a cross-source descriptor-id collision unless force=true. Admin only.
// @Tags admin_theme_market
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body ThemeMarketInstallReq true "source name + catalog entry id (+ optional force)"
// @Success 200 {object} common.Resp[thememarket.InstalledTheme]
// @Failure 400 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Failure 409 {object} common.Resp[any]
// @Router /v4/admin/theme-market/install [post]
func InstallThemeMarketTheme(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	var req ThemeMarketInstallReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	target := fmt.Sprintf("entry:%s@source:%s", req.ID, req.Source)

	source, ok := findThemeSource(req.Source)
	if !ok {
		common.ErrorStrResp(c, "source not found", 404)
		return
	}
	entries, err := thememarket.FetchCatalog(c.Request.Context(), source.IndexURL)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "theme_market.install", target, false)
		respondNetworkOrGeneric(c, "fetch the source's catalog", err, 502)
		return
	}
	var entry thememarket.CatalogEntry
	found := false
	for _, e := range entries {
		if e.ID == req.ID {
			entry, found = e, true
			break
		}
	}
	if !found {
		audit.Event(c, user.ID, user.Username, "theme_market.install", target, false)
		common.ErrorStrResp(c, "entry not found in source catalog", 404)
		return
	}

	tier, err := thememarket.VerifyEntry(source, entry)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "theme_market.install", target, false)
		common.ErrorResp(c, err, 400)
		return
	}

	data, err := thememarket.DownloadTheme(c.Request.Context(), entry.URL, entry.SHA256)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "theme_market.install", target, false)
		respondNetworkOrGeneric(c, "download the theme", err, 400)
		return
	}
	descriptorID, hasJS, err := thememarket.ValidateDescriptor(data)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "theme_market.install", target, false)
		common.ErrorResp(c, err, 400)
		return
	}

	// I2: only a VERIFIED (signed) source's theme may carry js into storage
	// at all — an untrusted/unsigned source is refused outright here, not
	// merely "stored but unprivileged". This is what stops a random,
	// no-pubkey URL from becoming a site-wide js distribution channel: the
	// only way js reaches ANY user's browser is (a) a signed source (b) that
	// SPECIFIC user then explicitly trusting it via the existing js-trust
	// gate — neither step is skippable.
	if hasJS && tier != thememarket.TierVerified {
		audit.Event(c, user.ID, user.Username, "theme_market.install", target, false)
		common.ErrorStrResp(c, "a theme carrying custom JS can only be installed from a verified (signed) source", 400)
		return
	}

	themeInstalledMu.Lock()
	defer themeInstalledMu.Unlock()

	current := currentInstalledThemes()

	existingSameIdentity, sameIdentityFound := findInstalledByIdentity(current, source.Name, entry.ID)
	conflicting, conflictFound := findInstalledByDescriptorID(current, descriptorID)
	// A "conflict" is a DIFFERENT (source, entry) identity already installed
	// under the same descriptor id — not simply re-finding this exact
	// identity's own prior record (that's just the ordinary update path).
	crossSourceConflict := conflictFound &&
		!(sameIdentityFound && conflicting.Source == existingSameIdentity.Source && conflicting.EntryID == existingSameIdentity.EntryID)

	if crossSourceConflict && !req.Force {
		common.ErrorStrResp(c, fmt.Sprintf(
			"theme id %q is already installed from a different source (%q, entry %q) — set force=true to replace it",
			descriptorID, conflicting.Source, conflicting.EntryID), 409)
		return
	}

	action := "theme_market.install"
	if sameIdentityFound {
		if older, comparable := versionOlder(entry.Version, existingSameIdentity.Version); comparable && older && !req.Force {
			common.ErrorStrResp(c, fmt.Sprintf(
				"catalog version %q is older than the installed version %q — set force=true to downgrade",
				entry.Version, existingSameIdentity.Version), 409)
			return
		}
		action = "theme_market.update"
	}

	record := thememarket.InstalledTheme{
		ID: descriptorID, Source: source.Name, EntryID: entry.ID,
		Name: entry.Name, Author: entry.Author, Version: entry.Version,
		Tier: tier, HasJS: hasJS, InstalledAt: time.Now().Unix(),
		Theme: json.RawMessage(data),
	}

	next := make([]thememarket.InstalledTheme, 0, len(current)+1)
	for _, t := range current {
		if t.Source == source.Name && t.EntryID == entry.ID {
			continue // replaced by `record` below (ordinary same-identity update)
		}
		if crossSourceConflict && t.Source == conflicting.Source && t.EntryID == conflicting.EntryID {
			continue // force-evicted cross-source conflict
		}
		next = append(next, t)
	}
	next = append(next, record)

	if err := saveInstalledThemes(next); err != nil {
		audit.Event(c, user.ID, user.Username, action, target, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, action, target, true)
	common.SuccessResp(c, record)
}

// ListInstalledThemeMarketThemes lists every installed marketplace theme
// (the parsed, admin-friendly view — the same underlying data every visitor
// can read anonymously through the dedicated GET /public/themes, but
// pre-parsed here for the admin UI's convenience).
// @Summary List installed theme marketplace themes
// @Description Lists every theme installed from the theme marketplace. Admin only (the same list is also readable anonymously, unparsed, through GET /public/themes — this is a parsed convenience view for the admin UI).
// @Tags admin_theme_market
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[[]thememarket.InstalledTheme]
// @Router /v4/admin/theme-market/installed [get]
func ListInstalledThemeMarketThemes(c *gin.Context) {
	list := currentInstalledThemes()
	if list == nil {
		list = []thememarket.InstalledTheme{}
	}
	common.SuccessResp(c, list)
}

// UninstallThemeMarketTheme removes one installed theme by its (source,
// entry id) IDENTITY (I5) — NOT by the theme's own descriptor id, which is
// display-only and is no longer how installed records are addressed.
// @Summary Uninstall a theme marketplace theme
// @Description Removes an installed marketplace theme by its (source, catalog entry id) identity. Admin only.
// @Tags admin_theme_market
// @Security ApiKeyAuth
// @Produce json
// @Param source path string true "source name"
// @Param entryId path string true "catalog entry id"
// @Success 200 {object} common.Resp[[]thememarket.InstalledTheme]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/theme-market/installed/{source}/{entryId} [delete]
func UninstallThemeMarketTheme(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	source := c.Param("source")
	entryID := c.Param("entryId")
	themeInstalledMu.Lock()
	defer themeInstalledMu.Unlock()
	current := currentInstalledThemes()
	kept := make([]thememarket.InstalledTheme, 0, len(current))
	removed := false
	for _, t := range current {
		if t.Source == source && t.EntryID == entryID {
			removed = true
			continue
		}
		kept = append(kept, t)
	}
	if !removed {
		common.ErrorStrResp(c, "installed theme not found", 404)
		return
	}
	err := saveInstalledThemes(kept)
	target := fmt.Sprintf("entry:%s@source:%s", entryID, source)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "theme_market.uninstall", target, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "theme_market.uninstall", target, true)
	common.SuccessResp(c, kept)
}

// PublicMarketThemes serves the installed theme-marketplace list
// ANONYMOUSLY (I1 remediation). Moved OFF the general /api/public/settings
// blob — which carries no ETag/conditional-caching support and is fetched on
// literally every anonymous page load — onto this DEDICATED route with real
// ETag-based conditional caching: a repeat visitor within the same content
// version gets a bodyless 304 instead of paying the full byte count every
// time. conf.MarketThemes is now PRIVATE (internal/bootstrap/data/setting.go)
// so it no longer appears in the generic public-settings map at all — this
// is the ONLY anonymous read path for it.
// @Summary Public installed theme marketplace themes
// @Description Returns every theme installed from the theme marketplace, no authentication required. Supports conditional GET via ETag/If-None-Match.
// @Tags public
// @Produce json
// @Success 200 {object} common.Resp[[]thememarket.InstalledTheme]
// @Router /v4/public/themes [get]
func PublicMarketThemes(c *gin.Context) {
	raw := setting.GetStr(conf.MarketThemes)
	sum := sha256.Sum256([]byte(raw))
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	c.Header("Cache-Control", publicSettingsCacheControl)
	c.Header("ETag", etag)
	if match := c.GetHeader("If-None-Match"); match != "" && etagMatches(match, etag) {
		// c.Status alone only records the code on gin's buffered
		// responseWriter; nothing here calls Write (which is what normally
		// flushes it), so WriteHeaderNow is required to actually send a
		// (bodyless) 304 rather than gin defaulting to 200 once the handler
		// returns having written nothing (same seam thumbnail.go's ETag path
		// guards against).
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	list, err := thememarket.ParseInstalledThemes(raw)
	if err != nil {
		list = nil
	}
	if list == nil {
		list = []thememarket.InstalledTheme{}
	}
	common.SuccessResp(c, list)
}

// etagMatches reports whether ifNoneMatch (the raw If-None-Match header,
// which may list multiple comma-separated values and use the weak "W/"
// prefix) matches etag. A bare "*" always matches (RFC 7232).
func etagMatches(ifNoneMatch, etag string) bool {
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, part := range strings.Split(ifNoneMatch, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		if p == etag {
			return true
		}
	}
	return false
}
