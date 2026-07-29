package data

import (
	"strconv"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

var initialSettingItems []model.SettingItem

func initSettings() {
	InitialSettings()
	// check deprecated
	settings, err := op.GetSettingItems()
	if err != nil {
		utils.Log.Fatalf("failed get settings: %+v", err)
	}
	settingMap := map[string]*model.SettingItem{}
	for _, v := range settings {
		if !isActive(v.Key) && v.Flag != model.DEPRECATED {
			v.Flag = model.DEPRECATED
			err = op.SaveSettingItem(&v)
			if err != nil {
				utils.Log.Fatalf("failed save setting: %+v", err)
			}
		}
		settingMap[v.Key] = &v
	}
	// create or save setting
	save := false
	for i := range initialSettingItems {
		item := &initialSettingItems[i]
		item.Index = uint(i)
		if item.PreDefault == "" {
			item.PreDefault = item.Value
		}
		// err
		stored, ok := settingMap[item.Key]
		if !ok {
			stored, err = op.GetSettingItemByKey(item.Key)
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				utils.Log.Fatalf("failed get setting: %+v", err)
				continue
			}
		}
		if stored != nil && item.Key != conf.VERSION && stored.Value != item.PreDefault {
			item.Value = stored.Value
		}
		_, err = op.HandleSettingItemHook(item)
		if err != nil {
			utils.Log.Errorf("failed to execute hook on %s: %+v", item.Key, err)
			continue
		}
		// save
		if stored == nil || *item != *stored {
			save = true
		}
	}
	if save {
		err = db.SaveSettingItems(initialSettingItems)
		if err != nil {
			utils.Log.Fatalf("failed save setting: %+v", err)
		} else {
			op.SettingCacheUpdate()
		}
	}
}

// markFreshInstall flips the just-seeded conf.SetupCompleted row from its
// "true" default to "false", marking this run as a brand-new install that
// should see the first-run setup wizard. Must run AFTER initSettings() has
// created the row (initUser, which detects the fresh admin, runs before
// initSettings in InitData, so the flip happens as a separate follow-up step
// rather than by pre-seeding the item's Value). Existing/upgraded instances
// never call this, so their setup_completed row keeps its seeded "true".
func markFreshInstall() {
	item, err := op.GetSettingItemByKey(conf.SetupCompleted)
	if err != nil {
		utils.Log.Errorf("failed to load %s for fresh-install marking: %+v", conf.SetupCompleted, err)
		return
	}
	item.Value = "false"
	if err := op.SaveSettingItem(item); err != nil {
		utils.Log.Errorf("failed to mark fresh install (%s=false): %+v", conf.SetupCompleted, err)
	}
}

func isActive(key string) bool {
	for _, item := range initialSettingItems {
		if item.Key == key {
			return true
		}
	}
	return false
}

func InitialSettings() []model.SettingItem {
	var token string
	if flags.Dev {
		token = "dev_token"
	} else {
		token = random.Token()
	}
	defaultRoleID := strconv.Itoa(model.GUEST)
	initialSettingItems = []model.SettingItem{
		// site settings
		{Key: conf.VERSION, Value: conf.Version, Type: conf.TypeString, Group: model.SITE, Flag: model.READONLY},
		//{Key: conf.ApiUrl, Value: "", Type: conf.TypeString, Group: model.SITE},
		//{Key: conf.BasePath, Value: "", Type: conf.TypeString, Group: model.SITE},
		{Key: conf.SiteTitle, Value: "AList", Type: conf.TypeString, Group: model.SITE},
		{Key: conf.Announcement, Value: "### repo\nhttps://github.com/alist-org/alist", Type: conf.TypeText, Group: model.SITE},
		{Key: "pagination_type", Value: "all", Type: conf.TypeSelect, Options: "all,pagination,load_more,auto_load_more", Group: model.SITE},
		{Key: "default_page_size", Value: "50", Type: conf.TypeNumber, Group: model.SITE},
		{Key: conf.AllowIndexed, Value: "false", Type: conf.TypeBool, Group: model.SITE},
		{Key: conf.AllowMounted, Value: "true", Type: conf.TypeBool, Group: model.SITE},
		{Key: conf.RobotsTxt, Value: "User-agent: *\nAllow: /", Type: conf.TypeText, Group: model.SITE},
		{Key: conf.DefaultRole, Value: defaultRoleID, Type: conf.TypeSelect, Group: model.SITE},
		// Setup wizard completion flag. Seeded "true" (existing/upgraded
		// instances are never interrupted by the wizard). Bootstrap flips it to
		// "false" for the exact run that creates a brand-new admin user — see
		// markFreshInstall below — so only genuinely new installs see it.
		{Key: conf.SetupCompleted, Value: "true", Type: conf.TypeBool, Group: model.SITE, Flag: model.PRIVATE, Help: "Whether the first-run setup wizard has been completed."},
		// newui settings
		{Key: conf.UseNewui, Value: "false", Type: conf.TypeBool, Group: model.SITE},
		{Key: conf.FrontendRememberSort, Value: "false", Type: conf.TypeBool, Group: model.SITE, Help: "Persist frontend list sorting in the browser. When disabled, backend/driver order is used until the user sorts manually in the current session."},
		// Disabled built-in extensions (JSON string array of ids). PRIVATE: an
		// internal management flag driven by the built-in enable/disable endpoints,
		// never surfaced in the settings UI. Empty default: every built-in starts
		// enabled; ApplyDisabledBuiltins re-applies this set at boot so a disabled
		// built-in stays off across restarts.
		{Key: conf.DisabledBuiltinExtensions, Value: "[]", Type: conf.TypeText, Group: model.SITE, Flag: model.PRIVATE, Help: "JSON array of built-in extension ids turned off in the extension manager. Managed via the built-in enable/disable endpoints, not the settings UI."},
		// style settings
		{Key: conf.Logo, Value: "/images/logo.svg", Type: conf.TypeText, Group: model.STYLE},
		{Key: conf.Favicon, Value: "/images/logo.svg", Type: conf.TypeString, Group: model.STYLE},
		{Key: conf.MainColor, Value: "#1890ff", Type: conf.TypeString, Group: model.STYLE},
		{Key: "home_icon", Value: "🏠", Type: conf.TypeString, Group: model.STYLE},
		// Site-wide default theme descriptor (theming spec §J4): a full Theme
		// JSON blob applied as the baseline for every visitor, anonymous
		// included, before any per-browser localStorage choice. PUBLIC (no
		// explicit Flag — zero value) so it reaches /api/public/settings; an
		// admin's local theme choice made while editing still only affects
		// their own browser until they explicitly set it as the site default
		// (see AppearancePage's "set as site default" action). Empty means
		// "use the built-in default theme". Validated on save
		// (internal/op/site_theme.go): must parse as JSON and stay under a
		// size cap. A "js" field IS allowed (product decision, reversed
		// 2026-07) — but a value carrying one is REFUSED through this generic
		// bulk-save endpoint (server/handles/setting.go's SaveSettings): it
		// must go through the dedicated POST /api/v4/admin/settings/site-theme
		// endpoint instead, which requires RequireAdmin (not the delegable
		// setting:manage scope) AND an explicit js_confirmed:true in the
		// request body — see server/handles/site_theme.go.
		{Key: conf.SiteTheme, Value: "", Type: conf.TypeText, Group: model.STYLE, Help: "Site-wide default theme (JSON), applied for every visitor including anonymous ones. A visitor's own local theme choice overrides it. Empty uses the built-in default theme. Managed from the Appearance page. A theme with custom JS cannot be saved here — it runs with full page privileges for every visitor and requires the dedicated confirmation flow."},
		// Installed theme-marketplace themes (theming spec §J5). PRIVATE
		// (opus final-review, I1) — unlike site_theme above, this does NOT
		// ride the generic /api/public/settings blob; it's served anonymously
		// only through the dedicated, ETag-cacheable GET /api/v4/public/themes
		// (server/handles/theme_market.go's PublicMarketThemes). A stored
		// entry MAY carry custom JS ONLY from a verified source (I2 — an
		// untrusted source's js is refused at install) — it is still never
		// auto-enabled; each user has to pass the existing per-theme
		// js-trust gate before it runs.
		{Key: conf.MarketThemes, Value: "[]", Type: conf.TypeText, Group: model.STYLE, Flag: model.PRIVATE, Help: "Themes installed from the theme marketplace (JSON array). Anonymously readable only via GET /api/v4/public/themes (ETag-cacheable), not the general public settings. A theme carrying custom JS is stored but never auto-enabled. Managed via the theme marketplace admin API."},
		// home_container/settings_layout were v3 frontend layout knobs; the v4
		// frontend has no consumer for either (both the home page container and
		// the settings page layout are fixed by the current UI, not configurable
		// via these keys). Kept + marked DEPRECATED rather than deleted outright:
		// an upgraded instance may still have a stored non-default value, and
		// DeleteSettingItemByKey (internal/op/setting.go) refuses to delete a
		// setting that isn't already flagged deprecated, so this is the
		// documented path to eventually retiring the row.
		{Key: "home_container", Value: "max_980px", Type: conf.TypeSelect, Options: "max_980px,hope_container", Group: model.STYLE, Flag: model.DEPRECATED, Help: "Unused: no v4 frontend consumer. Kept for upgraded instances that stored a value; safe to ignore."},
		{Key: "settings_layout", Value: "list", Type: conf.TypeSelect, Options: "list,responsive", Group: model.STYLE, Flag: model.DEPRECATED, Help: "Unused: no v4 frontend consumer. Kept for upgraded instances that stored a value; safe to ignore."},
		// preview settings
		{Key: conf.TextTypes, Value: "txt,htm,html,xml,java,properties,sql,js,md,json,conf,ini,vue,php,py,bat,gitignore,yml,go,sh,c,cpp,h,hpp,tsx,vtt,srt,ass,rs,lrc", Type: conf.TypeText, Group: model.PREVIEW, Flag: model.PRIVATE},
		{Key: conf.AudioTypes, Value: "mp3,flac,ogg,m4a,wav,opus,wma", Type: conf.TypeText, Group: model.PREVIEW, Flag: model.PRIVATE},
		{Key: conf.VideoTypes, Value: "mp4,mkv,avi,mov,rmvb,webm,flv,m3u8", Type: conf.TypeText, Group: model.PREVIEW, Flag: model.PRIVATE},
		{Key: conf.ImageTypes, Value: "jpg,tiff,jpeg,png,gif,bmp,svg,ico,swf,webp", Type: conf.TypeText, Group: model.PREVIEW, Flag: model.PRIVATE},
		//{Key: conf.OfficeTypes, Value: "doc,docx,xls,xlsx,ppt,pptx", Type: conf.TypeText, Group: model.PREVIEW, Flag: model.PRIVATE},
		{Key: conf.ProxyTypes, Value: "m3u8,url", Type: conf.TypeText, Group: model.PREVIEW, Flag: model.PRIVATE},
		{Key: conf.ProxyIgnoreHeaders, Value: "authorization,referer", Type: conf.TypeText, Group: model.PREVIEW, Flag: model.PRIVATE},
		{Key: "external_previews", Value: `{}`, Type: conf.TypeText, Group: model.PREVIEW},
		{Key: "iframe_previews", Value: `{}`, Type: conf.TypeText, Group: model.PREVIEW},
		{Key: "preview_settings", Value: `{}`, Type: conf.TypeText, Group: model.PREVIEW},
		//		{Key: conf.OfficeViewers, Value: `{
		//	"Microsoft":"https://view.officeapps.live.com/op/view.aspx?src=$url",
		//	"Google":"https://docs.google.com/gview?url=$url&embedded=true",
		//}`, Type: conf.TypeText, Group: model.PREVIEW},
		//		{Key: conf.PdfViewers, Value: `{
		//	"pdf.js":"https://alist-org.github.io/pdf.js/web/viewer.html?file=$url"
		//}`, Type: conf.TypeText, Group: model.PREVIEW},
		{Key: "audio_cover", Value: "/images/logo.svg", Type: conf.TypeString, Group: model.PREVIEW},
		{Key: conf.AudioAutoplay, Value: "true", Type: conf.TypeBool, Group: model.PREVIEW},
		{Key: conf.VideoAutoplay, Value: "true", Type: conf.TypeBool, Group: model.PREVIEW},
		{Key: conf.ThumbnailSize, Value: "144", Type: conf.TypeNumber, Group: model.PREVIEW, Help: "Thumbnail width in pixels. Height is scaled proportionally."},
		{Key: conf.ReadMeAutoRender, Value: "true", Type: conf.TypeBool, Group: model.PREVIEW},
		{Key: conf.FilterReadMeScripts, Value: "true", Type: conf.TypeBool, Group: model.PREVIEW},
		{Key: conf.PreviewArchivesByDefault, Value: "true", Type: conf.TypeBool, Group: model.PREVIEW, Help: "Whether the built-in archive (zip) previewer is offered as the default (prior) candidate rather than requiring a manual switch."},
		// global settings
		{Key: conf.HideFiles, Value: "/\\/README.md/i", Type: conf.TypeText, Group: model.GLOBAL},
		{Key: "package_download", Value: "true", Type: conf.TypeBool, Group: model.GLOBAL},
		// PRODUCT DECISION (reversed 2026-07): script content here now genuinely
		// EXECUTES. An inline <script> is auto-stamped with the page's per-
		// response CSP nonce (server/static/sanitize.go), so it runs under the
		// existing nonce-gated script-src with no 'unsafe-inline' relaxation.
		// An external <script src="..."> only runs if its domain is listed in
		// ScriptSrcAllowlist below. on*="" inline event handlers and
		// javascript: URLs are STILL stripped unconditionally — see
		// sanitize.go's doc comment. Injected into the front-end document only
		// (server/static/static.go's noRoute handler swaps in conf.ManageHtml,
		// which has neither placeholder substituted, for any /@manage path) —
		// the admin panel is deliberately not affected by site-customization
		// content.
		{Key: conf.CustomizeHead, Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Raw HTML injected just before </head> on front-end pages only (never the admin panel). Scripts CAN execute: an inline <script> is auto-signed with the page's CSP nonce; an external <script src=\"...\"> needs its domain listed in \"Script source allowlist\" below. on*= event handlers and javascript: URLs are still stripped. Only paste content you trust — it runs with full page privileges for every visitor."},
		{Key: conf.CustomizeBody, Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Raw HTML injected just before </body> on front-end pages only (never the admin panel). Scripts CAN execute: an inline <script> is auto-signed with the page's CSP nonce; an external <script src=\"...\"> needs its domain listed in \"Script source allowlist\" below. on*= event handlers and javascript: URLs are still stripped. Only paste content you trust — it runs with full page privileges for every visitor."},
		// ScriptSrcAllowlist: which external script-src hosts CSP admits, so an
		// external <script src="..."> in CustomizeHead/CustomizeBody actually
		// loads. Comma or newline separated (e.g. "cdn.example.com,
		// https://analytics.example.org"). Empty by default — no third-party
		// script domain is trusted until explicitly added here.
		{Key: conf.ScriptSrcAllowlist, Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Domains allowed to load external <script src=\"...\"> from Customize Head/Body, comma or newline separated (e.g. cdn.example.com). This is a security-relevant allowlist: only add domains whose scripts you trust to run with full privileges on every visitor's page. Empty = no external script domain is trusted."},
		{Key: conf.LinkExpiration, Value: "0", Type: conf.TypeNumber, Group: model.GLOBAL, Flag: model.PRIVATE},
		{Key: conf.SignAll, Value: "true", Type: conf.TypeBool, Group: model.GLOBAL, Flag: model.PRIVATE},
		{Key: conf.PrivacyRegs, Value: `(?:(?:\d|[1-9]\d|1\d\d|2[0-4]\d|25[0-5])\.){3}(?:\d|[1-9]\d|1\d\d|2[0-4]\d|25[0-5])
([[:xdigit:]]{1,4}(?::[[:xdigit:]]{1,4}){7}|::|:(?::[[:xdigit:]]{1,4}){1,6}|[[:xdigit:]]{1,4}:(?::[[:xdigit:]]{1,4}){1,5}|(?:[[:xdigit:]]{1,4}:){2}(?::[[:xdigit:]]{1,4}){1,4}|(?:[[:xdigit:]]{1,4}:){3}(?::[[:xdigit:]]{1,4}){1,3}|(?:[[:xdigit:]]{1,4}:){4}(?::[[:xdigit:]]{1,4}){1,2}|(?:[[:xdigit:]]{1,4}:){5}:[[:xdigit:]]{1,4}|(?:[[:xdigit:]]{1,4}:){1,6}:)
(?U)access_token=(.*)&`,
			Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE},
		{Key: conf.FilenameCharMapping, Value: `{"/": "|"}`, Type: conf.TypeText, Group: model.GLOBAL},
		{Key: conf.ForwardDirectLinkParams, Value: "false", Type: conf.TypeBool, Group: model.GLOBAL},
		{Key: conf.IgnoreDirectLinkParams, Value: "sign,alist_ts", Type: conf.TypeString, Group: model.GLOBAL},
		{Key: conf.MaxDevices, Value: "0", Type: conf.TypeNumber, Group: model.GLOBAL},
		{Key: conf.DeviceEvictPolicy, Value: "deny", Type: conf.TypeSelect, Options: "deny,evict_oldest", Group: model.GLOBAL},
		{Key: conf.DeviceSessionTTL, Value: "86400", Type: conf.TypeNumber, Group: model.GLOBAL},
		// Plugin index URL (Step 9.3). Empty by default: the privacy baseline
		// (Step 2a) forbids default outbound calls, so the official plugin index
		// is fetched only when an admin sets this and explicitly requests it.
		{Key: conf.PluginIndexURL, Value: "", Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Official plugin registry index URL. Empty disables plugin browsing; fetched only on explicit admin request."},
		// Plugin trust policy (Step 11). Defaults to the most restrictive floor:
		// only first-party (official) signed plugins may install/load. Widening it
		// permits community-verified (verified) or third-party (any) plugins.
		{Key: conf.PluginPolicy, Value: "official-only", Type: conf.TypeSelect, Options: "official-only,verified,any", Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Trust floor for installing/loading plugins. official-only (default): only first-party signed; verified: also packages signed by an imported plugin source; any: also unsigned third-party (installs only after an explicit, non-skippable risk confirmation). Enforced at install and load."},
		{Key: conf.PluginSources, Value: "[]", Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Admin-imported plugin sources: JSON array of {name, index_url, pubkey (ed25519 hex)}. Packages signed by an imported source's key install as tier-verified with a source-attribution warning. Manage via the plugin sources admin API."},
		// Theme marketplace sources (theming spec §J5). Mirrors plugin_sources'
		// shape/management pattern; pubkey is optional (a source without one has
		// no signing capability — its themes install "untrusted", not blocked).
		{Key: conf.ThemeSources, Value: "[]", Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Admin-imported theme marketplace sources: JSON array of {name, index_url, pubkey? (ed25519 hex)}. A source without pubkey has no signing capability; its themes install as untrusted. Manage via the theme marketplace admin API."},
		// Federated (SSO/OIDC) auto-provision policy gate. Defaults false (most
		// restrictive): a federated login method may prove an external identity,
		// but the host refuses to create a new AList user for it — an unknown
		// identity gets no session. Enable to let federated logins auto-create a
		// user with the default role; the host never provisions an admin. Primary
		// (LDAP) methods never auto-provision regardless of this flag.
		{Key: conf.AuthNAutoProvision, Value: "false", Type: conf.TypeBool, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Allow federated (SSO/OIDC) login methods to auto-create an AList user for an unknown external identity. Default off: unknown federated identities are refused. Auto-provisioned users get the default role, never admin."},
		// Configured OIDC SSO providers (JSON array). PRIVATE: it stores client
		// secrets and is never in the public settings map. Empty default: no SSO
		// until an admin adds a provider via /api/v4/admin/sso/providers.
		{Key: conf.SSOProviders, Value: "[]", Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "JSON array of configured OIDC SSO providers. Managed via the SSO admin API; client secrets are write-only and never returned."},
		// WebAuthn (passkey) Relying Party config. All empty by default and derived
		// from site_url / the request origin at ceremony time; set explicitly only
		// when the derived defaults are wrong (reverse proxy, multiple origins).
		{Key: conf.WebAuthnRPID, Value: "", Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "WebAuthn/passkey Relying Party ID: the registrable domain (host only, no scheme or port), e.g. example.com. Empty derives it from site_url/the request host. Must be a registrable suffix of every allowed origin; changing it invalidates all existing passkeys."},
		{Key: conf.WebAuthnRPDisplayName, Value: "", Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "WebAuthn/passkey Relying Party display name shown by some authenticators. Empty falls back to site_title."},
		{Key: conf.WebAuthnRPOrigins, Value: "", Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "WebAuthn/passkey allowed origins (scheme+host+port), comma/newline separated, e.g. https://example.com. Empty derives a single origin from site_url/the request."},

		// single settings
		{Key: conf.Token, Value: token, Type: conf.TypeString, Group: model.SINGLE, Flag: model.PRIVATE},
		// TokenPlaintext defaults true: the seeded token above is plaintext,
		// matching pre-existing installs. Rotating via ResetToken/SetToken
		// without explicitly opting into reveal switches this to false and
		// stores a salted hash instead (see pkg/utils.HashToken).
		{Key: conf.TokenPlaintext, Value: "true", Type: conf.TypeBool, Group: model.SINGLE, Flag: model.PRIVATE},
		// index_progress is the persisted build-progress snapshot (ext.IndexProgress
		// JSON, 子项目 A). SINGLE + PRIVATE: internal state the build path writes and
		// the frontend polls, never an admin-editable setting.
		{Key: conf.IndexProgress, Value: "{}", Type: conf.TypeText, Group: model.SINGLE, Flag: model.PRIVATE},

		// traffic settings
		{Key: conf.TaskUploadThreadsNum, Value: strconv.Itoa(conf.Conf.Tasks.Upload.Workers), Type: conf.TypeNumber, Group: model.TRAFFIC, Flag: model.PRIVATE},
		{Key: conf.TaskCopyThreadsNum, Value: strconv.Itoa(conf.Conf.Tasks.Copy.Workers), Type: conf.TypeNumber, Group: model.TRAFFIC, Flag: model.PRIVATE},
		{Key: conf.TaskOfflineDownloadThreadsNum, Value: strconv.Itoa(conf.Conf.Tasks.Download.Workers), Type: conf.TypeNumber, Group: model.OFFLINE_DOWNLOAD, Help: "Number of concurrent offline-download workers. Higher pulls more URLs at once at the cost of bandwidth/IO."},
		{Key: conf.StreamMaxClientDownloadSpeed, Value: "-1", Type: conf.TypeNumber, Group: model.TRAFFIC, Flag: model.PRIVATE},
		{Key: conf.StreamMaxClientUploadSpeed, Value: "-1", Type: conf.TypeNumber, Group: model.TRAFFIC, Flag: model.PRIVATE},
		{Key: conf.StreamMaxServerDownloadSpeed, Value: "-1", Type: conf.TypeNumber, Group: model.TRAFFIC, Flag: model.PRIVATE},
		{Key: conf.StreamMaxServerUploadSpeed, Value: "-1", Type: conf.TypeNumber, Group: model.TRAFFIC, Flag: model.PRIVATE},
		// Per-connection download cap (KiB/s): each download is limited on its
		// own, independent of the shared max_client_download_speed bucket.
		// Default -1 (unlimited) so it never throttles unless an admin opts in.
		{Key: conf.StreamMaxConnectionDownloadSpeed, Value: "-1", Type: conf.TypeNumber, Group: model.TRAFFIC, Flag: model.PRIVATE, Help: "Bandwidth cap for each individual download connection, in KiB/s. -1 means unlimited. Unlike max_client_download_speed (a single bucket shared across all connections), this limits every connection on its own."},
		// Brute-force lockout thresholds, shared by every unauthenticated
		// credential endpoint (password login, login-method login, token
		// refresh). Lenient defaults: 5 failed attempts per IP trip a 300s
		// lockout, so a user mistyping a password is never affected.
		{Key: conf.LoginMaxAttempts, Value: "5", Type: conf.TypeNumber, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "Failed sign-in attempts allowed per client IP before it is temporarily locked out. Applies to password login, login-method login and token refresh. Non-positive falls back to the built-in default (5)."},
		{Key: conf.LoginLockoutDuration, Value: "300", Type: conf.TypeNumber, Group: model.GLOBAL, Flag: model.PRIVATE, Help: "How long (seconds) an IP stays locked out after hitting login_max_attempts, measured from its last attempt. Non-positive falls back to the built-in default (300)."},

		// index settings (file search MVP tuning — W6). Read at request time by
		// server/handles/search.go via setting.GetInt; a blank/non-positive value
		// falls back to the search package's own defaults (20 / 1000), so these
		// only tighten or loosen the bound rather than replace the safety net.
		{Key: conf.SearchMaxDepth, Value: "20", Type: conf.TypeNumber, Group: model.INDEX, Help: "How many directory levels below the search root the file search descends. 0 or blank uses the built-in default (20)."},
		{Key: conf.SearchResultLimit, Value: "1000", Type: conf.TypeNumber, Group: model.INDEX, Help: "Maximum number of results a single file search returns. 0 or blank uses the built-in default (1000)."},

		// real-index backend selection (子项目 A). search_index picks the active
		// SearchIndexer ("none" keeps the index-less BFS fallback); a setting hook
		// (search.Init) re-resolves the backend on change. ignore_paths and
		// max_index_depth tune the build and are PRIVATE (internal build knobs).
		{Key: conf.SearchIndex, Value: "none", Type: conf.TypeSelect, Options: "none,database_non_full_text,bleve,meilisearch", Group: model.INDEX, Help: "Which search index backend to use. 'none' uses the index-less real-time search. Others require the matching backend extension to be enabled and an index to be built."},
		{Key: conf.AutoUpdateIndex, Value: "false", Type: conf.TypeBool, Group: model.INDEX, Help: "Incrementally update the search index as files change. Requires an active index backend."},
		{Key: conf.IgnorePaths, Value: "", Type: conf.TypeText, Group: model.INDEX, Flag: model.PRIVATE, Help: "Virtual paths (one per line) the index build skips."},
		{Key: conf.MaxIndexDepth, Value: "20", Type: conf.TypeNumber, Group: model.INDEX, Flag: model.PRIVATE, Help: "How many directory levels below each mount root the index build descends."},

		// search-backend connection config (子项目 C, wave C4): migrated from the
		// config-file (conf.BleveDir / conf.Meilisearch) to DB settings so the search
		// feature's bleve/meilisearch config sections are admin-editable. Read at
		// open/connect time by the backends, so a change takes effect on the next
		// open/reconnect. bleve_index_path blank = use the config-file default
		// (<data>/bleve); meilisearch_api_key is a secret (PRIVATE), host/prefix VISIBLE.
		{Key: conf.BleveIndexPath, Value: "", Type: conf.TypeString, Group: model.INDEX, Help: "Directory the bleve full-text index is stored under. Blank uses the built-in default (<data>/bleve). Takes effect the next time the index is opened."},
		{Key: conf.MeilisearchHost, Value: "http://localhost:7700", Type: conf.TypeString, Group: model.INDEX, Help: "Base URL of the external Meilisearch service the meilisearch backend connects to."},
		{Key: conf.MeilisearchAPIKey, Value: "", Type: conf.TypeString, Group: model.INDEX, Flag: model.PRIVATE, Help: "API key for the Meilisearch service. Kept private."},
		{Key: conf.MeilisearchIndexPrefix, Value: "", Type: conf.TypeString, Group: model.INDEX, Help: "Prefix that namespaces this instance's Meilisearch indexes on a shared server."},

		// offline_download settings (SimpleHttp tool tuning — W6). The size cap is
		// read at task-run time by internal/offline_download/task.go; the worker
		// count (above, offline_download_task_threads_num) is grouped here too.
		{Key: conf.OfflineDownloadMaxSizeGB, Value: "100", Type: conf.TypeNumber, Group: model.OFFLINE_DOWNLOAD, Help: "Per-download size backstop in GiB for the SimpleHttp offline downloader. A download exceeding this fails rather than truncating. 0 or blank uses the built-in default (100 GiB)."},
		// offline_download_tool picks the active daemon tool (子项目 B). "SimpleHttp"
		// keeps the built-in direct-download path; the others require the matching
		// tool extension to be enabled. A setting hook (offline_download.Init)
		// re-resolves the tool on change.
		{Key: conf.OfflineDownloadTool, Value: "SimpleHttp", Type: conf.TypeSelect, Options: "SimpleHttp,aria2,qBittorrent,Transmission", Group: model.OFFLINE_DOWNLOAD, Help: "Which offline-download tool to use. 'SimpleHttp' uses the built-in direct-download path. Others require the matching tool extension to be enabled and configured."},
		// offline_download_transfer_threads_num sets the transfer engine's worker
		// count (子项目 B, wave B2): once a daemon tool finishes downloading into the
		// local temp area, this many TransferTasks move the finished files into the
		// destination storage in parallel. Independent of the download worker count.
		{Key: conf.OfflineDownloadTransferThreadsNum, Value: "5", Type: conf.TypeNumber, Group: model.OFFLINE_DOWNLOAD, Help: "Number of concurrent transfer workers that move finished daemon-tool downloads from the local temp area into the destination storage."},
		// aria2 tool config (子项目 B, wave B3): the JSON-RPC endpoint (visible, so
		// an admin can point it at their daemon) and the rpc secret token (a
		// credential, so PRIVATE and kept out of logs). Read by the
		// builtin.offline_download.aria2 tool extension.
		{Key: conf.Aria2Uri, Value: "http://localhost:6800/jsonrpc", Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD, Help: "aria2 JSON-RPC endpoint the aria2 offline-download tool talks to, e.g. http://localhost:6800/jsonrpc."},
		{Key: conf.Aria2Secret, Value: "", Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD, Flag: model.PRIVATE, Help: "aria2 rpc secret token (--rpc-secret). Leave blank if the daemon has no secret configured."},
		// qBittorrent tool config (子项目 B, wave B4): the WebUI base URL (may embed
		// admin credentials in its userinfo, so PRIVATE and kept out of logs) and the
		// seeding time limit in minutes (visible number; 0 = no limit, applied
		// best-effort at add time under the transfer-then-remove lifecycle). Read by
		// the builtin.offline_download.qbittorrent tool extension.
		{Key: conf.QbittorrentUrl, Value: "http://admin:adminadmin@localhost:8080/", Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD, Flag: model.PRIVATE, Help: "qBittorrent WebUI URL, credentials embedded in the userinfo, e.g. http://admin:adminadmin@localhost:8080/. Stored privately and kept out of logs."},
		{Key: conf.QbittorrentSeedtime, Value: "0", Type: conf.TypeNumber, Group: model.OFFLINE_DOWNLOAD, Help: "qBittorrent seeding time limit in minutes (0 = no limit). Applied best-effort at add time; full post-download seeding is not enforced under the transfer-then-remove lifecycle."},
		// Transmission tool config (子项目 B, wave B5): the RPC endpoint (visible, so an
		// admin can point it at their daemon; the localhost default embeds no
		// credentials) and the seeding time limit in minutes (visible number; 0 = no
		// limit). Under the transfer-then-remove lifecycle the seed limit is NOT
		// enforced — the minimal RPC seam exposes no seed-limit mutator — so it is kept
		// for parity/future use. Read by the builtin.offline_download.transmission tool
		// extension.
		{Key: conf.TransmissionUri, Value: "http://localhost:9091/transmission/rpc", Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD, Help: "Transmission RPC endpoint the Transmission offline-download tool talks to, e.g. http://localhost:9091/transmission/rpc."},
		{Key: conf.TransmissionSeedtime, Value: "0", Type: conf.TypeNumber, Group: model.OFFLINE_DOWNLOAD, Help: "Transmission seeding time limit in minutes (0 = no limit). Not enforced under the transfer-then-remove lifecycle; retained for parity."},

		// frp internal tunnel (design §2.1): alist supervises an external frpc
		// process that maps its local HTTP port to a public frps remote port.
		// Default disabled so an instance is never exposed unless an admin opts
		// in; the token is PRIVATE (never returned by the public settings API).
		{Key: conf.FrpEnabled, Value: "false", Type: conf.TypeBool, Group: model.FRP, Help: "Enable the frp tunnel. When on, alist starts a supervised frpc process that exposes this instance to the public through your frps server. Leave off unless you understand the exposure risk."},
		{Key: conf.FrpServerAddr, Value: "", Type: conf.TypeString, Group: model.FRP, Help: "frps server address (domain or IP)."},
		{Key: conf.FrpServerPort, Value: "7000", Type: conf.TypeNumber, Group: model.FRP, Help: "frps server bind port."},
		{Key: conf.FrpToken, Value: "", Type: conf.TypeString, Group: model.FRP, Flag: model.PRIVATE, Help: "frps authentication token. Stored privately and never returned by the settings API."},
		{Key: conf.FrpRemotePort, Value: "", Type: conf.TypeNumber, Group: model.FRP, Help: "Remote port on the frps server that this instance is exposed on (tcp protocol)."},
		{Key: conf.FrpProtocol, Value: "tcp", Type: conf.TypeSelect, Options: "tcp,http", Group: model.FRP, Help: "Proxy protocol. tcp maps a remote port; http uses the frps vhost."},
		{Key: conf.FrpBinaryPath, Value: "frpc", Type: conf.TypeString, Group: model.FRP, Help: "Path to the frpc executable (found on PATH or an absolute path). You must supply frpc yourself."},
	}
	if flags.Dev {
		initialSettingItems = append(initialSettingItems, []model.SettingItem{
			{Key: "test_deprecated", Value: "test_value", Type: conf.TypeString, Flag: model.DEPRECATED},
			{Key: "test_options", Value: "a", Type: conf.TypeSelect, Options: "a,b,c"},
			{Key: "test_help", Type: conf.TypeString, Help: "this is a help message"},
		}...)
	}
	return initialSettingItems
}
