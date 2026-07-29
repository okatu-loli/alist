package extension

import (
	"fmt"
	"sort"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// This file is the runtime enable/disable seam for built-in extensions (design
// §2.3/§2.4). A built-in (labels/SSO/passkey feature bundles, the FRP background
// service) registers enabled at boot; an admin may turn it OFF in the extension
// manager, and that choice must survive a restart. Turning a built-in off must:
//   - flip its registry entry disabled (so ByKind/EnabledEntries/AggregateContri-
//     butions stop reaching it — the contribution/dispatch teardown is already
//     zero-cost, done by the enabled filter);
//   - for a BackgroundService, stop the supervised service (StopService cancels
//     its Run — the FRP tunnel actually goes down);
//   - persist the id in the disabled set so ApplyDisabledBuiltins re-applies it
//     after registration on the next boot.
//
// internal/extension is a low-level package that MUST NOT import internal/op or
// internal/setting (op already imports extension — that would cycle; see
// default.go). The persisted set therefore lives behind DisabledStore, an
// interface the boot layer (internal/bootstrap) implements over the
// conf.DisabledBuiltinExtensions setting and injects via SetDisabledStore. Until
// wired — and in unit tests — an in-memory store keeps the manager fully
// functional and testable without a DB.

// DisabledStore persists the set of disabled built-in extension ids across
// restarts. The setting-backed implementation is injected from internal/bootstrap
// so this package stays free of an internal/op import cycle.
type DisabledStore interface {
	// Load returns the currently-persisted disabled id set (order not required).
	Load() ([]string, error)
	// Save replaces the persisted disabled id set.
	Save(ids []string) error
}

// inMemoryDisabledStore is the default DisabledStore before internal/bootstrap
// wires the setting-backed one. It keeps the manager usable (and unit tests
// self-contained) without persistence to disk.
type inMemoryDisabledStore struct {
	mu  sync.Mutex
	ids []string
}

func (s *inMemoryDisabledStore) Load() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...), nil
}

func (s *inMemoryDisabledStore) Save(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append([]string(nil), ids...)
	return nil
}

// BuiltinManager orchestrates the three legs of a built-in enable/disable
// (registry flip, BackgroundService start/stop, persistence) over a Registry, a
// ServiceSupervisor, and a DisabledStore. It is injectable so tests can drive it
// with fakes; the process-wide DefaultBuiltinManager binds it to
// DefaultRegistry/DefaultServiceSupervisor and the boot-injected store.
type BuiltinManager struct {
	mu    sync.Mutex
	r     *Registry
	sup   *ServiceSupervisor
	store DisabledStore
}

// NewBuiltinManager builds a manager over r, sup, and store. sup may be nil for a
// registry with no BackgroundService built-ins (feature-bundle-only tests).
func NewBuiltinManager(r *Registry, sup *ServiceSupervisor, store DisabledStore) *BuiltinManager {
	if store == nil {
		store = &inMemoryDisabledStore{}
	}
	return &BuiltinManager{r: r, sup: sup, store: store}
}

// SetStore swaps the persistence backend. Called once at boot to inject the
// setting-backed store; safe against concurrent enable/disable.
func (m *BuiltinManager) SetStore(store DisabledStore) {
	if store == nil {
		return
	}
	m.mu.Lock()
	m.store = store
	m.mu.Unlock()
}

// requireBuiltin returns the entry for id iff it is a registered built-in,
// erroring otherwise so a disk-plugin or unknown id never flows through the
// built-in enable/disable path (nor mutates the persisted set).
func (m *BuiltinManager) requireBuiltin(id string) (*Entry, error) {
	e, ok := m.r.Get(id)
	if !ok {
		return nil, fmt.Errorf("extension %q not found", id)
	}
	if !e.Builtin() {
		return nil, fmt.Errorf("extension %q is not a built-in", id)
	}
	return e, nil
}

// DisableBuiltin turns a built-in off: it validates the id is a built-in (else
// errors without any state change), disables the registry entry, stops the
// supervised service if it is a BackgroundService, and records the id in the
// persisted disabled set.
func (m *BuiltinManager) DisableBuiltin(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.requireBuiltin(id)
	if err != nil {
		return err
	}
	// DeclaresKind (Manifest truth source), not HasKind: see registry.go's
	// doc — BackgroundService has no FeatureBundle expansion path, so a
	// built-in that legitimately declares it alongside another primary Kind
	// must still be recognized here.
	isBg := e.DeclaresKind(ext.KindBackgroundService)
	if err := m.r.Disable(id); err != nil {
		return err
	}
	if isBg && m.sup != nil {
		m.sup.StopService(id)
	}
	return m.persistLocked(id, true)
}

// EnableBuiltin turns a built-in back on: it validates the id is a built-in,
// enables the registry entry, starts the supervised service if it is a
// BackgroundService, and removes the id from the persisted disabled set.
func (m *BuiltinManager) EnableBuiltin(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.requireBuiltin(id)
	if err != nil {
		return err
	}
	// DeclaresKind, not HasKind — see DisableBuiltin above.
	isBg := e.DeclaresKind(ext.KindBackgroundService)
	if err := m.r.Enable(id); err != nil {
		return err
	}
	if isBg && m.sup != nil {
		_ = m.sup.StartService(id)
	}
	return m.persistLocked(id, false)
}

// ApplyDisabledBuiltins re-applies the persisted disabled set after built-in
// registration: every listed id that resolves to a registered built-in is
// disabled (so the supervisor never starts it and its contributions are not
// aggregated). Ids that are not registered built-ins (a stale id, or a
// coincidental disk-plugin id) are ignored. It only reads persistence; it never
// writes. Boot calls it after all Init* register the built-ins and before the
// supervisor starts.
func (m *BuiltinManager) ApplyDisabledBuiltins() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids, err := m.store.Load()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if e, ok := m.r.Get(id); ok && e.Builtin() {
			_ = m.r.Disable(id)
		}
	}
	return nil
}

// persistLocked loads the disabled set, adds (disabled) or removes (enabled) id,
// and saves it back deduped + id-sorted for a stable JSON array. Callers hold
// m.mu, so the read-modify-write is atomic against concurrent enable/disable.
func (m *BuiltinManager) persistLocked(id string, disabled bool) error {
	ids, err := m.store.Load()
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(ids)+1)
	for _, x := range ids {
		set[x] = true
	}
	if disabled {
		set[id] = true
	} else {
		delete(set, id)
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return m.store.Save(out)
}

// DefaultBuiltinManager is the process-wide built-in manager, bound to
// DefaultRegistry and DefaultServiceSupervisor. Its store starts in-memory and is
// replaced by the setting-backed one at boot (bootstrap.InitBuiltinManage calls
// SetDisabledStore). The package-level EnableBuiltin/DisableBuiltin/
// ApplyDisabledBuiltins wrappers below delegate here; the admin handlers and boot
// wiring call those.
var DefaultBuiltinManager = NewBuiltinManager(DefaultRegistry, DefaultServiceSupervisor, &inMemoryDisabledStore{})

// SetDisabledStore injects the persistence backend into DefaultBuiltinManager.
func SetDisabledStore(store DisabledStore) { DefaultBuiltinManager.SetStore(store) }

// EnableBuiltin enables the built-in id on the process-wide manager.
func EnableBuiltin(id string) error { return DefaultBuiltinManager.EnableBuiltin(id) }

// DisableBuiltin disables the built-in id on the process-wide manager.
func DisableBuiltin(id string) error { return DefaultBuiltinManager.DisableBuiltin(id) }

// ApplyDisabledBuiltins re-applies the persisted disabled set on the process-wide
// manager (boot).
func ApplyDisabledBuiltins() error { return DefaultBuiltinManager.ApplyDisabledBuiltins() }
