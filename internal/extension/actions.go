package extension

import (
	"context"
	"fmt"
	"sync"

	"github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// ActionInvoker executes one of an extension's declared menu actions. It is the
// host-side seam for Action.Invoke (SDK §2) for built-in / in-process
// extensions, which register a concrete invoker directly (no gRPC round trip
// needed — see InvokeExtensionAction for the gRPC-backed path, W8b).
type ActionInvoker interface {
	InvokeAction(ctx context.Context, actionID string, args map[string]any) (any, error)
}

var (
	actionMu       sync.RWMutex
	actionInvokers = map[string]ActionInvoker{}
)

// RegisterActionInvoker wires an in-process invoker for an extension id. A nil
// invoker deregisters. Safe for concurrent use.
func RegisterActionInvoker(extID string, inv ActionInvoker) {
	actionMu.Lock()
	defer actionMu.Unlock()
	if inv == nil {
		delete(actionInvokers, extID)
		return
	}
	actionInvokers[extID] = inv
}

// actionFactory is the gRPC-dispatch seam for Action (W8b), mirroring
// apiRouteFactory (internal/extension/apiroute.go) one Kind over: nil until
// server/handles/plugins.go wires it to pluginproc.Default.AcquireAction.
var actionFactory func(e *Entry) (ext.Action, func(), error)

// SetActionFactory installs the gRPC Action provider.
func SetActionFactory(fn func(e *Entry) (ext.Action, func(), error)) {
	actionFactory = fn
}

// resolveAction resolves the gRPC ext.Action for a declared-but-unwired-in-
// process entry, mirroring resolveAPIRoute: only ExecGRPC entries with a
// binPath, a declared Kind=Action, and an installed actionFactory resolve to
// anything; everything else returns nil (no host-side executor), which
// InvokeExtensionAction turns into NOT_IMPLEMENT.
//
// The Kind check guards against launching a subprocess for an entry that
// never declared Action at all: without it, any ExecGRPC entry with a
// binPath would still pay the cost of a full plugin launch
// (host.LaunchAction dispenses ActionService and waits on the handshake)
// only to find the plugin never implements ext.Action — a needlessly
// started subprocess where the cheap, immediate 501 an undeclared Kind
// should produce instead.
//
// The check reads the Manifest's declared Kinds (hasKind, lifecycle.go)
// rather than Entry.HasKind: HasKind only sees the entry's primary Kind plus
// extraKinds, and extraKinds is populated solely by FeatureBundle indexing
// (featurebundle.go) — a plugin that legitimately declares multiple Kinds
// itself (e.g. manifest Kinds: ["storage_driver", "action"], registered
// under its primary Kind=StorageDriver) would never get an extraKinds entry
// for Action and so would be wrongly 501'd here despite having declared it.
// The Manifest is the actual source of truth for "did this plugin declare
// Action", independent of which Kind bucket the registry happened to index
// it under.
func resolveAction(e *Entry) (ext.Action, func()) {
	if e.execMode == ExecGRPC && e.binPath != "" && hasKind(e.Manifest, ext.KindAction) && actionFactory != nil {
		a, release, err := actionFactory(e)
		if err == nil {
			return a, release
		}
		log.Warnf("extension: resolveAction %q: actionFactory failed: %v", e.Manifest.ID, err)
	}
	return nil, func() {}
}

// ErrActionNotDeclared is returned when an extension exists but did not declare
// the requested action in its Contributions.MenuActions. Callers map it to 404:
// an undeclared action is indistinguishable from a missing one to a client.
var ErrActionNotDeclared = fmt.Errorf("extension: action not declared")

// actionDeclared reports whether the manifest declares a menu action with id
// actionID.
func actionDeclared(m ext.Manifest, actionID string) bool {
	for _, a := range m.Contributions.MenuActions {
		if a.ID == actionID {
			return true
		}
	}
	return false
}

// InvokeExtensionAction routes an action call to an extension's handler, but
// only if the enabled extension declared that action. Resolution order:
//   - extension not enabled/registered      → ErrActionNotDeclared (404)
//   - action not in MenuActions             → ErrActionNotDeclared (404)
//   - declared, in-process invoker          → invoker result
//   - declared, no invoker, gRPC resolves   → ext.Action.Invoke result (W8b)
//   - declared, neither resolves            → ext NOT_IMPLEMENT (seam)
//
// principal is the caller's identity handle, threaded through to the gRPC
// leg's Invoke call (ext.Action.Invoke takes one) — mirrors SubmitAssist. The
// in-process ActionInvoker seam predates principal-carrying and is left
// unchanged: it never saw one before and still doesn't, same as
// AssistProvider (SubmitAssist's doc comment).
func InvokeExtensionAction(ctx context.Context, r *Registry, principal ext.Principal, extID, actionID string, args map[string]any) (any, error) {
	e, ok := r.Get(extID)
	if !ok || !e.Enabled() {
		return nil, ErrActionNotDeclared
	}
	if !actionDeclared(e.Manifest, actionID) {
		return nil, ErrActionNotDeclared
	}
	actionMu.RLock()
	inv := actionInvokers[extID]
	actionMu.RUnlock()
	if inv != nil {
		return inv.InvokeAction(ctx, actionID, args)
	}
	if a, release := resolveAction(e); a != nil {
		defer release()
		return a.Invoke(ctx, principal, actionID, args)
	}
	return nil, ext.NewError(ext.ErrCodeNotImplement,
		fmt.Sprintf("action %q of extension %q has no host-side invoker wired", actionID, extID))
}
