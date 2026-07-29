package conf

const (
	TypeString = "string"
	TypeSelect = "select"
	TypeBool   = "bool"
	TypeText   = "text"
	TypeNumber = "number"
)

const (
	// site
	VERSION              = "version"
	SiteTitle            = "site_title"
	Announcement         = "announcement"
	AllowIndexed         = "allow_indexed"
	AllowMounted         = "allow_mounted"
	RobotsTxt            = "robots_txt"
	DefaultRole          = "default_role"
	UseNewui             = "use_newui"
	FrontendRememberSort = "frontend_remember_sort"
	// SetupCompleted marks whether the first-run setup wizard has been
	// completed. Seeded "true" for every install by default so upgrades of
	// existing instances are never interrupted; only the exact moment
	// bootstrap creates a brand-new admin user (internal/bootstrap/data/user.go
	// initUser) flips the freshly-seeded row to "false", marking a genuine
	// new install that should see the wizard. Flipped back to "true" by the
	// authenticated POST /admin/settings/setup/complete handler once the
	// admin finishes (or explicitly skips through) the wizard. This never
	// gates authentication itself — it only drives a post-login frontend
	// redirect — so no unauthenticated setup endpoint is introduced.
	SetupCompleted = "setup_completed"

	Logo      = "logo"
	Favicon   = "favicon"
	MainColor = "main_color"
	// SiteTheme is the site-wide default theme descriptor: a full frontend
	// Theme object (tokens/radius/font/css/branding/notification, optional
	// js, JSON — mirrors alist-v4-web's src/theme/types.ts Theme) applied as
	// the baseline for every visitor, including anonymous ones. PUBLIC (see
	// internal/bootstrap/data/setting.go) so it reaches unauthenticated
	// clients through /api/public/settings — a user's own local theme choice
	// (browser localStorage) still overrides it. Empty means "use the
	// built-in default theme". Validated on save (internal/op/site_theme.go):
	// must parse as JSON and stay under a size cap.
	//
	// A "js" field IS allowed (product decision, reversed 2026-07 — it used
	// to be refused outright here). It runs with full page privileges for
	// EVERY visitor, anonymous included, once set — there is no per-visitor
	// consent step to gate on the way a personal theme's js-trust checkbox
	// does; each visitor's own browser auto-grants itself trust for this
	// exact js at load time (siteTheme.ts's loadSiteThemeFromPublicSettings),
	// reusing the very same per-browser jsTrust gate a personal theme's
	// checkbox flow feeds. Because of that, the confirmation step CANNOT be a
	// frontend-only UI convention (red-team finding, HIGH: this repo's RBAC
	// supports a role scoped to ONLY setting:manage, not full admin — such a
	// role could otherwise POST straight to the generic bulk-settings endpoint
	// and run arbitrary JS for every visitor with zero review). The real gate
	// is enforced server-side, in TWO layers:
	//   1. validateSiteTheme (internal/op/site_theme.go) itself no longer
	//      rejects js — but server/handles/setting.go's SaveSettings (the
	//      GENERIC bulk /admin/settings save, RequireSystem("setting",
	//      "manage")) refuses outright any site_theme value carrying a
	//      non-blank js field, full stop, regardless of caller intent.
	//   2. The ONLY path that can actually save a js-carrying site_theme is
	//      the dedicated POST /api/v4/admin/settings/site-theme endpoint
	//      (server/handles/site_theme.go), gated on middlewares.RequireAdmin
	//      (the non-delegable authorization control, not the scoped
	//      setting:manage permission) AND an explicit `js_confirmed: true` in
	//      the request body — missing/false is a 400 with a readable reason,
	//      so even a direct API call (bypassing alist-v4-web's SiteThemeCard
	//      review dialog entirely) must affirmatively assert "I confirm this".
	//      That endpoint also writes an audit event recording who did it.
	SiteTheme = "site_theme"

	// preview
	TextTypes           = "text_types"
	AudioTypes          = "audio_types"
	VideoTypes          = "video_types"
	ImageTypes          = "image_types"
	ProxyTypes          = "proxy_types"
	ProxyIgnoreHeaders  = "proxy_ignore_headers"
	AudioAutoplay       = "audio_autoplay"
	VideoAutoplay       = "video_autoplay"
	ThumbnailSize       = "thumbnail_size"
	ReadMeAutoRender    = "readme_autorender"
	FilterReadMeScripts = "filter_readme_scripts"
	// PreviewArchivesByDefault: whether the built-in archive (zip) previewer
	// is `prior` (the default candidate shown first) rather than requiring the
	// visitor to switch to it manually. v3 parity key — was missing from v4
	// entirely, so the frontend had this hard-coded to always-prior. Newly seeded
	// here; an instance migrated from v3 BEFORE this key existed gets it seeded
	// with its default on the very next boot too (internal/bootstrap/data/setting.go's
	// initSettings() inserts any InitialSettings() key missing from the DB,
	// independent of the one-time migratev4 pass), so no separate migration
	// patch is needed for already-migrated instances.
	PreviewArchivesByDefault = "preview_archives_by_default"
	// global
	HideFiles = "hide_files"
	// CustomizeHead/CustomizeBody are raw HTML spliced into every visitor's
	// index.html (server/static/static.go), just before </head> and </body>
	// respectively. PRODUCT DECISION (reversed 2026-07): script content here
	// now genuinely EXECUTES — server/static/sanitize.go no longer strips
	// <script> tags wholesale. An inline <script> is auto-stamped with the
	// page's per-response CSP nonce (so it runs under the existing
	// nonce-gated script-src with no 'unsafe-inline' relaxation needed); an
	// external <script src="..."> only runs if its origin is listed in
	// ScriptSrcAllowlist below (CSP script-src is built dynamically to
	// include it). on*= inline event-handler attributes and javascript: URLs
	// are STILL stripped unconditionally — neither can meaningfully carry a
	// nonce, and neither has a legitimate CSS/meta/script use, so there is no
	// tradeoff in continuing to remove them.
	CustomizeHead = "customize_head"
	CustomizeBody = "customize_body"
	// ScriptSrcAllowlist is a comma/newline-separated list of domains admitted
	// into the CSP script-src directive (server/static/csp.go), letting an
	// external <script src="..."> pasted into CustomizeHead/CustomizeBody
	// actually load. Empty by default (no third-party script domain is
	// trusted until an admin explicitly adds one) — PRIVATE (this only feeds a
	// server-generated response header; no anonymous page load needs to read
	// the raw setting value itself).
	ScriptSrcAllowlist      = "script_src_allowlist"
	LinkExpiration          = "link_expiration"
	SignAll                 = "sign_all"
	PrivacyRegs             = "privacy_regs"
	FilenameCharMapping     = "filename_char_mapping"
	ForwardDirectLinkParams = "forward_direct_link_params"
	IgnoreDirectLinkParams  = "ignore_direct_link_params"
	MaxDevices              = "max_devices"
	DeviceEvictPolicy       = "device_evict_policy"
	DeviceSessionTTL        = "device_session_ttl"
	// PluginIndexURL is the official plugin registry index URL (Step 9.3).
	// Defaults EMPTY: the privacy baseline (Step 2a) forbids default outbound
	// calls, so the index is fetched only on explicit admin request.
	PluginIndexURL = "plugin_index_url"
	// PluginPolicy is the admin trust floor for installing/loading plugins:
	// official-only (default, most restrictive) | verified (adds imported-
	// source-signed) | any (adds unsigned third-party, behind the mandatory
	// risk confirmation). Enforced at BOTH install and load time.
	PluginPolicy = "plugin_policy"
	// PluginSources is a JSON array of admin-imported plugin sources
	// ({name, index_url, pubkey}). A package signed by an imported source's
	// key is tier-verified (🟡). Managed via /api/v4/admin/plugins/sources.
	PluginSources = "plugin_sources"
	// ThemeSources is a JSON array of admin-imported theme-marketplace
	// sources ({name, index_url, pubkey?} — see internal/thememarket.Source).
	// Unlike PluginSources, pubkey is OPTIONAL: a source with none has no
	// signing capability, so every theme it serves installs at
	// thememarket.TierUntrusted (still installable — themes carry no code
	// execution risk beyond the same per-theme js-trust gate every other
	// theme source already goes through). Managed via
	// /api/v4/admin/theme-market/sources. PRIVATE — same posture as
	// PluginSources (source list is an admin management concern, not
	// something an anonymous visitor's page load needs).
	ThemeSources = "theme_sources"
	// MarketThemes is a JSON array of themes installed from the theme
	// marketplace (internal/thememarket.InstalledTheme), each carrying the
	// raw theme descriptor JSON. An admin-installed marketplace theme must
	// still be selectable by every visitor's picker, not just the admin's own
	// browser — but UNLIKE SiteTheme, this is flagged PRIVATE (opus
	// final-review, I1): it does NOT ride the generic /api/public/settings
	// blob (fetched on every anonymous page load, no conditional-caching
	// support). Instead it is served ONLY through the dedicated,
	// ETag-cacheable GET /api/v4/public/themes (server/handles/
	// theme_market.go's PublicMarketThemes) — anonymous visibility is
	// unchanged, just moved to a route that can actually be cached cheaply.
	// A theme carrying custom JS IS allowed here, but ONLY from a verified
	// (signed) source — an untrusted source's js is refused at install time (I2, see
	// server/handles/theme_market.go's InstallThemeMarketTheme) — and even a
	// verified theme's js is never auto-enabled; running it still requires
	// each individual user to explicitly trust it through the existing
	// frontend js-trust gate (spec §D5), exactly like an imported user theme
	// (see src/features/appearance/marketThemes.ts's loader). Managed via
	// /api/v4/admin/theme-market/install and .../installed/{source}/{entryId}.
	MarketThemes = "market_installed_themes"
	// DisabledBuiltinExtensions is a JSON string array of the ids of built-in
	// extensions (labels/SSO/passkey feature bundles, the FRP background service,
	// …) an admin has turned OFF in the extension manager. Built-ins register
	// enabled at boot; ApplyDisabledBuiltins re-applies this set after
	// registration so a disabled built-in stays off across restarts (its
	// contributions are not aggregated and its BackgroundService is not started).
	// PRIVATE: an internal management flag, never exposed in the public settings
	// map. Managed via the built-in enable/disable endpoints, not the settings UI.
	DisabledBuiltinExtensions = "disabled_builtin_extensions"
	// AuthNAutoProvision is the policy gate for federated (SSO/OIDC) login
	// methods that assert an identity with no matching AList user. Defaults
	// "false" (most restrictive): a federated method may NOT create a user, so
	// an unknown identity is refused a session. When "true", a federated login
	// for an unknown user auto-creates one with the configured default role —
	// never an admin (the host caps it, see server/handles/authn.go). Primary
	// (LDAP) methods never auto-provision regardless of this setting: they log
	// in only pre-existing users.
	AuthNAutoProvision = "authn_auto_provision"
	// SSOProviders is a JSON array of configured OIDC providers (ProviderConfig:
	// {key, name, preset, issuer, client_id, client_secret, scopes, claim map,
	// enabled}). It is PRIVATE — it holds client secrets — and never exposed
	// through the public settings map. Each enabled provider is registered as a
	// trusted "sso:<key>" federated AuthNMethod. Managed via /api/v4/admin/sso/
	// providers; the admin API never echoes client_secret back.
	SSOProviders = "sso_providers"

	// WebAuthn (passkey) Relying Party configuration. All three default EMPTY and
	// are derived at runtime from conf.SiteURL (and, at ceremony time, the request
	// origin) when unset — see internal/authn/passkey. An operator sets them
	// explicitly only when the derived defaults are wrong (reverse proxy with a
	// different public host, multiple origins, a custom display name).
	//
	// WebAuthnRPID is the Relying Party ID: the effective registrable domain
	// (host only, no scheme or port), e.g. "example.com". It must be a registrable
	// suffix of every allowed origin, and a credential is permanently bound to it,
	// so changing it invalidates every existing passkey.
	WebAuthnRPID = "webauthn_rp_id"
	// WebAuthnRPDisplayName is the human-readable site name some authenticators
	// show during a ceremony. Falls back to site_title when empty.
	WebAuthnRPDisplayName = "webauthn_rp_display_name"
	// WebAuthnRPOrigins is the comma/newline-separated list of fully-qualified
	// origins (scheme + host + port) permitted to run passkey ceremonies, e.g.
	// "https://example.com". Falls back to the origin derived from SiteURL/request.
	WebAuthnRPOrigins = "webauthn_rp_origins"

	// LoginMaxAttempts is the per-IP failed-attempt threshold that trips the
	// brute-force lockout shared by every unauthenticated credential endpoint
	// (password login, AuthNMethod login, token refresh). Defaults to a lenient
	// 5 so a user mistyping a password a few times is never locked out; an admin
	// can tighten it. A non-positive value falls back to the built-in default.
	LoginMaxAttempts = "login_max_attempts"
	// LoginLockoutDuration is how long (in seconds) an IP that hit
	// LoginMaxAttempts stays locked out, measured from its last attempt.
	// Defaults to 300s. A non-positive value falls back to the built-in default.
	LoginLockoutDuration = "login_lockout_duration"

	// single
	// Token is the admin bearer credential ONLY. Since v4 Step 6 it no longer
	// seeds any download/share/archive signature (those use signing_keys); an
	// admin token rotation does not invalidate outstanding signed links.
	Token = "token"
	// TokenPlaintext marks how conf.Token is stored: "true" means the Value
	// is the token in plaintext (readable back via GetToken); "false" means
	// Value is a salted sha256 digest (pkg/utils.HashToken) and cannot be
	// reversed — GetToken then withholds it. Defaults to "true" so upgrades
	// keep working with a pre-existing plaintext token; new tokens minted via
	// ResetToken/SetToken default to hashed (reveal=false) unless the caller
	// opts into plaintext.
	TokenPlaintext = "token_plaintext"

	// traffic
	TaskUploadThreadsNum          = "upload_task_threads_num"
	TaskCopyThreadsNum            = "copy_task_threads_num"
	TaskOfflineDownloadThreadsNum = "offline_download_task_threads_num"
	StreamMaxClientDownloadSpeed  = "max_client_download_speed"
	StreamMaxClientUploadSpeed    = "max_client_upload_speed"
	StreamMaxServerDownloadSpeed  = "max_server_download_speed"
	StreamMaxServerUploadSpeed    = "max_server_upload_speed"
	// StreamMaxConnectionDownloadSpeed caps the bandwidth of EACH download
	// connection independently (KiB/s), unlike StreamMaxClientDownloadSpeed whose
	// single bucket is shared across all connections. Defaults to -1 (unlimited)
	// so it never throttles unless an admin opts in.
	StreamMaxConnectionDownloadSpeed = "max_connection_download_speed"

	// index (file search MVP tuning; read by server/handles/search.go — W6)
	// SearchMaxDepth bounds how many directory levels below the search root the
	// index-less BFS walk descends. A non-positive value falls back to the search
	// package's own default (20).
	SearchMaxDepth = "search_max_depth"
	// SearchResultLimit caps how many hits a single search returns. A
	// non-positive value falls back to the search package's own default (1000).
	SearchResultLimit = "search_result_limit"
	// SearchIndex selects the active real-index backend (子项目 A): a Select over
	// "none" plus the ids of the enabled SearchIndexer backends
	// (database_non_full_text/bleve/meilisearch). "none" (default) keeps the
	// index-less BFS fallback. Changing it re-inits the search base
	// (search.Init via a setting hook) to resolve the newly-selected backend.
	SearchIndex = "search_index"
	// AutoUpdateIndex gates incremental index maintenance on filesystem changes
	// (the later auto-update wave). Default "false".
	AutoUpdateIndex = "auto_update_index"
	// IgnorePaths is a newline/comma-separated list of virtual paths the index
	// build skips (alongside a storage's DisableIndex flag). PRIVATE.
	IgnorePaths = "ignore_paths"
	// MaxIndexDepth bounds how many directory levels below each mount root the
	// index build descends. PRIVATE.
	MaxIndexDepth = "max_index_depth"
	// IndexProgress is the persisted build-progress snapshot (ext.IndexProgress
	// JSON) the frontend polls. SINGLE + PRIVATE: internal state, not an
	// admin-editable setting.
	IndexProgress = "index_progress"
	// Search-backend connection config, migrated from the config-file to DB
	// settings (子项目 C, wave C4) so the search feature's bleve/meilisearch config
	// sections are admin-editable in the UI. The config-file fields
	// (conf.Conf.BleveDir / conf.Conf.Meilisearch) remain as back-compat defaults,
	// but these settings are authoritative and read at open/connect time.
	//
	// BleveIndexPath is the on-disk directory the bleve backend stores its index
	// under. EMPTY (default) means "use the config-file default", conf.Conf.BleveDir
	// (<data>/bleve).
	BleveIndexPath = "bleve_index_path"
	// MeilisearchHost is the external Meilisearch service base URL (VISIBLE).
	MeilisearchHost = "meilisearch_host"
	// MeilisearchAPIKey is the Meilisearch API key — a credential, so PRIVATE and
	// kept out of the public settings map.
	MeilisearchAPIKey = "meilisearch_api_key"
	// MeilisearchIndexPrefix namespaces this instance's indexes on a shared
	// Meilisearch server (the index uid is "<prefix>alist").
	MeilisearchIndexPrefix = "meilisearch_index_prefix"

	// offline_download (SimpleHttp tool tuning; read by KEY — W6)
	// OfflineDownloadMaxSizeGB is the per-download size backstop, in GiB, applied
	// to the SimpleHttp fallback stream. A non-positive value falls back to the
	// built-in default (100 GiB).
	OfflineDownloadMaxSizeGB = "offline_download_max_size_gb"
	// OfflineDownloadTool selects the active offline-download daemon tool (子项目
	// B): a Select over "SimpleHttp" plus the ids of the enabled
	// OfflineDownloadTool backends (aria2/qBittorrent/Transmission). "SimpleHttp"
	// (default) keeps the built-in direct-download path. Changing it re-inits the
	// offline_download base (offline_download.Init via a setting hook) to resolve
	// the newly-selected tool.
	OfflineDownloadTool = "offline_download_tool"
	// OfflineDownloadTransferThreadsNum bounds the concurrency of the transfer
	// engine (子项目 B, wave B2): after a daemon tool finishes a download into the
	// local temp area, TransferTasks move the finished files into the destination
	// storage. This is the worker count for that second (transfer) task manager,
	// independent of the download worker count (offline_download_task_threads_num).
	OfflineDownloadTransferThreadsNum = "offline_download_transfer_threads_num"
	// Aria2Uri / Aria2Secret configure the aria2 daemon the
	// builtin.offline_download.aria2 tool extension talks to over JSON-RPC (子项目
	// B, wave B3). Aria2Uri is the JSON-RPC endpoint (default
	// http://localhost:6800/jsonrpc, VISIBLE so an admin can point it at their
	// daemon); Aria2Secret is the aria2 rpc secret token (a credential, so PRIVATE
	// and kept out of logs). The tool lazily (re)builds its client from these on
	// change and gates readiness on an aria2.getVersion probe.
	Aria2Uri    = "aria2_uri"
	Aria2Secret = "aria2_secret"

	// QbittorrentUrl / QbittorrentSeedtime configure the qBittorrent WebUI the
	// builtin.offline_download.qbittorrent tool extension talks to over the WebUI
	// API v2 (子项目 B, wave B4). QbittorrentUrl is the WebUI base URL and MAY embed
	// credentials in its userinfo (default http://admin:adminadmin@localhost:8080/),
	// so it is PRIVATE and kept out of logs. QbittorrentSeedtime is the seeding time
	// limit in minutes (default 0 = no limit); under the transfer-then-remove
	// lifecycle it is applied best-effort at add time, not enforced by a blocking
	// wait. The tool lazily (re)builds its client from the URL on change and gates
	// readiness on an app/version probe.
	QbittorrentUrl      = "qbittorrent_url"
	QbittorrentSeedtime = "qbittorrent_seedtime"

	// TransmissionUri / TransmissionSeedtime configure the Transmission daemon the
	// builtin.offline_download.transmission tool extension talks to over the
	// Transmission RPC (子项目 B, wave B5). TransmissionUri is the RPC endpoint
	// (default http://localhost:9091/transmission/rpc, VISIBLE so an admin can
	// point it at their daemon; the default is localhost with no embedded
	// credentials). TransmissionSeedtime is the seeding time limit in minutes
	// (default 0 = no limit); under the transfer-then-remove lifecycle it is NOT
	// enforced by a blocking wait — Transmission's minimal RPC seam (add/get/
	// remove) exposes no seed-limit mutator, so the setting is retained for parity/
	// future use only. The tool lazily (re)builds its client from the URI on change
	// and gates readiness on a cheap torrent-get probe.
	TransmissionUri      = "transmission_uri"
	TransmissionSeedtime = "transmission_seedtime"

	// frp (internal tunnel; manages an external frpc process — design §2.1)
	FrpEnabled    = "frp_enabled"
	FrpServerAddr = "frp_server_addr"
	FrpServerPort = "frp_server_port"
	FrpToken      = "frp_token"
	FrpRemotePort = "frp_remote_port"
	FrpProtocol   = "frp_protocol"
	FrpBinaryPath = "frp_binary_path"
)

const (
	UNKNOWN = iota
	FOLDER
	// OFFICE
	VIDEO
	AUDIO
	TEXT
	IMAGE
)
