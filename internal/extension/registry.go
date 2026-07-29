package extension

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
)

// InstanceMode is how the registry instantiates an extension (9.0 §3).
//
//   - Constructor: one instance per mounted storage (StorageDriver:
//     constructor-per-storage).
//   - Singleton: one shared instance for the whole process (offline-download /
//     archive / search / notification kinds, reserved).
type InstanceMode int

const (
	InstanceModeConstructor InstanceMode = iota
	InstanceModeSingleton
)

func (m InstanceMode) String() string {
	switch m {
	case InstanceModeConstructor:
		return "constructor"
	case InstanceModeSingleton:
		return "singleton"
	default:
		return "unknown"
	}
}

// DriverConstructor builds a fresh legacy storage driver instance. Storage
// drivers are constructor-mode: the host constructs one per mounted storage.
type DriverConstructor = func() driver.Driver

// CapabilityMismatchError is returned when a registration declares capabilities
// its implementation does not provide (9.0 §4 loud failure).
type CapabilityMismatchError struct {
	Missing []ext.Capability
}

func (e *CapabilityMismatchError) Error() string {
	return fmt.Sprintf("declared capabilities not implemented: %v", e.Missing)
}

// Entry is one registered extension in the Kind-keyed registry.
type Entry struct {
	Manifest ext.Manifest
	Kind     ext.Kind
	Mode     InstanceMode

	// extraKinds are additional Kind buckets this entry is indexed under beyond
	// its primary Kind, added by FeatureBundle expansion (AddKindIndex) so the
	// composite is reachable through each F1 wiring point it contributes.
	extraKinds []ext.Kind

	// implemented is the actually-supported capability set (the validation
	// target for Manifest.Capabilities).
	implemented []ext.Capability

	// constructor and info are set for built-in storage drivers (the legacy
	// bridge path used in 9.1). nil/zero for other kinds.
	constructor DriverConstructor
	info        driver.Info

	// bundle is a plugin's loaded per-locale field-translation map (Task ②:
	// per-plugin bundled locale files), keyed by locale tag then by
	// "field.<name>.label"/"field.<name>.help". Loaded once at registration
	// from the plugin's version dir (LoadPluginLocales) and reused by every
	// per-locale BuildPluginDriverInfo call — StorageDriverInfoMapLocale reads
	// it to localize this plugin's Additional items on each request. Empty for
	// built-in entries (constructor != nil) and for plugins that ship no
	// i18n/ directory.
	bundle map[string]map[string]string

	// singleton is the shared instance for singleton-mode extensions. Reserved:
	// no live singleton kinds in 9.1.
	singleton ext.Extension

	// execMode is the execution mode (in-process built-in vs gRPC subprocess),
	// added in 9.2. Zero value is ExecInProcess.
	execMode ExecMode

	// binPath is the plugin binary launched for ExecGRPC entries (installed
	// plugins, 9.3). Empty for built-ins.
	binPath string

	enabled bool

	// builtin marks a host-registered, trusted built-in extension (labels/SSO/
	// passkey via RegisterBuiltinFeatureBundle, FRP via
	// RegisterBuiltinBackgroundService). Built-ins surface in the extension
	// manager as enable/disable-able items that cannot be uninstalled, and are
	// listed even while disabled (BuiltinEntries). Plugins from disk are
	// builtin==false.
	builtin bool
}

// Builtin reports whether the entry is a host-registered trusted built-in
// (registered through RegisterBuiltinFeatureBundle /
// RegisterBuiltinBackgroundService) rather than an installed disk plugin.
func (e *Entry) Builtin() bool { return e.builtin }

// BinPath returns the plugin binary path for a gRPC-mode entry (empty for
// built-ins).
func (e *Entry) BinPath() string { return e.binPath }

// ID returns the extension's identifier.
func (e *Entry) ID() string { return e.Manifest.ID }

// Enabled reports whether the entry is currently enabled.
func (e *Entry) Enabled() bool { return e.enabled }

// Capabilities returns the entry's declared capabilities.
func (e *Entry) Capabilities() []ext.Capability { return e.Manifest.Capabilities }

// HasKind reports whether the entry participates in Kind k — either as its
// primary Kind or as an extra kind index added by FeatureBundle expansion
// (AddKindIndex). Dispatch paths use this instead of `e.Kind == k` so a
// composite bundle whose primary Kind is FeatureBundle is still reachable through
// the APIRoute / EventHook wiring it contributes.
func (e *Entry) HasKind(k ext.Kind) bool {
	if e.Kind == k {
		return true
	}
	for _, extra := range e.extraKinds {
		if extra == k {
			return true
		}
	}
	return false
}

// DeclaresKind reports whether the entry's Manifest declares Kind k in its
// Kinds list — the SDK-documented truth source for "did this extension
// declare Kind k" (an extension "declares one or more Kinds it implements",
// ext/manifest.go). Unlike HasKind, this does not depend on registry
// indexing: RegisterPluginMode only indexes an entry under its PRIMARY Kind
// (Manifest.Kinds[0]), so a plugin that legitimately declares several Kinds
// on one manifest (e.g. Kinds: ["access_protocol", "api_route"]) is wrongly
// missed by HasKind for every Kind past the first — HasKind's extraKinds is
// populated solely by FeatureBundle expansion (featurebundle.go
// AddKindIndex), never by a plain multi-Kind declaration. A dispatch gate
// whose question is "did the plugin declare this Kind" (rather than "is this
// entry reachable through the registry's Kind index") should call
// DeclaresKind, not HasKind. Exported so callers outside this package (e.g.
// server/handles) can ask the same question without reaching into the
// package-private hasKind(Manifest, Kind) helper.
func (e *Entry) DeclaresKind(k ext.Kind) bool {
	return hasKind(e.Manifest, k)
}

// Registry is the unified, Kind-keyed extension registry (9.0 §3). It subsumes
// the v3 driverMap: built-in storage drivers register here as
// Kind=StorageDriver, constructor mode.
type Registry struct {
	mu     sync.RWMutex
	byID   map[string]*Entry
	byKind map[ext.Kind]map[string]*Entry

	// publicPrefixes is the cross-plugin mutual-exclusion table for
	// Contributions.PublicPrefix (design §3 PublicRouter): normalized,
	// lowercased top-level path segment -> the id of the extension that
	// claimed it. Populated by claimPublicPrefixLocked at registration
	// (RegisterPluginMode/Register) and released by Deregister, so a claim
	// only ever outlives the entry that made it for as long as the entry
	// stays registered (a soft Disable, which merely flips e.enabled, keeps
	// the claim — see Registry.Disable's doc comment).
	publicPrefixes map[string]string
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byID:           make(map[string]*Entry),
		byKind:         make(map[ext.Kind]map[string]*Entry),
		publicPrefixes: make(map[string]string),
	}
}

// DefaultRegistry is the process-wide registry the core uses. op registration
// delegates here.
var DefaultRegistry = NewRegistry()

// guardReservedOverwrite is the core defense-in-depth check protecting the
// reserved built-in id namespace: a plugin (newBuiltin==false) must never
// overwrite an existing trusted built-in entry (existing.builtin==true) with the
// same id — doing so would silently re-enable a feature an admin had disabled and
// let a third-party plugin impersonate a trusted built-in id. Callers hold r.mu.
// A built-in re-registering its own id at boot (builtin==true both sides) and a
// plugin update (builtin==false both sides) are both unaffected.
func guardReservedOverwrite(existing *Entry, id string, newBuiltin bool) error {
	if existing != nil && existing.builtin && !newBuiltin {
		return fmt.Errorf("extension %q: the id is a reserved built-in and cannot be overwritten by a plugin", id)
	}
	return nil
}

// claimPublicPrefixLocked enforces the cross-plugin mutual-exclusion half of
// the public_prefix conflict check (the reserved-word half,
// CheckReservedPrefix, already ran at install/enable time — lifecycle.go's
// gate). An empty prefix is a no-op (nothing to claim). Re-claiming the SAME
// prefix by the SAME id is idempotent (a re-registration: Update/Rollback/
// LoadEnabled all Deregister then re-register the same entry). A prefix
// already claimed by a DIFFERENT id is refused with a readable conflict
// error, and the caller (Register/RegisterPluginMode) must not register the
// entry in that case. Caller holds r.mu.
func (r *Registry) claimPublicPrefixLocked(id, prefix string) error {
	if prefix == "" {
		return nil
	}
	if err := CheckReservedPrefix(prefix); err != nil {
		return err
	}
	seg, err := normalizePrefix(prefix)
	if err != nil {
		return err
	}
	key := strings.ToLower(seg[0])
	if owner, ok := r.publicPrefixes[key]; ok && owner != id {
		return fmt.Errorf("extension %q: public prefix %q is already claimed by %q", id, prefix, owner)
	}
	if r.publicPrefixes == nil {
		r.publicPrefixes = map[string]string{}
	}
	r.publicPrefixes[key] = id
	return nil
}

// releasePublicPrefixLocked releases every prefix id currently claims (a
// no-op if it holds none). Caller holds r.mu.
func (r *Registry) releasePublicPrefixLocked(id string) {
	for k, owner := range r.publicPrefixes {
		if owner == id {
			delete(r.publicPrefixes, k)
		}
	}
}

// ResolvePublicPrefix returns the extension id that has claimed the top-level
// path segment seg (case-insensitive), and whether one is claimed. This is
// the read side the NoRoute prefix dispatcher (server/handles.
// ExtPublicPrefixDispatch) consults for every unmatched request's first path
// segment; a miss must fall through unchanged to the SPA fallback.
func (r *Registry) ResolvePublicPrefix(seg string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.publicPrefixes[strings.ToLower(seg)]
	return id, ok
}

// Register validates and inserts an entry. It fails loudly if the entry declares
// capabilities its implementation does not provide (9.0 §4).
func (r *Registry) Register(e *Entry) error {
	if e.Manifest.ID == "" {
		return fmt.Errorf("extension: empty manifest id")
	}
	if err := validateCapabilities(e.Manifest.Capabilities, e.implemented); err != nil {
		return fmt.Errorf("extension %q: %w", e.Manifest.ID, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Review fix (round 1, TOCTOU): validated INSIDE the lock, using the
	// lock-free getLocked, not before it. Checking with r.Get (its own RLock)
	// BEFORE taking r.mu left a window where a concurrent Deregister of the
	// referenced provider — running between this check and the r.byID insert
	// below — could let an entry whose oauth step now references a REMOVED
	// provider slip in registered. Validating and inserting under the SAME
	// critical section closes that window (mirrors Enable's already-correct
	// pattern).
	if err := validateWizardOAuthProviders(e.Manifest, r.getLocked); err != nil {
		return err
	}
	if err := guardReservedOverwrite(r.byID[e.Manifest.ID], e.Manifest.ID, e.builtin); err != nil {
		return err
	}
	if err := r.claimPublicPrefixLocked(e.Manifest.ID, e.Manifest.Contributions.PublicPrefix); err != nil {
		return err
	}
	r.byID[e.Manifest.ID] = e
	km := r.byKind[e.Kind]
	if km == nil {
		km = make(map[string]*Entry)
		r.byKind[e.Kind] = km
	}
	km[e.Manifest.ID] = e
	return nil
}

// RegisterPlugin inserts an installed plugin (9.3 lifecycle) as a gRPC-mode
// entry. Unlike RegisterStorageDriver there is no in-process constructor: the
// entry records the manifest, its primary Kind, the binary path, and ExecGRPC
// mode. Wiring op dispatch to launch the subprocess is exercised by the 9.2
// executor; 9.3 only manages the entry's registration/enable state. The entry
// starts enabled (callers register it as part of Enable).
func (r *Registry) RegisterPlugin(m ext.Manifest, binPath string) error {
	return r.RegisterPluginMode(m, binPath, ExecGRPC)
}

// RegisterPluginMode inserts an installed plugin in an explicit execution mode.
// Since plugin parity (2026-07-14) installed plugins always run ExecGRPC (path =
// native binary); the mode parameter remains for built-in callers and tests.
func (r *Registry) RegisterPluginMode(m ext.Manifest, path string, mode ExecMode) error {
	if m.ID == "" {
		return fmt.Errorf("extension: empty manifest id")
	}
	// Reserved-prefix rejection (plugin path only). The `builtin.` namespace is
	// owned by host-registered trusted built-ins (registered through Register via
	// RegisterBuiltinExtension/RegisterBuiltinFeatureBundle); a disk/third-party
	// plugin must not claim it. Rejecting here — not in ValidateManifest, which is
	// NOT on the built-in Register path but WOULD reject the built-ins if it were —
	// gives the admin a clean error instead of the overwrite guard's silent no-op.
	if strings.HasPrefix(m.ID, "builtin.") {
		return fmt.Errorf("extension %q: the 'builtin.' namespace is reserved for built-in features", m.ID)
	}
	if len(m.Kinds) == 0 {
		return fmt.Errorf("extension %q: no kinds", m.ID)
	}
	kind := m.Kinds[0]
	// A plugin's bundled locale files live at <version dir>/i18n/*.json,
	// alongside its binary (path is <version dir>/<binary>, see
	// lifecycle.go's findBinary) — so the binary's directory IS the version
	// dir to load locales from. A path with no directory component (bare
	// filename, seen only in unit tests that don't stage a real install)
	// resolves to ".", and LoadPluginLocales returns an empty map when that
	// has no i18n/ subdir, exactly like a plugin that ships no locales.
	bundle := LoadPluginLocales(filepath.Dir(path))
	entry := &Entry{
		Manifest:    m,
		Kind:        kind,
		Mode:        InstanceModeConstructor,
		implemented: m.Capabilities, // ext-native plugins declare == implement; gRPC dispatch reads the declaration
		info:        BuildPluginDriverInfo(m, DefaultLocale, bundle),
		bundle:      bundle,
		execMode:    mode,
		binPath:     path,
		enabled:     true,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Review fix (round 1, TOCTOU): validated INSIDE the lock via getLocked —
	// see Register's identical fix for why a pre-lock r.Get check left a race
	// window against a concurrent Deregister of the referenced provider.
	if err := validateWizardOAuthProviders(m, r.getLocked); err != nil {
		return err
	}
	// Defense in depth: even though the reserved-prefix check above already
	// rejects `builtin.` ids, guard the direct byID write the same way Register
	// does so no plugin (builtin==false) can ever overwrite a trusted built-in.
	if err := guardReservedOverwrite(r.byID[m.ID], m.ID, entry.builtin); err != nil {
		return err
	}
	if err := r.claimPublicPrefixLocked(m.ID, m.Contributions.PublicPrefix); err != nil {
		return err
	}
	r.byID[m.ID] = entry
	km := r.byKind[kind]
	if km == nil {
		km = make(map[string]*Entry)
		r.byKind[kind] = km
	}
	km[m.ID] = entry
	return nil
}

// Deregister removes an entry entirely (9.3 uninstall/disable-then-remove). A
// missing id is a no-op. It clears every kind bucket the entry appears in,
// including the extra kinds a FeatureBundle expansion added (AddKindIndex).
func (r *Registry) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return
	}
	delete(r.byID, id)
	if km := r.byKind[e.Kind]; km != nil {
		delete(km, id)
	}
	for _, k := range e.extraKinds {
		if km := r.byKind[k]; km != nil {
			delete(km, id)
		}
	}
	r.releasePublicPrefixLocked(id)
}

// AddKindIndex indexes an already-registered entry under an ADDITIONAL Kind
// bucket (beyond its primary Kind) and records it in the entry's extraKinds. It
// is how FeatureBundle expansion makes a composite reachable through each F1
// wiring point it contributes (ByKind(EventHook) etc. then find it, HasKind
// reports true). A no-op if the entry is already indexed under k. Errors if the
// id is unknown.
func (r *Registry) AddKindIndex(id string, k ext.Kind) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("extension %q not found", id)
	}
	if e.Kind == k {
		return nil
	}
	for _, existing := range e.extraKinds {
		if existing == k {
			return nil
		}
	}
	e.extraKinds = append(e.extraKinds, k)
	km := r.byKind[k]
	if km == nil {
		km = make(map[string]*Entry)
		r.byKind[k] = km
	}
	km[id] = e
	return nil
}

// Get returns an entry by id.
func (r *Registry) Get(id string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	return e, ok
}

// EnabledEntries returns every enabled entry, id-sorted for deterministic
// output. It is the source for the contributions aggregation (Step 10): an
// entry is one extension regardless of how many Kinds it declares, so this
// dedups across the kind index by iterating byID.
func (r *Registry) EnabledEntries() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Entry
	for _, e := range r.byID {
		if e.enabled {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out
}

// BuiltinEntries returns every built-in entry (builtin==true) regardless of
// enabled state, id-sorted. It is the source for the extension manager's
// built-in section: a disabled built-in is excluded from EnabledEntries/ByKind
// but must still be listed so it can be re-enabled, so this deliberately does
// NOT filter on enabled.
func (r *Registry) BuiltinEntries() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Entry
	for _, e := range r.byID {
		if e.builtin {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out
}

// ByKind returns the enabled entries of a Kind.
func (r *Registry) ByKind(k ext.Kind) []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Entry
	for _, e := range r.byKind[k] {
		if e.enabled {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out
}

// getLocked reads r.byID directly with NO locking of its own. It exists solely
// so a method that ALREADY holds r.mu (Register, RegisterPluginMode, Enable —
// all three validate wizard oauth providers under the lock, review round 1)
// can pass a registry lookup into validateWizardOAuthProviders without
// deadlocking against Go's non-reentrant sync.RWMutex — unlike the exported
// Get, which takes its own RLock and must only be called when the caller
// holds no lock at all.
func (r *Registry) getLocked(id string) (*Entry, bool) {
	e, ok := r.byID[id]
	return e, ok
}

// Enable enables an entry by id. Re-validates that any wizard oauth step it
// declares still references an existing Kind=AuthProvider extension (the
// referenced provider may have been uninstalled while this entry sat
// disabled) — the same check Register/RegisterPluginMode run at initial
// registration.
func (r *Registry) Enable(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("extension %q not found", id)
	}
	if err := validateWizardOAuthProviders(e.Manifest, r.getLocked); err != nil {
		return err
	}
	e.enabled = true
	return nil
}

// Disable disables an entry by id. A disabled entry is excluded from lookups.
func (r *Registry) Disable(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("extension %q not found", id)
	}
	e.enabled = false
	return nil
}
