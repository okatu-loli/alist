package extension

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// StateFile is the lifecycle state file, kept beside the per-plugin dirs under
// <DataDir>/plugins. Decision (9.3): plugin state lives in this JSON file, NOT a
// DB table — the file system (per-version dirs) is already the source of truth,
// no schema is stable yet, and a migration can formalize it later if the admin
// UI needs relational queries.
const StateFile = "state.json"

// PluginState is the persisted record for one installed plugin.
type PluginState struct {
	ID              string            `json:"id"`
	Kind            ext.Kind          `json:"kind"`
	Name            map[string]string `json:"name,omitempty"` // manifest i18n display name (LangMap); shown by the UI instead of the id
	ActiveVersion   string            `json:"active_version"`
	PreviousVersion string            `json:"previous_version,omitempty"` // rollback target
	Versions        []string          `json:"versions"`                   // versions present on disk
	Enabled         bool              `json:"enabled"`
	Tier            string            `json:"tier,omitempty"`      // trust tier at install (official|verified|third-party)
	ExecModeName    string            `json:"exec_mode,omitempty"` // chosen execution mode (grpc|in-process)
	InstalledAt     time.Time         `json:"installed_at"`
	UpdatedAt       time.Time         `json:"updated_at"`

	// Grant is what the admin APPROVED for this plugin (design §6.2), persisted
	// alongside the rest of the lifecycle state. It — not the Manifest — is the
	// runtime authority: the dispatch gate and the borrowed-capability layer read
	// it, so a capability the Manifest declares but the Grant omits is denied. Nil
	// until consent is recorded; a nil Grant means default-deny at the gate, NOT
	// "fall back to the Manifest". A direct install records a default Grant
	// (grantFromManifest); a wizard/consent install or a post-install adjust
	// overwrites it via SetGrant.
	Grant *Grant `json:"grant,omitempty"`
	// NeedsReconsent marks a plugin whose active version requests MORE than the
	// current Grant approves (design §6.6, set by Update). The extra capabilities
	// stay denied at runtime until the admin re-approves via SetGrant; the UI
	// surfaces the flag so the admin knows re-consent is due.
	NeedsReconsent bool `json:"needs_reconsent,omitempty"`

	// Config is the persisted config_form values an admin set for this plugin
	// (a JSON object). Stage 1: stored/served for the admin UI only — the plugin
	// cannot yet read it back (the reverse host bridge is a later wave). Empty
	// until an admin saves.
	Config json.RawMessage `json:"config,omitempty"`
}

// state is the whole state file.
type state struct {
	Plugins map[string]*PluginState `json:"plugins"`
}

// Manager owns the installed-plugin lifecycle: install / enable / disable /
// update / rollback / uninstall, persisting state to <dir>/state.json and
// integrating enable/disable with a Registry.
//
// Every install/enable is gated on the trust tier (TieredVerifier) and the
// admin policy (plugin_policy) — enforced at BOTH install and enable (load) so
// a tightened policy or a hand-edited state file cannot smuggle a plugin past
// the gate. Since the 2026-07-14 plugin-parity decision all tiers execute the
// same way (gRPC subprocess); the tier decides install warnings and the policy
// floor only.
type Manager struct {
	dir      string
	registry *Registry
	tiered   *TieredVerifier
	policy   func() Policy

	// sup is the BackgroundService supervisor this Manager drives on
	// enable/disable/uninstall/rollback so a Kind=BackgroundService DISK plugin's
	// supervised goroutine (its Run — which spawns the frpc process) is actually
	// started/stopped when the admin flips it, exactly as BuiltinManager does for
	// built-in services. nil in tests / single-root callers (no supervisor), where
	// enable/disable stays a pure registry+state operation. Wired to
	// DefaultServiceSupervisor for the process-wide DefaultManager.
	sup *ServiceSupervisor

	mu    sync.Mutex
	state *state
}

// SetSupervisor wires the BackgroundService supervisor this Manager drives on
// lifecycle transitions. Called once at DefaultManager construction. A nil
// supervisor leaves BackgroundService enable/disable as a pure registry change
// (the pre-supervisor behavior tests rely on).
func (m *Manager) SetSupervisor(sup *ServiceSupervisor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sup = sup
}

// isBackgroundService reports whether the plugin id declares Kind=
// BackgroundService. Caller holds m.mu. Used to decide whether a lifecycle
// transition must start/stop a supervised service goroutine. Reads the
// Manifest's declared Kinds (DeclaresKind), not Entry.HasKind: HasKind only
// sees the entry's primary Kind plus extraKinds, and extraKinds is populated
// solely by FeatureBundle indexing — a plugin that legitimately declares
// BackgroundService alongside another primary Kind (e.g. Kinds:
// ["storage_driver", "background_service"]) would never get an extraKinds
// entry for it and so a runtime enable/disable would silently skip
// start/stop despite the plugin having declared the service (mirrors
// SetGrant's existing hasKind(man, ...) use just above, and resolveAction in
// actions.go).
func (m *Manager) isBackgroundService(id string) bool {
	e, ok := m.registry.Get(id)
	return ok && e != nil && e.DeclaresKind(ext.KindBackgroundService)
}

// isFeatureBundle reports whether the plugin id declares Kind=FeatureBundle,
// mirroring isBackgroundService above (DeclaresKind, the Manifest truth
// source — see its doc comment for why HasKind is the wrong question here).
func (m *Manager) isFeatureBundle(id string) bool {
	e, ok := m.registry.Get(id)
	return ok && e != nil && e.DeclaresKind(ext.KindFeatureBundle)
}

// expandFeatureBundleIfDeclared (re-)wires a FeatureBundle plugin's
// contributed sub-Kinds (APIRoute/EventHook/ScheduledJob/FileDecorator) onto
// the registry right after id's entry has been (re-)registered.
// registerActive fully REPLACES the entry (Deregister, which drops every
// extraKinds index a prior expansion added, followed by RegisterPluginMode,
// which creates a brand-new Entry with none), so every lifecycle transition
// that calls registerActive on an enabled plugin — Enable, LoadEnabled (a
// server restart), Update's re-registration of an already-enabled plugin, and
// Rollback — must re-expand a bundle right after, or its contributed points
// silently go unreachable until the next full Disable/Enable cycle (design
// §2.5's F1 wiring points, gRPC leg: featurebundle.go
// expandGRPCFeatureBundle). Failure is logged, not returned/fatal: a bundle
// that fails to expand (e.g. a manifest that stopped declaring
// FeatureBundle, or a malformed sub-Kind list) must never fail the whole
// lifecycle operation the caller is performing — mirrors the
// fire-and-forget `_ = m.sup.StartService(id)` pattern just below for
// BackgroundService. Caller holds m.mu.
func (m *Manager) expandFeatureBundleIfDeclared(id string) {
	if !m.isFeatureBundle(id) {
		return
	}
	if _, err := ExpandFeatureBundle(m.registry, id); err != nil {
		log.Warnf("extension: failed to expand feature bundle %q: %v", id, err)
	}
}

// NewManager builds a Manager rooted at dir (<DataDir>/plugins), treating
// verifier as the sole (official) trust root under the most restrictive policy
// (official-only). This preserves the Step 9 behavior — only official-signed
// packages install — and is what the lifecycle tests and any single-root caller
// get. Use NewManagerWithTrust for the tiered, policy-driven Step 11 path.
func NewManager(dir string, registry *Registry, verifier *Verifier) (*Manager, error) {
	return NewManagerWithTrust(dir, registry, &TieredVerifier{official: verifier},
		func() Policy { return PolicyOfficialOnly })
}

// NewManagerWithTrust builds a Manager with an explicit tiered verifier (official
// + optional verified-community root) and a policy source. policy is a func so a
// live setting change takes effect on the next install/enable without rebuilding
// the Manager. A nil policy defaults to official-only (fail-closed).
func NewManagerWithTrust(dir string, registry *Registry, tiered *TieredVerifier, policy func() Policy) (*Manager, error) {
	if policy == nil {
		policy = func() Policy { return PolicyOfficialOnly }
	}
	m := &Manager{dir: dir, registry: registry, tiered: tiered, policy: policy}
	if err := m.loadState(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) loadState() error {
	m.state = &state{Plugins: map[string]*PluginState{}}
	b, err := os.ReadFile(filepath.Join(m.dir, StateFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("extension: read state: %w", err)
	}
	if err := json.Unmarshal(b, m.state); err != nil {
		return fmt.Errorf("extension: parse state: %w", err)
	}
	if m.state.Plugins == nil {
		m.state.Plugins = map[string]*PluginState{}
	}
	return nil
}

// persistState writes state.json atomically (temp + rename). Caller holds m.mu.
func (m *Manager) persistState() error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(m.dir, StateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.dir, StateFile))
}

func (m *Manager) versionDir(id, version string) string {
	return filepath.Join(m.dir, id, version)
}

// Dir returns the plugin root directory (state.json + per-plugin dirs live here).
// Used by the reverse-host builder to root the persistent KV backend.
func (m *Manager) Dir() string { return m.dir }

// List returns a copy of all installed plugin states, id-sorted for stable
// output.
func (m *Manager) List() []PluginState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PluginState, 0, len(m.state.Plugins))
	for _, p := range m.state.Plugins {
		out = append(out, *p)
	}
	// small n; simple insertion by id keeps output deterministic without a sort import churn
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// UIDir returns the ui/ subdirectory of a plugin's ACTIVE version dir, where
// its iframe bundle lives (SDK §5). It errors if the plugin is not installed;
// the dir itself may not exist (a plugin without custom UI), which the caller
// surfaces as a 404 when a file is requested.
func (m *Manager) UIDir(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return "", fmt.Errorf("extension %q: not installed", id)
	}
	return filepath.Join(m.versionDir(ps.ID, ps.ActiveVersion), "ui"), nil
}

// AssetDir returns the ROOT of a plugin's ACTIVE version dir, from which its
// declared theme assets (token JSON, restricted css, bundled woff2) are served
// read-only under /plugin-assets/<id>/ (theming spec §5.4). Unlike UIDir it is
// the version root, not the ui/ subdir, because theme files live alongside the
// manifest (themes/, fonts/). It errors if the plugin is not installed; a
// requested file that does not exist surfaces as a 404 at the handler.
func (m *Manager) AssetDir(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return "", fmt.Errorf("extension %q: not installed", id)
	}
	return m.versionDir(ps.ID, ps.ActiveVersion), nil
}

// ActiveManifest loads and validates the manifest of a plugin's ACTIVE version.
// Used by the install wizard to read Manifest.Wizard for a plugin that may not
// be enabled yet.
func (m *Manager) ActiveManifest(id string) (ext.Manifest, error) {
	m.mu.Lock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		m.mu.Unlock()
		return ext.Manifest{}, fmt.Errorf("extension %q: not installed", id)
	}
	dir := m.versionDir(ps.ID, ps.ActiveVersion)
	m.mu.Unlock()
	return LoadManifest(dir)
}

// ActiveTier classifies a plugin's ACTIVE version: its trust tier, the imported
// source that vouches for it (🟡 only, else ""), and its manifest. Used by the
// install wizard to pick the right risk interstitial. A present-but-invalid
// signature reports third-party (install itself would reject it), so the wizard
// errs toward showing the risk page.
func (m *Manager) ActiveTier(id string) (Tier, string, ext.Manifest, error) {
	m.mu.Lock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		m.mu.Unlock()
		return TierThirdParty, "", ext.Manifest{}, fmt.Errorf("extension %q: not installed", id)
	}
	dir := m.versionDir(ps.ID, ps.ActiveVersion)
	m.mu.Unlock()
	man, err := LoadManifest(dir)
	if err != nil {
		return TierThirdParty, "", ext.Manifest{}, err
	}
	tier, source, _ := m.tiered.DetermineTier(dir, man)
	return tier, source, man, nil
}

// Get returns the state of one installed plugin.
func (m *Manager) Get(id string) (PluginState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.state.Plugins[id]
	if !ok {
		return PluginState{}, false
	}
	return *p, true
}

// SetGrant records the admin-approved Grant for an installed plugin (design
// §6.4 consent submit / §6.5 post-install adjust). It REPLACES the effective
// Grant wholesale — the caller submits the complete approved set — stamps it
// with the active version as reviewed, clears NeedsReconsent, and persists.
// Because the runtime dispatch gate reads this Grant on every dispatch and the
// borrowed-capability layer is rebuilt per plugin launch, a revocation or a
// narrowed network host takes effect immediately (no cache to invalidate). A nil
// grant records an empty Grant: revoke everything (default-deny).
//
// allowExtraNetwork lets the submitted grant's network set include a host the
// manifest did not declare — the one axis an admin may extend beyond the
// manifest, and only once they've acknowledged the risk (the server handler
// gates this on an explicit `acknowledged` flag before calling in). host_apis,
// storage_tokens, and the fs level stay clamped to the manifest regardless.
func (m *Manager) SetGrant(id string, g *Grant, allowExtraNetwork bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return fmt.Errorf("extension %q: not installed", id)
	}
	man, err := LoadManifest(m.versionDir(ps.ID, ps.ActiveVersion))
	if err != nil {
		return err
	}
	// The Manifest is the ceiling: an admin may narrow what the plugin requested,
	// never approve a capability it never declared. Clamp the submitted grant to
	// the manifest so a buggy console / CSRF'd admin session / API misuse cannot
	// over-grant beyond the plugin's declared needs (F-consent red-team F2),
	// except for network hosts when allowExtraNetwork is set.
	ng := g.clampToManifest(man, allowExtraNetwork)
	ng.ReviewedVersion = ps.ActiveVersion
	ps.Grant = ng
	ps.NeedsReconsent = false
	ps.UpdatedAt = time.Now()
	if err := m.persistState(); err != nil {
		return err
	}
	// Rebind a running BackgroundService so its borrowed-capability layer (which
	// closes over Grant.Network at launch) picks up the new allowlist. host_api
	// gate changes are already live per-dispatch; non-bg subprocess eviction is
	// done by the caller (server layer) via pluginproc.Default.Restart. StopService
	// then StartService installs a fresh session (mirrors Enable's supervisor use).
	if ps.Enabled && m.sup != nil && hasKind(man, ext.KindBackgroundService) {
		m.sup.StopService(id)
		_ = m.sup.StartService(id)
	}
	return nil
}

// hasKind reports whether the manifest declares Kind k.
func hasKind(m ext.Manifest, k ext.Kind) bool {
	for _, kk := range m.Kinds {
		if kk == k {
			return true
		}
	}
	return false
}

// GrantedHostAPIs is the persisted-consent resolver the dispatch gate reads
// (grant_gate.go grantOverride). For an installed plugin it returns the host_api
// names its Grant approves and true — AUTHORITATIVE, so the gate never falls
// back to the Manifest: a capability the Manifest declares but the Grant omits
// is denied. An installed plugin with no recorded Grant returns an empty set +
// true (default-deny). An id the Manager does not know (e.g. a built-in, which
// declares no plugin permissions) returns false, letting the gate apply its
// own built-in rule rather than force-denying it.
func (m *Manager) GrantedHostAPIs(id string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return nil, false
	}
	return ps.Grant.hostAPINames(), true
}

// RequestedPermissions is the flat set of capabilities a plugin's Manifest
// declares — "what the plugin asks for" (design §6.1) — the requested side of
// the consent/settings UI, paired with the approved Grant.
type RequestedPermissions struct {
	Network       []string `json:"network,omitempty"`
	HostAPIs      []string `json:"host_apis,omitempty"`
	StorageTokens []string `json:"storage_tokens,omitempty"`
	Filesystem    string   `json:"filesystem,omitempty"`
}

// PermissionView is the {requested, granted} document the admin permission /
// consent UI renders (design §6.7 GET /permissions): Requested from the active
// version's Manifest, Granted the persisted Grant (nil until consent is
// recorded), and NeedsReconsent flagging an update that asked for more than the
// current Grant approves. It is the single data source for both the install
// consent page and the post-install "permissions & network" settings slot.
type PermissionView struct {
	ID             string               `json:"id"`
	Version        string               `json:"version"`
	Requested      RequestedPermissions `json:"requested"`
	Granted        *Grant               `json:"granted,omitempty"`
	NeedsReconsent bool                 `json:"needs_reconsent"`
}

// PermissionView builds the {requested, granted} document for one installed
// plugin. Requested is read from the active version's Manifest; Granted is a
// copy of the persisted Grant so a caller cannot mutate lifecycle state through
// the returned pointer.
func (m *Manager) PermissionView(id string) (PermissionView, error) {
	m.mu.Lock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		m.mu.Unlock()
		return PermissionView{}, fmt.Errorf("extension %q: not installed", id)
	}
	var grant *Grant
	if ps.Grant != nil {
		g := *ps.Grant
		grant = &g
	}
	version := ps.ActiveVersion
	needs := ps.NeedsReconsent
	dir := m.versionDir(ps.ID, ps.ActiveVersion)
	m.mu.Unlock()

	man, err := LoadManifest(dir)
	if err != nil {
		return PermissionView{}, err
	}
	return PermissionView{
		ID:      id,
		Version: version,
		Requested: RequestedPermissions{
			Network:       man.Permissions.Network,
			HostAPIs:      man.Permissions.HostAPIs,
			StorageTokens: man.Permissions.StorageTokens,
			Filesystem:    man.Permissions.Filesystem,
		},
		Granted:        grant,
		NeedsReconsent: needs,
	}, nil
}

// GetConfig returns the persisted config_form values for an installed plugin
// (a JSON object), or nil if none has been saved. Errors if id is not installed.
//
// G4 §3: secretConfigFieldsOrErr (below) is resolved BEFORE m.mu is taken —
// ActiveManifest (which it calls) locks m.mu itself, and this mutex is not
// reentrant, so computing it while already holding the lock would deadlock.
// The plugin's active version could in principle change between this call
// and the locked section below (a vanishingly narrow window); the same
// already exists for GetPluginConfig's own redaction (server/handles/
// plugins.go), so this isn't a new risk this change introduces.
//
// [Minor 3] Uses secretConfigFieldsOrErr, not SecretConfigFields: if the
// manifest itself fails to resolve, this rejects rather than silently
// treating the plugin as having no secret fields — which would otherwise
// hand back still-encrypted ciphertext masquerading as the real config
// value with no indication anything was wrong.
func (m *Manager) GetConfig(id string) (json.RawMessage, error) {
	secretFields, err := m.secretConfigFieldsOrErr(id)
	if err != nil {
		return nil, fmt.Errorf("extension %q: resolving secret config fields: %w", id, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return nil, fmt.Errorf("extension %q: not installed", id)
	}
	return m.decryptSecretConfigFields(ps.Config, secretFields)
}

// SetConfig persists the admin-supplied config_form values for an installed
// plugin. cfg must be valid JSON (an empty cfg clears it). Stage 1 only stores;
// the plugin does not read it back yet, so no subprocess rebind is needed.
//
// G4 §3: secret fields (x-secret in the plugin's config_form) are encrypted
// before the value ever reaches persistState/disk — see
// encryptSecretConfigFields's doc comment for why secretConfigFieldsOrErr
// must be resolved before m.mu is taken, same as GetConfig above.
//
// [Minor 3] Uses secretConfigFieldsOrErr, not SecretConfigFields: if the
// manifest itself fails to resolve, this REJECTS THE SAVE rather than
// silently treating the plugin as having no secret fields, which would
// otherwise write a genuinely secret value to disk in plaintext the moment
// its manifest transiently fails to parse — exactly when the caller has the
// least ability to tell that happened.
func (m *Manager) SetConfig(id string, cfg json.RawMessage) error {
	secretFields, err := m.secretConfigFieldsOrErr(id)
	if err != nil {
		return fmt.Errorf("extension %q: resolving secret config fields: %w", id, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return fmt.Errorf("extension %q: not installed", id)
	}
	if len(cfg) > 0 && !json.Valid(cfg) {
		return fmt.Errorf("extension %q: config is not valid JSON", id)
	}
	encrypted, err := m.encryptSecretConfigFields(cfg, secretFields)
	if err != nil {
		return err
	}
	ps.Config = encrypted
	ps.UpdatedAt = time.Now()
	return m.persistState()
}

// GrantForID returns a copy of the plugin's persisted Grant (nil if none), and
// whether the plugin is installed. Used by the reverse-host builder.
func (m *Manager) GrantForID(id string) (*Grant, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok || ps.Grant == nil {
		return nil, ok
	}
	g := *ps.Grant
	return &g, true
}

// Install installs a plugin from a local .tar.gz package. The package is
// extracted to a staging dir, its signature+checksums are verified and its
// manifest validated BEFORE anything is moved into place; only then is the
// version dir atomically swapped in. A fresh install lands disabled; call Enable
// to register+run it. Returns the installed manifest.
func (m *Manager) Install(tarPath string) (ext.Manifest, error) {
	return m.installVersion(tarPath, false)
}

// Update installs a new version of an already-installed plugin alongside the
// current one, switches the active version to it, and remembers the prior
// version for Rollback. If the plugin was enabled, it is re-registered on the
// new version.
func (m *Manager) Update(tarPath string) (ext.Manifest, error) {
	return m.installVersion(tarPath, true)
}

func (m *Manager) installVersion(tarPath string, isUpdate bool) (ext.Manifest, error) {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return ext.Manifest{}, err
	}
	staging, err := os.MkdirTemp(m.dir, ".staging-*")
	if err != nil {
		return ext.Manifest{}, err
	}
	defer os.RemoveAll(staging)

	if err := extractTarGz(tarPath, staging); err != nil {
		return ext.Manifest{}, err
	}
	man, err := LoadManifest(staging) // validates kind + core_api compatibility
	if err != nil {
		return ext.Manifest{}, err
	}
	// Step 11 gate (before anything is trusted or moved into place): classify the
	// trust tier, enforce the admin policy floor, and pick the execution mode. A
	// present-but-invalid signature is an integrity failure and is rejected
	// outright; a disallowed tier or a package with no runnable binary is
	// refused with a clear reason.
	tier, mode, err := m.gate(staging, man)
	if err != nil {
		return ext.Manifest{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, known := m.state.Plugins[man.ID]
	if isUpdate {
		if !known {
			return ext.Manifest{}, fmt.Errorf("extension %q: not installed, cannot update", man.ID)
		}
		if existing.ActiveVersion == man.Version {
			return ext.Manifest{}, fmt.Errorf("extension %q: version %s is already active", man.ID, man.Version)
		}
	}

	final := m.versionDir(man.ID, man.Version)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return ext.Manifest{}, err
	}
	// Reinstalling the exact version: drop the old dir so the rename lands.
	if err := os.RemoveAll(final); err != nil {
		return ext.Manifest{}, err
	}
	if err := os.Rename(staging, final); err != nil { // atomic dir swap
		return ext.Manifest{}, fmt.Errorf("extension: install swap: %w", err)
	}

	now := time.Now()
	if !known {
		ps := &PluginState{
			ID:            man.ID,
			Kind:          primaryKind(man),
			Name:          man.Name,
			ActiveVersion: man.Version,
			Versions:      []string{man.Version},
			Enabled:       false,
			Tier:          tier.String(),
			ExecModeName:  mode.String(),
			InstalledAt:   now,
			UpdatedAt:     now,
			// Direct-install default consent: approve exactly what the Manifest
			// requests (design §6.2). A wizard/consent install overwrites this with
			// the admin's actual selection via SetGrant; the runtime gate reads this
			// Grant, never the Manifest, from here on.
			Grant: grantFromManifest(man),
		}
		m.state.Plugins[man.ID] = ps
	} else {
		if !contains(existing.Versions, man.Version) {
			existing.Versions = append(existing.Versions, man.Version)
		}
		if isUpdate {
			existing.PreviousVersion = existing.ActiveVersion
			existing.ActiveVersion = man.Version
			existing.Kind = primaryKind(man)
			existing.Tier = tier.String()
			existing.ExecModeName = mode.String()
			existing.UpdatedAt = now
			// Incremental re-authorization (design §6.6): an update NEVER
			// auto-grants a capability the admin has not approved. The prior
			// Grant is kept as-is; if the new version requests more than it
			// approves, flag NeedsReconsent — the extra capabilities stay
			// denied at the runtime gate until the admin re-approves via
			// SetGrant. A pure downgrade needs no re-consent. If somehow no
			// Grant was recorded (legacy state), seed the default so the gate
			// still reads a persisted authority instead of the Manifest.
			if existing.Grant == nil {
				existing.Grant = grantFromManifest(man)
				existing.NeedsReconsent = false
			} else {
				existing.NeedsReconsent = manifestExceedsGrant(man, existing.Grant)
				// Clamp the kept Grant to the new manifest: a downgrade whose
				// manifest drops a capability must not leave a stale approval for
				// it (the manifest is the live ceiling — F-consent red-team F3).
				// Widenings are gated by NeedsReconsent above; clamping only ever
				// removes now-undeclared capabilities, never adds.
				existing.Grant = existing.Grant.clampToManifest(man, false)
			}
			if existing.Enabled {
				// re-register on the new version's binary
				if err := m.registerActive(existing); err != nil {
					return ext.Manifest{}, err
				}
				// registerActive replaced the entry (Deregister + fresh
				// RegisterPluginMode), so a FeatureBundle's expansion must be
				// redone against the new entry, same as Enable/LoadEnabled.
				m.expandFeatureBundleIfDeclared(existing.ID)
			}
		}
	}
	if err := m.persistState(); err != nil {
		return ext.Manifest{}, err
	}
	return man, nil
}

// Enable registers the plugin's active version with the Registry (gRPC exec
// mode) and marks it enabled. Enabling a StorageDriver plugin makes it visible
// to the executor; actually running it is the 9.2-tested path.
func (m *Manager) Enable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return fmt.Errorf("extension %q: not installed", id)
	}
	if err := m.registerActive(ps); err != nil {
		return err
	}
	ps.Enabled = true
	ps.UpdatedAt = time.Now()
	if err := m.persistState(); err != nil {
		return err
	}
	// A Kind=BackgroundService disk plugin needs its supervised Run started now
	// (spawns the frpc subprocess); without this the entry is registered but the
	// service never runs. StartService is idempotent and skips non-bg / unwired
	// entries, so this is a no-op for storage/auth plugins.
	if m.sup != nil && m.isBackgroundService(id) {
		_ = m.sup.StartService(id)
	}
	// A Kind=FeatureBundle disk plugin needs its contributed sub-Kinds
	// (APIRoute/EventHook/ScheduledJob/FileDecorator) indexed onto the
	// registry now that its gRPC entry exists — without this the entry sits
	// registered under its primary Kind alone and every dispatch path the
	// bundle contributes (DispatchEvent, the scheduler tick, DecorateFields,
	// an APIRoute proxy) never reaches it.
	m.expandFeatureBundleIfDeclared(id)
	return nil
}

// LoadEnabled re-registers every plugin marked Enabled in the persisted state
// with the Registry. It exists because a Manager only reads state.json on
// construction (loadState) — it does NOT touch the Registry until Enable is
// called — and the process-wide Manager (DefaultManager) is built lazily, on
// the first admin API call. Left unaddressed, a server restart would leave
// every previously-enabled plugin's driver missing from the Registry (state
// still says enabled=true, but nothing registered it), so any storage backed
// by that plugin fails to load with "no driver named" until an admin happens
// to hit the plugin API and re-Enable it by hand.
//
// Callers (bootstrap.LoadPlugins) must run this BEFORE storages are loaded so
// plugin-backed storage drivers are already in the Registry when
// bootstrap.LoadStorages looks them up.
//
// A single plugin failing to (re-)register — missing binary, a manifest that
// no longer parses, or a trust tier the current plugin_policy now forbids —
// is logged and skipped; it must never abort the rest of the plugins or boot.
// The returned error, if non-nil, aggregates every failure for the caller to
// log as a warning; it is intentionally not fatal.
func (m *Manager) LoadEnabled() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []string
	for _, ps := range m.state.Plugins {
		if !ps.Enabled {
			continue
		}
		if err := m.registerActive(ps); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ps.ID, err))
			continue
		}
		// Restart property: a FeatureBundle's expansion (extraKinds indexes)
		// lives on the in-memory Entry, not state.json — registerActive just
		// created a brand-new Entry with none, so it must be re-expanded here
		// exactly as Enable does, or a bundle's contributed points stay
		// unreachable across a server restart.
		m.expandFeatureBundleIfDeclared(ps.ID)
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("extension: failed to load %d enabled plugin(s): %s", len(errs), strings.Join(errs, "; "))
}

// Disable removes the plugin from the Registry and marks it disabled. State and
// on-disk versions are kept.
func (m *Manager) Disable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return fmt.Errorf("extension %q: not installed", id)
	}
	// Stop the supervised BackgroundService (cancels Run → kills the frpc
	// subprocess) BEFORE deregistering, while the entry's kind is still
	// resolvable. StopService is a no-op if nothing is running.
	if m.sup != nil && m.isBackgroundService(id) {
		m.sup.StopService(id)
	}
	// Tear down a FeatureBundle's F1/F2 wiring (any in-process instances it
	// registered) before the entry disappears. For a gRPC bundle this is a
	// harmless no-op (expandGRPCFeatureBundle never registers one); the
	// registry-side extraKinds indexes are dropped by Deregister just below
	// regardless.
	if m.isFeatureBundle(id) {
		CollapseFeatureBundle(id)
	}
	m.registry.Deregister(id)
	ps.Enabled = false
	ps.UpdatedAt = time.Now()
	return m.persistState()
}

// Rollback switches the active version back to the previously-active one (kept
// from the last Update) and swaps the rollback pointer, so a second Rollback
// returns to where it started. Both versions stay on disk.
func (m *Manager) Rollback(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.state.Plugins[id]
	if !ok {
		return fmt.Errorf("extension %q: not installed", id)
	}
	if ps.PreviousVersion == "" {
		return fmt.Errorf("extension %q: no previous version to roll back to", id)
	}
	ps.ActiveVersion, ps.PreviousVersion = ps.PreviousVersion, ps.ActiveVersion
	ps.UpdatedAt = time.Now()
	if ps.Enabled {
		// If an enabled BackgroundService is rolling to a different version, its
		// running subprocess is bound to the old binary — stop it, re-register the
		// new active version, then start it fresh so Run execs the rolled-to binary.
		wasBg := m.sup != nil && m.isBackgroundService(id)
		if wasBg {
			m.sup.StopService(id)
		}
		if err := m.registerActive(ps); err != nil {
			return err
		}
		if m.sup != nil && m.isBackgroundService(id) {
			_ = m.sup.StartService(id)
		}
		// registerActive replaced the entry; re-expand a FeatureBundle the
		// same way Enable/LoadEnabled/Update do.
		m.expandFeatureBundleIfDeclared(id)
	}
	return m.persistState()
}

// Uninstall deregisters the plugin and removes all its on-disk versions and its
// state entry.
func (m *Manager) Uninstall(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.state.Plugins[id]; !ok {
		return fmt.Errorf("extension %q: not installed", id)
	}
	// Stop the supervised BackgroundService (kills its subprocess) before tearing
	// the plugin down, while its kind is still resolvable from the registry.
	if m.sup != nil && m.isBackgroundService(id) {
		m.sup.StopService(id)
	}
	if m.isFeatureBundle(id) {
		CollapseFeatureBundle(id)
	}
	m.registry.Deregister(id)
	if err := os.RemoveAll(filepath.Join(m.dir, id)); err != nil {
		return err
	}
	delete(m.state.Plugins, id)
	return m.persistState()
}

// registerActive (re)registers the plugin's active version with the Registry in
// the execution mode its trust tier permits. It RE-GATES at load time (defense
// in depth): re-classify the tier, re-check the current admin policy, and
// re-derive the mode, so enabling a plugin that a tightened policy now forbids —
// or whose state file was edited — fails here too, not just at install. Caller
// holds m.mu.
func (m *Manager) registerActive(ps *PluginState) error {
	dir := m.versionDir(ps.ID, ps.ActiveVersion)
	man, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	tier, mode, err := m.gate(dir, man)
	if err != nil {
		return err
	}
	path, err := findBinary(dir)
	if err != nil {
		return err
	}
	ps.Tier = tier.String()
	ps.ExecModeName = mode.String()
	if ps.Name == nil {
		ps.Name = man.Name // backfill for plugins installed before Name existed
	}
	m.registry.Deregister(ps.ID) // replace any prior registration
	return m.registry.RegisterPluginMode(man, path, mode)
}

// gate classifies dir's package and returns the trust tier and execution mode, or
// an error refusing the install/load. It is the single point where trust-tier
// and admin-policy rules are applied, so install and enable enforce identical
// rules. A presented-but-invalid signature is rejected outright regardless of
// policy — never downgraded to an unsigned install.
//
// It also runs the STATELESS half of the public_prefix conflict check
// (CheckReservedPrefix, apiroute.go): gate is the one call both installVersion
// (Install/Update) and registerActive (Enable/LoadEnabled/Update's
// re-registration/Rollback) already pass through, so a plugin requesting a
// reserved top-level prefix ("api", "d", "ext-public", …) is refused at
// install time too, not only when an admin later enables it. The STATEFUL half
// — cross-plugin mutual exclusion over the prefix→pluginID table — is a
// registration-time concern (it actually claims the resource) and lives in
// Registry.RegisterPluginMode/Register instead, so a merely-installed
// (disabled) plugin never occupies a prefix another plugin might also want to
// install with.
func (m *Manager) gate(dir string, man ext.Manifest) (Tier, ExecMode, error) {
	tier, _, sigErr := m.tiered.DetermineTier(dir, man)
	if sigErr != nil {
		return tier, ExecInProcess, sigErr
	}
	if !m.policy().Allows(tier) {
		return tier, ExecInProcess, fmt.Errorf(
			"extension %q: trust tier %s is not permitted by plugin_policy=%s", man.ID, tier, m.policy())
	}
	if p := man.Contributions.PublicPrefix; p != "" {
		if err := CheckReservedPrefix(p); err != nil {
			return tier, ExecInProcess, fmt.Errorf("extension %q: %w", man.ID, err)
		}
	}
	hasNative, _ := detectArtifacts(dir)
	mode, ok := SelectExecMode(tier, hasNative)
	if !ok {
		return tier, ExecInProcess, fmt.Errorf(
			"extension %q: package ships no runnable binary; refused", man.ID)
	}
	return tier, mode, nil
}

func primaryKind(m ext.Manifest) ext.Kind {
	if len(m.Kinds) > 0 {
		return m.Kinds[0]
	}
	return ""
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// isPackageMeta reports whether name is a package metadata file (not runnable
// code).
func isPackageMeta(name string) bool {
	return name == ManifestFile || name == ChecksumsFile || name == SignatureFile
}

// detectArtifacts reports what runnable code a package version dir ships: a
// native binary is any non-metadata, non-.wasm file. Stray .wasm modules from
// the pre-parity era are ignored — they are not runnable.
func detectArtifacts(dir string) (hasNative, hasWASM bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isPackageMeta(name) || strings.HasSuffix(name, ".wasm") {
			continue
		}
		hasNative = true
	}
	return hasNative, false
}

// findBinary locates the plugin's native executable in an installed version dir:
// any non-metadata, non-.wasm file. Prefers one literally named "plugin" (or
// "plugin.exe"); otherwise requires exactly one candidate.
func findBinary(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isPackageMeta(name) || strings.HasSuffix(name, ".wasm") {
			continue
		}
		if name == "plugin" || name == "plugin.exe" {
			return filepath.Join(dir, name), nil
		}
		candidates = append(candidates, name)
	}
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("extension: no binary found in %s", dir)
	case 1:
		return filepath.Join(dir, candidates[0]), nil
	default:
		return "", fmt.Errorf("extension: ambiguous binary in %s: %v (name one 'plugin')", dir, candidates)
	}
}

// FindBinary exposes findBinary for out-of-package tooling (plugin validate).
func FindBinary(dir string) (string, error) { return findBinary(dir) }

// DetectArtifacts exposes detectArtifacts for out-of-package tooling.
func DetectArtifacts(dir string) (hasNative, hasWASM bool) { return detectArtifacts(dir) }

// extractTarGz unpacks a .tar.gz into destDir, rejecting entries whose path
// escapes destDir (zip-slip). Only regular files and dirs are extracted.
func extractTarGz(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("extension: gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("extension: tar: %w", err)
		}
		clean := filepath.Clean(hdr.Name)
		if clean == "." {
			continue
		}
		if strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." || filepath.IsAbs(clean) {
			return fmt.Errorf("extension: unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		rel, err := filepath.Rel(destDir, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("extension: unsafe path in archive: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // size bounded by trusted-after-verify staging; verified before use
				out.Close()
				return err
			}
			out.Close()
		default:
			// skip symlinks/devices/etc — a plugin package is files + dirs only
		}
	}
	return nil
}
