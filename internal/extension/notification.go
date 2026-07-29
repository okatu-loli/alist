package extension

import (
	"context"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// notificationFactory builds a live ext.Notification plus a release func for
// an enabled Kind=NotificationChannel gRPC plugin Entry. It is the
// gRPC-dispatch seam, mirroring eventHookFactory: the server layer installs
// it (SetNotificationFactory) so internal/extension need not import
// pluginproc. Until it is wired the factory is nil and only in-process
// instances (RegisterNotificationInstance) are delivered to.
var notificationFactory func(e *Entry) (ext.Notification, func(), error)

// SetNotificationFactory installs the gRPC Notification provider.
func SetNotificationFactory(fn func(e *Entry) (ext.Notification, func(), error)) {
	notificationFactory = fn
}

var (
	notificationMu        sync.RWMutex
	notificationInstances = map[string]ext.Notification{}
)

// RegisterNotificationInstance registers an in-process ext.Notification under
// id (a built-in feature bundle or a test double), bypassing
// notificationFactory. A nil value deregisters. Mirrors
// RegisterEventHookInstance.
func RegisterNotificationInstance(id string, n ext.Notification) {
	notificationMu.Lock()
	defer notificationMu.Unlock()
	if n == nil {
		delete(notificationInstances, id)
		return
	}
	notificationInstances[id] = n
}

// resolveNotification returns the live channel for an entry (in-process
// instance first, then the gRPC factory seam), or nil+noop when none is
// wired.
func resolveNotification(e *Entry) (ext.Notification, func()) {
	notificationMu.RLock()
	if n, ok := notificationInstances[e.Manifest.ID]; ok {
		notificationMu.RUnlock()
		return n, func() {}
	}
	notificationMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && notificationFactory != nil {
		if n, release, err := notificationFactory(e); err == nil {
			return n, release
		}
	}
	return nil, func() {}
}

// DispatchNotification fans a host notice out to every enabled
// Kind=NotificationChannel plugin (design: NotificationChannel kind — a sink
// the host fans notices out to). It is the host-side seam a future
// host/FeatureBundle caller invokes to raise a notice (no host code emits
// notices yet — this dispatch seam exists ahead of that, mirroring
// DispatchEvent). Delivery is:
//
//   - isolated: each Notify runs under a panic recover and its error is
//     logged and swallowed — one channel cannot break dispatch to the others;
//   - synchronous: channels run inline in dispatch order. A channel is
//     contractually required to return promptly (see ext.Notification); an
//     async/timeout wrapper is a documented hardening seam once a slow real
//     channel exists.
//
// Any capability a channel then BORROWS (rendering a richer message, calling
// out) is separately Grant-gated + Authorize-enforced by the F0 hostcap
// layer.
func DispatchNotification(ctx context.Context, r *Registry, n ext.Notice) {
	for _, e := range r.ByKind(ext.KindNotification) {
		ch, release := resolveNotification(e)
		if ch == nil {
			continue
		}
		deliverNotification(ctx, e.Manifest.ID, ch, n)
		release()
	}
}

// deliverNotification invokes one channel with panic + error isolation.
func deliverNotification(ctx context.Context, id string, ch ext.Notification, n ext.Notice) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("extension: Notification %q panicked on %q: %v", id, n.Title, r)
		}
	}()
	if err := ch.Notify(ctx, n); err != nil {
		log.Warnf("extension: Notification %q failed on %q: %v", id, n.Title, err)
	}
}

// DispatchNotificationDefault fans a notice out over DefaultRegistry, the
// entry point for a future host caller that holds no Registry handle.
func DispatchNotificationDefault(ctx context.Context, n ext.Notice) {
	DispatchNotification(ctx, DefaultRegistry, n)
}
