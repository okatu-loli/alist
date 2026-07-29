package extension

import (
	"fmt"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// authProviderFactory builds a live ext.AuthProvider plus a release func for an
// enabled Kind=AuthProvider gRPC plugin Entry. Registered by the server layer
// (SetAuthProviderFactory) to avoid an import cycle: internal/extension cannot
// import internal/extension/pluginproc+grpc/host at this level without pulling
// go-plugin into every extension build — the provider idiom matches
// pluginDriverFactory/SetPluginDriverFactory (storage.go).
var authProviderFactory func(e *Entry) (ext.AuthProvider, func(), error)

// SetAuthProviderFactory installs the provider that turns an enabled
// Kind=AuthProvider gRPC plugin Entry into a live ext.AuthProvider and a
// release func (mirrors pluginproc's Acquire/release pairing, so the
// subprocess is shared/reused across calls and torn down when idle). The
// server layer calls this once at init (server/handles/plugins.go), wiring
// pluginproc.Default.AcquireAuthProvider in without internal/extension
// importing pluginproc directly.
func SetAuthProviderFactory(fn func(e *Entry) (ext.AuthProvider, func(), error)) {
	authProviderFactory = fn
}

var (
	authInstMu    sync.RWMutex
	authInstances = map[string]ext.AuthProvider{}
)

// RegisterAuthProviderInstance registers an in-process ext.AuthProvider
// directly under id, bypassing authProviderFactory entirely. This is the seam
// a built-in AuthProvider (none exist yet — v4 ships no real network drive)
// or a test double uses to exercise the wizard oauth mechanism without a real
// subprocess. A nil value deregisters.
func RegisterAuthProviderInstance(id string, p ext.AuthProvider) {
	authInstMu.Lock()
	defer authInstMu.Unlock()
	if p == nil {
		delete(authInstances, id)
		return
	}
	authInstances[id] = p
}

// GetAuthProvider resolves a Kind=AuthProvider extension id to a live
// ext.AuthProvider plus a release func (call it — even a no-op — when done
// with this acquisition; mirrors pluginproc.Manager.Acquire's contract). It is
// the single seam the wizard engine and the oauth callback handler both go
// through, so tests can substitute RegisterAuthProviderInstance for a real
// subprocess.
func (r *Registry) GetAuthProvider(id string) (ext.AuthProvider, func(), error) {
	authInstMu.RLock()
	if p, ok := authInstances[id]; ok {
		authInstMu.RUnlock()
		return p, func() {}, nil
	}
	authInstMu.RUnlock()

	r.mu.RLock()
	e, ok := r.byID[id]
	var execMode ExecMode
	var binPath string
	if ok {
		ok = e.Kind == ext.KindAuthProvider && e.enabled
		execMode = e.execMode
		binPath = e.binPath
	}
	r.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("no auth provider named: %s", id)
	}
	if execMode == ExecGRPC && binPath != "" && authProviderFactory != nil {
		return authProviderFactory(e)
	}
	return nil, nil, fmt.Errorf("no auth provider named: %s", id)
}
