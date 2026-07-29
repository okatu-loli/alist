package extension

import (
	"fmt"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// This file is the host consumption of Kind=FeatureBundle (design §2.5): the
// composite底座 for share / labels. A FeatureBundle is a single value that also
// implements the existing extension-point interfaces it contributes (APIRoute,
// EventHook, ScheduledJob); the host EXPANDS it onto the F1 wiring points by
// asserting which interfaces it satisfies, reusing that plumbing rather than
// building a parallel one. Expansion is:
//
//   - register the bundle value as the in-process instance for each contributed
//     extension point (RegisterAPIRouteInstance / RegisterEventHookInstance / …);
//   - AddKindIndex the bundle's Entry under each contributed Kind, so ByKind(...)
//     and HasKind reach it even though its primary Kind is FeatureBundle.
//
// The dispatch-time Grant gate and the F0 borrow gate are unchanged — a bundle's
// public APIRoute still needs public_routes, its EventHook still needs events,
// and anything it borrows through Host stays Authorize-enforced.

var (
	featureBundleMu        sync.RWMutex
	featureBundleInstances = map[string]ext.FeatureBundle{}
)

// RegisterFeatureBundleInstance registers an in-process ext.FeatureBundle under
// id (a built-in composite or a test double). A nil value deregisters.
func RegisterFeatureBundleInstance(id string, b ext.FeatureBundle) {
	featureBundleMu.Lock()
	defer featureBundleMu.Unlock()
	if b == nil {
		delete(featureBundleInstances, id)
		return
	}
	featureBundleInstances[id] = b
}

// resolveFeatureBundle returns the in-process instance registered under e's
// manifest id, or nil+noop when none is registered. Unlike every other Kind's
// resolve function there is no gRPC-factory branch here (this is deliberate,
// not an oversight): ExpandFeatureBundle below never even calls this function
// for an ExecGRPC entry — it branches to expandGRPCFeatureBundle first, which
// wires the bundle's contributed sub-Kinds onto their OWN existing gRPC
// factories (resolveAPIRoute/resolveEventHook/...) instead of resolving one
// "bundle" value. A gRPC-mode FeatureBundle factory seam would therefore be
// dead code with nothing left to call it, which is exactly what used to be
// here (SetFeatureBundleFactory/featureBundleFactory, removed post-Phase-D
// once expandGRPCFeatureBundle made it unreachable) — see
// expandGRPCFeatureBundle's doc comment for the full rationale.
func resolveFeatureBundle(e *Entry) (ext.FeatureBundle, func()) {
	featureBundleMu.RLock()
	defer featureBundleMu.RUnlock()
	if b, ok := featureBundleInstances[e.Manifest.ID]; ok {
		return b, func() {}
	}
	return nil, func() {}
}

// ExpandedBundle records what a FeatureBundle expansion contributed, so the
// caller (and CollapseFeatureBundle) knows which wiring points to tear down.
type ExpandedBundle struct {
	Spec  ext.BundleSpec
	Kinds []ext.Kind // the extra kinds indexed (APIRoute/EventHook/ScheduledJob)
}

// expandableKinds are the sub-Kinds a FeatureBundle may contribute — the F1/F2
// wiring points ExpandFeatureBundle reuses (mirrors the interface-assertion set
// below: APIRoute/EventHook/ScheduledJob/FileDecorator). A manifest that
// declares any OTHER Kind alongside FeatureBundle (e.g. a StorageDriver mixed
// in by mistake) is not one of these reused wiring points; the gRPC expansion
// path below skips it with a warning rather than failing the whole expansion,
// since there is no F1 point to index it onto.
var expandableKinds = map[ext.Kind]bool{
	ext.KindAPIRoute:      true,
	ext.KindEventHook:     true,
	ext.KindScheduledJob:  true,
	ext.KindFileDecorator: true,
}

// ExpandFeatureBundle resolves an enabled Kind=FeatureBundle plugin and registers
// each extension point it contributes onto the F1 wiring points, indexing its
// Entry under each contributed Kind. It is idempotent per contributed interface
// (re-registering the same instance). Returns what was expanded, or an error if
// the id is not an enabled feature bundle / has no host-side instance.
//
// NOTE: the bundle instance is held for the process lifetime as the in-process
// handler (it is the live APIRoute/EventHook object), so the resolve release is a
// no-op for in-process instances; the gRPC leg is handled by
// expandGRPCFeatureBundle below, which never resolves/holds a bundle instance at
// all.
func ExpandFeatureBundle(r *Registry, id string) (ExpandedBundle, error) {
	e, ok := r.Get(id)
	// Nothing ever AddKindIndex's KindFeatureBundle itself onto an entry (the
	// bundle expansion adds APIRoute/EventHook/ScheduledJob/FileDecorator TO a
	// bundle, never the reverse), so DeclaresKind — the Manifest's own Kinds
	// list — is the sole truth source for "is this a FeatureBundle", exactly
	// like AccessProtocol above.
	if !ok || !e.Enabled() || !e.DeclaresKind(ext.KindFeatureBundle) {
		return ExpandedBundle{}, fmt.Errorf("no feature bundle named: %s", id)
	}

	// A gRPC (disk-plugin) FeatureBundle has no single Go value to resolve and
	// interface-assert across the process boundary — the plugin's contributed
	// sub-Kinds are separate gRPC clients, each with its own dispense/factory
	// seam (resolveAPIRoute/resolveEventHook/resolveScheduledJob/
	// resolveFileDecorator), not facets of one object. So instead of resolving a
	// bundle instance, trust the manifest's own declared Kinds list (the SDK
	// truth source, same as DeclaresKind above) for what to index: expansion
	// becomes pure registry bookkeeping, and dispatch to each sub-Kind then
	// falls through to that Kind's OWN existing gRPC factory exactly as it
	// would for a plain (non-bundle) gRPC plugin declaring that Kind directly.
	if e.execMode == ExecGRPC {
		return expandGRPCFeatureBundle(r, e)
	}

	bundle, release := resolveFeatureBundle(e)
	if bundle == nil {
		return ExpandedBundle{}, ext.NewError(ext.ErrCodeNotImplement,
			fmt.Sprintf("FeatureBundle %q has no host-side handler wired", id))
	}
	_ = release // in-process instance is long-lived; gRPC hold is a seam
	out := ExpandedBundle{Spec: bundle.Bundle()}

	// Discover contributed extension points by interface assertion on the bundle
	// value and wire each onto its F1 registration point + kind index.
	if route, ok := bundle.(ext.APIRoute); ok {
		RegisterAPIRouteInstance(id, route)
		if err := r.AddKindIndex(id, ext.KindAPIRoute); err != nil {
			return out, err
		}
		out.Kinds = append(out.Kinds, ext.KindAPIRoute)
	}
	if hook, ok := bundle.(ext.EventHook); ok {
		RegisterEventHookInstance(id, hook)
		if err := r.AddKindIndex(id, ext.KindEventHook); err != nil {
			return out, err
		}
		out.Kinds = append(out.Kinds, ext.KindEventHook)
	}
	if job, ok := bundle.(ext.ScheduledJob); ok {
		RegisterScheduledJobInstance(id, job)
		if err := r.AddKindIndex(id, ext.KindScheduledJob); err != nil {
			return out, err
		}
		out.Kinds = append(out.Kinds, ext.KindScheduledJob)
	}
	// A bundle that renders a FileColumn (the labels badge) supplies its data
	// through a FileDecorator field group; wire that leg onto the F2 decorator
	// registry the same way, so ByKind(FileDecorator)/DecorateFields reach it.
	if dec, ok := bundle.(ext.FileDecorator); ok {
		RegisterFileDecoratorInstance(id, dec)
		if err := r.AddKindIndex(id, ext.KindFileDecorator); err != nil {
			return out, err
		}
		out.Kinds = append(out.Kinds, ext.KindFileDecorator)
	}
	return out, nil
}

// expandGRPCFeatureBundle wires a gRPC-mode FeatureBundle entry's contributed
// sub-Kinds onto their F1/F2 registry indexes WITHOUT resolving or
// interface-asserting a bundle instance — there is no in-process value to
// assert against. It walks e.Manifest.Kinds (the manifest is already loaded
// and DeclaresKind(FeatureBundle) was checked by the caller), skips
// KindFeatureBundle itself and any declared Kind that is not one of the four
// expandable wiring points (logged as a warning, not an error — a manifest
// mixing in an unrelated Kind should not abort the whole expansion), and
// AddKindIndex's the rest. No RegisterXInstance call happens here: dispatch to
// each contributed Kind (DispatchAPIRoute/DispatchEvent/the scheduler tick/
// DecorateFields) resolves the live handler through that Kind's OWN existing
// gRPC factory seam, exactly as it would for a plain (non-bundle) gRPC plugin
// declaring the Kind directly.
//
// The returned ExpandedBundle.Spec is left zero: BundleSpec
// (NeedsStore/NeedsSign) only serves the IN-PROCESS built-in bundle's own
// construction (it tells the host which host handles to close over before the
// bundle value exists). A gRPC plugin instead requests those same
// capabilities through Manifest.Permissions.HostAPIs / its Grant, enforced by
// the F0 hostcap Grant gate on every borrowed call, so no separate spec is
// needed here.
func expandGRPCFeatureBundle(r *Registry, e *Entry) (ExpandedBundle, error) {
	out := ExpandedBundle{}
	for _, k := range e.Manifest.Kinds {
		if k == ext.KindFeatureBundle {
			continue
		}
		if !expandableKinds[k] {
			log.Warnf("extension: FeatureBundle %q declares non-expandable kind %q, skipped",
				e.Manifest.ID, k)
			continue
		}
		if err := r.AddKindIndex(e.Manifest.ID, k); err != nil {
			return out, err
		}
		out.Kinds = append(out.Kinds, k)
	}
	return out, nil
}

// RegisterBuiltinFeatureBundle installs an in-process, trusted built-in
// FeatureBundle (labels) into r: it inserts the enabled registry entry carrying
// the bundle's Manifest (its FileColumn/MenuAction contributions surface through
// AggregateContributions), registers the in-process instance, and expands it onto
// every F1/F2 wiring point it contributes (APIRoute/EventHook/ScheduledJob/
// FileDecorator). Registration is the single call a built-in bundle needs at boot;
// the entry is trusted by virtue of being host-registered, exactly like the
// built-in passkey/SSO login methods.
func RegisterBuiltinFeatureBundle(r *Registry, b ext.FeatureBundle) error {
	m := b.Manifest()
	if m.ID == "" {
		return fmt.Errorf("extension: built-in feature bundle with empty manifest id")
	}
	entry := &Entry{
		Manifest:  m,
		Kind:      ext.KindFeatureBundle,
		Mode:      InstanceModeSingleton,
		singleton: b,
		enabled:   true,
		builtin:   true,
	}
	if err := r.Register(entry); err != nil {
		return err
	}
	RegisterFeatureBundleInstance(m.ID, b)
	if _, err := ExpandFeatureBundle(r, m.ID); err != nil {
		return err
	}
	return nil
}

// CollapseFeatureBundle reverses ExpandFeatureBundle: it deregisters the bundle's
// in-process instances from each F1 wiring point (disable/uninstall). The extra
// kind indexes are cleared by Registry.Deregister when the entry is removed; for
// a mere disable the entry stays but its instances are gone, so dispatch resolves
// to nil and skips it.
func CollapseFeatureBundle(id string) {
	RegisterAPIRouteInstance(id, nil)
	RegisterEventHookInstance(id, nil)
	RegisterScheduledJobInstance(id, nil)
	RegisterFileDecoratorInstance(id, nil)
	RegisterFeatureBundleInstance(id, nil)
}
