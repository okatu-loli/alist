package extension

import (
	"path/filepath"
	"sync"

	"github.com/alist-org/alist/v3/cmd/flags"
)

// The process-wide lifecycle manager the admin API drives. Built lazily so tests
// that construct their own Manager (with a temp dir + custom verifier) are not
// affected, and so no plugin dir is touched until the first admin call.
var (
	defaultManager     *Manager
	defaultManagerErr  error
	defaultManagerOnce sync.Once
)

// PluginsDir returns <DataDir>/plugins — the directory DefaultManager roots
// itself at. A PURE path computation with no side effects (never touches
// disk, never initializes the plugin lifecycle machinery): a caller that
// only needs this path — not the full Manager — should call this directly
// rather than DefaultManager, so it isn't coupled to (or forced to pay the
// cost of) the Manager singleton's heavier first-call initialization (trust
// verification, policy wiring, background service supervision, ...). G4 §1:
// internal/op's storage Addition encryption resolves the shared secrets key
// this way, precisely to stay decoupled from the plugin Manager.
func PluginsDir() string {
	return filepath.Join(flags.DataDir, "plugins")
}

// DefaultManager returns the process-wide plugin Manager, rooted at
// PluginsDir() and gated on the trust tiers (official root + imported
// plugin sources) and the admin plugin_policy (read live via CurrentPolicy,
// wired by the server layer through SetPolicyProvider — internal/extension may
// not import internal/setting without an import cycle).
func DefaultManager() (*Manager, error) {
	defaultManagerOnce.Do(func() {
		dir := PluginsDir()
		defaultManager, defaultManagerErr = NewManagerWithTrust(
			dir, DefaultRegistry, DefaultTieredVerifier(), CurrentPolicy)
		if defaultManagerErr == nil {
			// Drive the process-wide BackgroundService supervisor on
			// enable/disable/uninstall/rollback so a Kind=BackgroundService disk
			// plugin (e.g. the external FRP plugin) actually starts/stops its
			// supervised Run — mirroring how BuiltinManager drives it for built-in
			// services. Both are bound to DefaultRegistry, so the ids line up.
			defaultManager.SetSupervisor(DefaultServiceSupervisor)
			// Wire the dispatch gate to the persisted consent Grant (F-consent):
			// GrantsCapability now reads what the admin APPROVED, not what the
			// Manifest declares. An installed plugin's granted host_apis are
			// authoritative (a declared-but-ungranted capability is denied); an
			// unknown id (a built-in) falls through to the gate's built-in rule.
			SetGrantOverride(defaultManager.GrantedHostAPIs)
		}
	})
	return defaultManager, defaultManagerErr
}
