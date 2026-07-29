package extension

import (
	"fmt"

	ext "github.com/AlistGo/alist-ext"
)

// This file is the host consumption of Kind=Feature (Project A-2): turning a
// natively-wired core feature (offline download / file search / sharing / SSO
// login) into a manageable built-in extension WITHOUT re-plumbing the feature
// itself. Unlike RegisterBuiltinFeatureBundle, there is no instance to resolve
// and nothing to expand onto an F1/F2 wiring point — the feature's routes and
// handlers already exist in core. RegisterBuiltinExtension only inserts a plain
// registry entry so the feature gains an identity that:
//
//   - lists in the extension manager (BuiltinEntries) as an enable/disable-able,
//     un-uninstallable item, exactly like labels/frp;
//   - toggles through EnableBuiltin/DisableBuiltin with the same persistence;
//   - flows into AggregateContributions while enabled (so the frontend can gate
//     its entry points on the feature's presence);
//   - backs a RequireExtensionEnabled route gate — the disabled entry makes the
//     gate return 403, the real boundary.

// RegisterBuiltinExtension installs a plain, trusted built-in Kind=Feature entry
// carrying m into r. It validates the manifest has an id and at least one Kind,
// then registers an enabled, builtin entry under m.Kinds[0]. No instance is
// created and no FeatureBundle expansion happens — the feature's own routes and
// handlers already live in core; this entry only makes the feature manageable
// and gate-able. A duplicate id or empty manifest is a loud error: a half-wired
// built-in must not register.
func RegisterBuiltinExtension(r *Registry, m ext.Manifest) error {
	if m.ID == "" {
		return fmt.Errorf("extension: built-in feature extension with empty manifest id")
	}
	if len(m.Kinds) == 0 {
		return fmt.Errorf("extension %q: built-in feature extension declares no kinds", m.ID)
	}
	entry := &Entry{
		Manifest: m,
		Kind:     m.Kinds[0],
		enabled:  true,
		builtin:  true,
	}
	return r.Register(entry)
}
