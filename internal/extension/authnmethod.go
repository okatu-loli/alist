package extension

import (
	"fmt"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// authNMethodFactory builds a live ext.AuthNMethod plus a release func for an
// enabled Kind=AuthNMethod gRPC plugin Entry. Registered by the server layer
// (SetAuthNMethodFactory) to avoid an import cycle: internal/extension cannot
// import internal/extension/pluginproc+grpc/host at this level without pulling
// go-plugin into every extension build — the provider idiom matches
// authProviderFactory/SetAuthProviderFactory (oauth.go).
var authNMethodFactory func(e *Entry) (ext.AuthNMethod, func(), error)

// SetAuthNMethodFactory installs the provider that turns an enabled
// Kind=AuthNMethod gRPC plugin Entry into a live ext.AuthNMethod and a release
// func (mirrors pluginproc's Acquire/release pairing, so the subprocess is
// shared/reused across calls and torn down when idle). The server layer calls
// this once at init (server/handles/plugins.go), wiring
// pluginproc.Default.AcquireAuthNMethod in without internal/extension importing
// pluginproc directly.
func SetAuthNMethodFactory(fn func(e *Entry) (ext.AuthNMethod, func(), error)) {
	authNMethodFactory = fn
}

var (
	authNInstMu    sync.RWMutex
	authNInstances = map[string]ext.AuthNMethod{}
	// authNTrusted records which in-process methods the HOST itself authored and
	// registered as built-ins. Trust is decided HERE, at registration, by the
	// host — it is NEVER read from anything a method reports about itself. Only an
	// id registered through RegisterTrustedAuthNMethodInstance appears here; every
	// gRPC subprocess plugin and every plain RegisterAuthNMethodInstance double is
	// absent, hence untrusted. This is the single source the admin-login backstop
	// (server/handles/authn.go) consults to decide whether a method may relax the
	// "no login-method admin session" invariant.
	authNTrusted = map[string]bool{}
)

// RegisterAuthNMethodInstance registers an in-process ext.AuthNMethod directly
// under id, bypassing authNMethodFactory entirely. This is the seam a test
// double uses to exercise the login orchestration without a real subprocess, and
// the path a NON-built-in in-process method takes. Such a method is UNtrusted: it
// can never relax the admin-login backstop. A built-in the core itself authors
// uses RegisterTrustedAuthNMethodInstance instead. A nil value deregisters (and
// clears any trust flag).
func RegisterAuthNMethodInstance(id string, m ext.AuthNMethod) {
	authNInstMu.Lock()
	defer authNInstMu.Unlock()
	if m == nil {
		delete(authNInstances, id)
		delete(authNTrusted, id)
		return
	}
	authNInstances[id] = m
	// A plain registration is never trusted; drop any stale flag from a prior
	// trusted registration under the same id.
	delete(authNTrusted, id)
}

// RegisterTrustedAuthNMethodInstance registers a BUILT-IN, host-authored
// in-process ext.AuthNMethod (e.g. the built-in passkey login) and marks it
// trusted. "Trusted" is a host-side registration fact set on THIS call path — it
// is not, and can never be, self-asserted by the method: a method has no way to
// reach this function across the plugin boundary, so a subprocess/third-party
// method is structurally excluded from trust. A trusted method is the only kind
// permitted to mint an admin session through the AuthNMethod login path. A nil
// value deregisters (and clears the trust flag).
func RegisterTrustedAuthNMethodInstance(id string, m ext.AuthNMethod) {
	authNInstMu.Lock()
	defer authNInstMu.Unlock()
	if m == nil {
		delete(authNInstances, id)
		delete(authNTrusted, id)
		return
	}
	authNInstances[id] = m
	authNTrusted[id] = true
}

// AuthNMethodTrusted reports whether id was registered by the host as a trusted
// built-in (RegisterTrustedAuthNMethodInstance). It is the authoritative trust
// check the admin-login backstop uses: a subprocess plugin, an unregistered id,
// or an untrusted in-process double all return false, so none of them can ever
// obtain the admin-login relaxation. Trust is host-decided, never method-reported.
func (r *Registry) AuthNMethodTrusted(id string) bool {
	authNInstMu.RLock()
	defer authNInstMu.RUnlock()
	return authNTrusted[id]
}

// GetAuthNMethod resolves an enabled Kind=AuthNMethod extension id to a live
// ext.AuthNMethod plus a release func (call it — even a no-op — when done;
// mirrors pluginproc.Manager.Acquire's contract). It is the single seam the
// login-flow orchestrator goes through, so tests can substitute
// RegisterAuthNMethodInstance for a real subprocess.
//
// It enforces the registry invariants a login method must satisfy before it can
// ever assert an identity: the id must be registered, be Kind=AuthNMethod, and
// be enabled. A disabled or unknown method resolves to an error — never a
// silent identity assertion.
func (r *Registry) GetAuthNMethod(id string) (ext.AuthNMethod, func(), error) {
	authNInstMu.RLock()
	if m, ok := authNInstances[id]; ok {
		authNInstMu.RUnlock()
		return m, func() {}, nil
	}
	authNInstMu.RUnlock()

	r.mu.RLock()
	e, ok := r.byID[id]
	var execMode ExecMode
	var binPath string
	if ok {
		ok = e.Kind == ext.KindAuthNMethod && e.enabled
		execMode = e.execMode
		binPath = e.binPath
	}
	r.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("no authn method named: %s", id)
	}
	if execMode == ExecGRPC && binPath != "" && authNMethodFactory != nil {
		return authNMethodFactory(e)
	}
	return nil, nil, fmt.Errorf("no authn method named: %s", id)
}

// AuthNMethods returns the enabled Kind=AuthNMethod entries, id-sorted. The
// login page renders primary/federated methods and the account page renders
// second_factor ones; the authoritative AuthKind comes from each method's
// MethodInfo() (resolved via GetAuthNMethod), but the entry list here is what
// the host iterates to know WHICH methods exist without launching every plugin.
func (r *Registry) AuthNMethods() []*Entry {
	return r.ByKind(ext.KindAuthNMethod)
}
