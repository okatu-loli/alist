package extension

import (
	"context"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// eventHookFactory builds a live ext.EventHook plus a release func for an
// enabled Kind=EventHook gRPC plugin Entry. It is the gRPC-dispatch seam,
// mirroring authNMethodFactory: the server layer installs it
// (SetEventHookFactory) so internal/extension need not import pluginproc. Until
// a gRPC EventHook bridge lands the factory is nil and only in-process instances
// (RegisterEventHookInstance) are delivered to — the tested path in F1.
var eventHookFactory func(e *Entry) (ext.EventHook, func(), error)

// SetEventHookFactory installs the gRPC EventHook provider.
func SetEventHookFactory(fn func(e *Entry) (ext.EventHook, func(), error)) {
	eventHookFactory = fn
}

var (
	eventHookMu        sync.RWMutex
	eventHookInstances = map[string]ext.EventHook{}
)

// RegisterEventHookInstance registers an in-process ext.EventHook under id (a
// built-in feature bundle or a test double), bypassing eventHookFactory. A nil
// value deregisters. Mirrors RegisterAuthNMethodInstance.
func RegisterEventHookInstance(id string, h ext.EventHook) {
	eventHookMu.Lock()
	defer eventHookMu.Unlock()
	if h == nil {
		delete(eventHookInstances, id)
		return
	}
	eventHookInstances[id] = h
}

// resolveEventHook returns the live hook for an entry (in-process instance
// first, then the gRPC factory seam), or nil+noop when none is wired.
func resolveEventHook(e *Entry) (ext.EventHook, func()) {
	eventHookMu.RLock()
	if h, ok := eventHookInstances[e.Manifest.ID]; ok {
		eventHookMu.RUnlock()
		return h, func() {}
	}
	eventHookMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && eventHookFactory != nil {
		if h, release, err := eventHookFactory(e); err == nil {
			return h, release
		}
	}
	return nil, func() {}
}

// DispatchEvent fans a host event out to every enabled Kind=EventHook plugin
// that was granted the "events" capability (design §2.5, §3 Events bus). It is
// the host-side seam the fs mutation path calls after an operation commits (see
// internal/fs). Delivery is:
//
//   - gated: a plugin that did not declare/was not granted CapEvents is skipped,
//     so the mere shape of a user's fs activity is not leaked to a plugin the
//     admin did not approve for it;
//   - isolated: each OnEvent runs under a panic recover and its error is logged
//     and swallowed — one plugin cannot break dispatch to the others, and no
//     plugin can affect the already-committed originating operation;
//   - synchronous: hooks run inline in dispatch order. A hook is contractually
//     required to return promptly (see ext.EventHook); an async/timeout wrapper
//     is a documented hardening seam (F1 report) once a slow real hook exists.
//
// Any capability the hook then BORROWS (re-reading the object, calling out) is
// separately Grant-gated + Authorize-enforced by the F0 hostcap layer.
func DispatchEvent(ctx context.Context, r *Registry, ev ext.Event) {
	for _, e := range r.ByKind(ext.KindEventHook) {
		if !e.GrantsCapability(CapEvents) {
			continue
		}
		hook, release := resolveEventHook(e)
		if hook == nil {
			continue
		}
		deliverEvent(ctx, e.Manifest.ID, hook, ev)
		release()
	}
}

// deliverEvent invokes one hook with panic + error isolation.
func deliverEvent(ctx context.Context, id string, hook ext.EventHook, ev ext.Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("extension: EventHook %q panicked on %s: %v", id, ev.Type, r)
		}
	}()
	if err := hook.OnEvent(ctx, ev); err != nil {
		log.Warnf("extension: EventHook %q failed on %s: %v", id, ev.Type, err)
	}
}

// DispatchEventDefault fans an event out over DefaultRegistry, the entry point
// for the fs mutation path (which holds no Registry handle).
func DispatchEventDefault(ctx context.Context, ev ext.Event) {
	DispatchEvent(ctx, DefaultRegistry, ev)
}
