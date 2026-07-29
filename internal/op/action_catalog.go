package op

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// ActionDef describes one entry of the action vocabulary the permission editor
// renders from (spec §3.1). Name is fully qualified for system actions
// ("system.<domain>.<verb>") and a bare verb for fs actions. Implies encodes
// hierarchical containment (§6) — T3's hierarchy-aware Decision.Can consumes
// it; it is populated now so the vocabulary is complete before that lands.
type ActionDef struct {
	Name         model.Action   `json:"name"`
	ResourceType string         `json:"resource_type"`
	Domain       string         `json:"domain,omitempty"`
	Verb         string         `json:"verb,omitempty"`
	Implies      []model.Action `json:"implies,omitempty"`
	I18nKey      string         `json:"i18n_key"`
	Danger       bool           `json:"danger,omitempty"`
	Source       string         `json:"source"`
}

// CatalogAction is an ActionDef as returned by ActionCatalog, plus an Orphaned
// flag: true means the action appears in a stored permission row but is
// registered by nobody (e.g. left behind by an uninstalled plugin). Orphaned
// entries carry only Name — the editor must preserve them as opaque items, not
// silently drop them (§3.1 F2d).
type CatalogAction struct {
	ActionDef
	Orphaned bool `json:"orphaned"`
}

// CatalogSnapshot is a point-in-time view of the registry. Generation
// increments on every Register/Unregister so clients can detect staleness (§3.3
// F3).
type CatalogSnapshot struct {
	Generation int             `json:"generation"`
	Actions    []CatalogAction `json:"actions"`
	Orphaned   []model.Action  `json:"orphaned"`
}

var (
	catalogMu  sync.RWMutex
	catalog    = map[model.Action]ActionDef{}
	catalogGen int
)

// RegisterActions adds (or overwrites) action definitions attributed to source.
// The registry is keyed by Name so a re-register of the same Name overwrites the
// prior entry (last wins) — this de-dups plugin reloads instead of producing
// ghost duplicates (§3.1 F2a). Namespace ownership is enforced: source "core"
// may register bare fs verbs and system.* actions; source "plugin:<id>" may only
// register plugin:<id>:* actions. Any violation is rejected with an error and
// leaves the registry unchanged.
func RegisterActions(source string, defs ...ActionDef) error {
	if err := validateSource(source); err != nil {
		return err
	}
	for i := range defs {
		if err := checkNamespaceOwnership(source, defs[i]); err != nil {
			return err
		}
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	for _, d := range defs {
		d.Source = source // authoritatively stamp source so it always matches
		catalog[d.Name] = d
	}
	catalogGen++
	return nil
}

// UnregisterBySource removes every action attributed to source (plugin unload,
// §3.1 F2b). Stored permission rows referencing those actions are untouched and
// will surface as orphaned in the next ActionCatalog.
func UnregisterBySource(source string) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	changed := false
	for name, d := range catalog {
		if d.Source == source {
			delete(catalog, name)
			changed = true
		}
	}
	if changed {
		catalogGen++
	}
}

// implCache is a point-in-time reverse-implication index: rev maps a child
// action to the set of actions that directly declare it in their Implies
// (parent→child edges reversed). gen is the catalog generation it was built
// from, so a stale index is detected and rebuilt on the next lookup. The map is
// never mutated after publication — a fresh one is built and swapped in — so
// concurrent readers holding an old pointer stay race-free.
type implCache struct {
	gen int
	rev map[model.Action][]model.Action
}

var implCachePtr atomic.Pointer[implCache]

// reverseImplications returns the cached child→parents index, rebuilding it only
// when the catalog generation has moved. Decision.Can consults it on every call
// (hot path: Authorize, FilterListing per child), so the warm path is a single
// RLock + atomic load. A concurrent rebuild is benign: each builds its own map
// and the last Store wins with identical content.
func reverseImplications() map[model.Action][]model.Action {
	catalogMu.RLock()
	gen := catalogGen
	if c := implCachePtr.Load(); c != nil && c.gen == gen {
		catalogMu.RUnlock()
		return c.rev
	}
	rev := make(map[model.Action][]model.Action, len(catalog))
	for _, d := range catalog {
		for _, child := range d.Implies {
			rev[child] = append(rev[child], d.Name)
		}
	}
	catalogMu.RUnlock()
	implCachePtr.Store(&implCache{gen: gen, rev: rev})
	return rev
}

// ancestors returns every action that (transitively) implies a — i.e. all of
// a's coarser parents up the Implies DAG, nearest first. It walks the reversed
// implication edges breadth-first with a visited set (a included) so an
// accidental cycle can never loop forever. fs actions declare no Implies and
// nobody implies them, so ancestors(fsVerb) is empty and Decision.Can degrades
// to the flat set-membership check the RBAC5 fs suite depends on.
func ancestors(a model.Action) []model.Action {
	rev := reverseImplications()
	if len(rev[a]) == 0 {
		return nil // fast path: no parents, no walk (covers all fs actions)
	}
	seen := map[model.Action]struct{}{a: {}}
	var out []model.Action
	queue := []model.Action{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, parent := range rev[cur] {
			if _, ok := seen[parent]; ok {
				continue // cycle / diamond guard
			}
			seen[parent] = struct{}{}
			out = append(out, parent)
			queue = append(queue, parent)
		}
	}
	return out
}

// ActionCatalog returns a sorted snapshot of the registry plus orphan detection:
// every distinct action found in stored permission rows that is not registered
// is appended as an Orphaned entry (§3.1 F6a).
func ActionCatalog() (CatalogSnapshot, error) {
	catalogMu.RLock()
	snap := CatalogSnapshot{Generation: catalogGen}
	registered := make(map[model.Action]struct{}, len(catalog))
	defs := make([]ActionDef, 0, len(catalog))
	for name, d := range catalog {
		registered[name] = struct{}{}
		defs = append(defs, d)
	}
	catalogMu.RUnlock()

	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	for _, d := range defs {
		snap.Actions = append(snap.Actions, CatalogAction{ActionDef: d})
	}

	stored, err := db.DistinctPermissionActions()
	if err != nil {
		return snap, err
	}
	var orphans []model.Action
	for _, a := range stored {
		if _, ok := registered[a]; !ok {
			orphans = append(orphans, a)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i] < orphans[j] })
	for _, a := range orphans {
		snap.Actions = append(snap.Actions, CatalogAction{ActionDef: ActionDef{Name: a}, Orphaned: true})
		snap.Orphaned = append(snap.Orphaned, a)
	}
	return snap, nil
}

// ValidateActions enforces, at permission create/update time, that every action
// is registered AND belongs to the row's resource type. A system row must not
// carry a bare fs verb (and vice versa); unknown actions and unknown resource
// types are rejected (spec §2.3).
func ValidateActions(resourceType string, acts model.Actions) error {
	rt := resourceType
	if rt == "" {
		rt = model.ResourceFS
	}
	if rt != model.ResourceFS && rt != model.ResourceSystem {
		return fmt.Errorf("unknown resource_type %q", resourceType)
	}
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	for _, a := range acts {
		d, ok := catalog[a]
		if !ok {
			return fmt.Errorf("unknown action %q", a)
		}
		defRT := d.ResourceType
		if defRT == "" {
			defRT = model.ResourceFS
		}
		if defRT != rt {
			return fmt.Errorf("action %q belongs to resource_type %q, not %q", a, defRT, rt)
		}
	}
	return nil
}

// validateSource rejects source strings that are neither "core" nor a
// well-formed "plugin:<id>".
func validateSource(source string) error {
	if source == "core" {
		return nil
	}
	if strings.HasPrefix(source, "plugin:") && strings.TrimPrefix(source, "plugin:") != "" {
		return nil
	}
	return fmt.Errorf("invalid action source %q (want \"core\" or \"plugin:<id>\")", source)
}

// checkNamespaceOwnership enforces that a source only registers names it owns:
// core owns bare fs verbs + system.*; plugin:<id> owns plugin:<id>:* only.
func checkNamespaceOwnership(source string, d ActionDef) error {
	name := string(d.Name)
	if source == "core" {
		if strings.HasPrefix(name, "plugin:") {
			return fmt.Errorf("core may not register plugin-namespaced action %q", name)
		}
		if strings.HasPrefix(name, "system.") || d.ResourceType == model.ResourceFS {
			return nil
		}
		return fmt.Errorf("core may only register system.* or fs actions, got %q", name)
	}
	// plugin:<id> (validateSource already vetted the shape)
	prefix := source + ":" // e.g. "plugin:acme:"
	if !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("source %q may only register %s* actions, got %q", source, prefix, name)
	}
	return nil
}
