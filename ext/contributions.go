package ext

// Contributions is the declarative front-end surface an extension adds to the
// host UI (SDK doc §4). The host aggregates the Contributions of every enabled
// extension into one versioned document
// (GET /api/v4/extensions/contributions); the frontend renders them natively.
// Everything here is data only — no behavior crosses this boundary.
type Contributions struct {
	ConfigForm     *JSONSchema     `json:"config_form,omitempty"`
	MenuActions    []MenuAction    `json:"menu_actions,omitempty"`
	Previewers     []PreviewerReg  `json:"previewers,omitempty"`
	UIPanels       []UIPanel       `json:"ui_panels,omitempty"`
	DashboardCards []DashboardCard `json:"dashboard_cards,omitempty"`
	SettingsPages  []SettingsPage  `json:"settings_pages,omitempty"`
	Themes         []Theme         `json:"themes,omitempty"`
	I18n           []I18nBundle    `json:"i18n,omitempty"`
	AssistForms    []AssistForm    `json:"assist_forms,omitempty"`

	// StatusProviders render a runtime status card the host polls (design §4):
	// AccessProtocol / BackgroundService plugins (running state, port, metrics,
	// start/stop toggle).
	StatusProviders []StatusProvider `json:"status_providers,omitempty"`
	// LoginMethods render login affordances for an AuthNMethod (design §4):
	// primary/federated login-page buttons, or a second-factor prompt component.
	LoginMethods []LoginMethod `json:"login_methods,omitempty"`
	// PublicPages render an extension's unauthenticated landing page (design §4):
	// a share bundle's public page (iframe, guest context).
	PublicPages []PublicPage `json:"public_pages,omitempty"`
	// FileColumns add a badge/column to the file listing (design §4): the labels
	// badge. Its data source is the extension's FileDecorator field group.
	FileColumns []FileColumn `json:"file_columns,omitempty"`
	// ConfigSections render titled setting groups on a feature's config page
	// (design C). A base feature declares its own "general" section with
	// Parent=self; an extension whose config extends a base feature declares one
	// with Parent=<base feature id>. The frontend's FeatureConfigPage(id) collects
	// every section (across enabled extensions) whose Parent==id.
	ConfigSections []ConfigSection `json:"config_sections,omitempty"`
	// PublicPrefix is the top-level short URL prefix an extension requests for
	// its PublicPages (design §3 PublicRouter): a share bundle requesting "/s"
	// is reachable at "/s/:share_id" instead of the default
	// /api/v4/ext-public/{id}/... namespace. A single, rooted, lowercase
	// alphanumeric-plus-hyphen segment (e.g. "/s") — validated at manifest load
	// (ValidateManifest) for that shape, and at registration
	// (Registry.RegisterPluginMode/Register) against the reserved-word table
	// (internal/extension/apiroute.go CheckReservedPrefix) and cross-plugin
	// mutual exclusion (a second extension requesting an already-claimed
	// prefix is refused). Empty means no custom prefix: the extension's
	// PublicPages stay under the default namespace.
	PublicPrefix string `json:"public_prefix,omitempty"`
}

// ConfigSection is a titled group of settings rendered on the Parent feature's
// config page (design C). An extension whose config extends a base feature
// declares one with Parent=<feature id>; a base feature declares its own
// "general" section with Parent=<self id>. Membership in the merged document
// equals "enabled" — disabling the owning extension removes its section, so the
// feature page shows only the sections of currently-enabled contributors.
type ConfigSection struct {
	Parent      string            `json:"parent"`
	Title       map[string]string `json:"title"`
	SettingKeys []string          `json:"setting_keys"`
	Order       int               `json:"order,omitempty"`
}

// FileColumn adds a badge/column to the file listing (design §4). Its data is
// NOT inlined in the base listing; it is registered as an fs/list extension field
// group (FieldGroup, "_"-prefixed), fetched on demand from the extension's
// FileDecorator only when the column is actually shown — so a column nobody opens
// costs nothing, and a slow decorator only slows its own column (design §2.6).
type FileColumn struct {
	ID         string            `json:"id"`
	Header     map[string]string `json:"header"`
	FieldGroup string            `json:"field_group"` // e.g. "_labels"; data source = FileDecorator
}

// LoginMethod renders a login affordance for an AuthNMethod (design §4). Kind
// selects placement: "primary"/"federated" methods appear as login-page buttons
// that drive the host login flow; a "second_factor" method renders a prompt
// component after the password step. UIRef optionally points at a custom
// component bundle (iframe); otherwise the frontend uses a generic renderer.
type LoginMethod struct {
	ID    string            `json:"id"`
	Kind  string            `json:"kind"` // primary | second_factor | federated
	Label map[string]string `json:"label"`
	Icon  string            `json:"icon,omitempty"`
	UIRef string            `json:"ui_ref,omitempty"`
}

// PublicPage renders an extension's unauthenticated landing page (design §4): a
// share bundle's public page. Route is mounted under the Manifest-requested
// top-level short prefix (e.g. public_prefix="/s" → "/s/:share_id"); an
// extension that requests no prefix falls back to the default namespace
// (/api/v4/ext-public/{id}/…). UIRef points at the iframe bundle, which runs in a
// guest context (the Bridge exposes only the public method subset).
type PublicPage struct {
	ID    string `json:"id"`
	Route string `json:"route"`
	UIRef string `json:"ui_ref,omitempty"`
}

// StatusProvider is a runtime status card the frontend renders on the
// system/services page (design §4). The host polls PollPath and hands the card
// the extension's reported status (an AccessProtocol's or BackgroundService's
// Status()): running state, port, metrics, a start/stop toggle.
type StatusProvider struct {
	ID    string            `json:"id"`
	Title map[string]string `json:"title"`
	// PollPath is the host status endpoint the frontend polls, e.g.
	// "/api/v4/extensions/{id}/status/{ID}" → the host resolves the extension's
	// Status().
	PollPath string `json:"poll_path"`
}

// MenuAction is a context-menu / toolbar action. Triggering it makes the
// frontend POST /api/v4/extensions/{id}/actions/{action-id}; the host routes to
// the extension's Action handler (SDK §4).
type MenuAction struct {
	ID     string            `json:"id"`
	Label  map[string]string `json:"label"`
	Icon   string            `json:"icon,omitempty"`
	Target string            `json:"target"` // file | dir | selection | global
}

// PreviewerReg registers a custom previewer: accepted mime/extension globs and
// the plugin-UI route that renders them (served under /ui/).
type PreviewerReg struct {
	ID     string   `json:"id"`
	Accept []string `json:"accept"`
	Route  string   `json:"route"`
}

// UIPanel is a custom rich-interaction panel rendered in a sandboxed iframe from
// the plugin bundle (SDK §5).
type UIPanel struct {
	ID        string            `json:"id"`
	Title     map[string]string `json:"title"`
	Route     string            `json:"route"`
	Placement string            `json:"placement,omitempty"` // sidebar | main | modal
}

// DashboardCard is a card shown on the admin dashboard.
type DashboardCard struct {
	ID    string            `json:"id"`
	Title map[string]string `json:"title"`
	Route string            `json:"route,omitempty"`
}

// SettingsPage is a settings section the extension contributes.
type SettingsPage struct {
	ID    string            `json:"id"`
	Title map[string]string `json:"title"`
	Route string            `json:"route,omitempty"`
}

// Theme is a UI theme the extension contributes (theming spec §5.2). Beyond the
// picker metadata (id/name/author) it points at the theme's token file and its
// optional restricted-css file + bundled fonts — all RELATIVE paths into the
// plugin's own asset tree, served same-origin under /plugin-assets/<plugin-id>/.
// The frontend fetches these, runs them through the shared sanitizer/validator
// gate, and registers the theme (no code or behavior crosses the boundary — a
// theme is declarative data + a gated, scoped stylesheet). A declaration with
// only id/name is metadata the frontend cannot load (nothing to fetch).
type Theme struct {
	ID     string            `json:"id"`
	Name   map[string]string `json:"name"`
	Author string            `json:"author,omitempty"`
	Tokens string            `json:"tokens,omitempty"` // relative path to the token JSON (spec §5.3)
	Css    string            `json:"css,omitempty"`    // optional relative path to the restricted CSS (§5.4)
	Fonts  []ThemeFont       `json:"fonts,omitempty"`  // optional same-origin bundled fonts (§5.5.3)
}

// ThemeFont is a bundled-font reference in a theme contribution: a family NAME +
// a RELATIVE path into the plugin's assets. The host rewrites the path to a
// same-origin /plugin-assets/<id>/… url and emits the @font-face — the plugin
// never writes a remote src. woff2 only (validated by the frontend's magic-byte
// sniff), backed by CSP font-src 'self'.
type ThemeFont struct {
	Family string `json:"family"`
	Src    string `json:"src"`
	Weight string `json:"weight,omitempty"`
	Style  string `json:"style,omitempty"`
}

// I18nBundle points at a locale message bundle the extension ships.
type I18nBundle struct {
	Locale string `json:"locale"`
	Route  string `json:"route"`
}

// AssistForm is an in-place completion sub-form referenced by a field's
// x-assist (SDK §4.1). Submitting it makes the frontend POST
// /api/v4/extensions/{id}/assist/{form-id}; the returned value refills the
// originating field.
type AssistForm struct {
	ID        string            `json:"id"`
	Title     map[string]string `json:"title"`
	Schema    *JSONSchema       `json:"schema"`
	TestProbe string            `json:"test_probe,omitempty"`
}
