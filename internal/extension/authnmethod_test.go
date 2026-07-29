package extension

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// stubAuthNMethod is an in-process ext.AuthNMethod test double: v4 ships no real
// SSO/LDAP plugin yet, so the host orchestration is proven against this stub.
// The full wire round trip is covered separately over a real gRPC subprocess by
// internal/extension/grpc's AuthNMethod parity tests. This stub only PROVES
// identity — it never issues a token.
type stubAuthNMethod struct {
	id   string
	kind ext.AuthKind
}

func (p *stubAuthNMethod) Manifest() ext.Manifest {
	return ext.Manifest{ID: p.id, Kinds: []ext.Kind{ext.KindAuthNMethod}}
}
func (p *stubAuthNMethod) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *stubAuthNMethod) Shutdown(ctx context.Context) error         { return nil }
func (p *stubAuthNMethod) MethodInfo() ext.AuthMethodInfo {
	return ext.AuthMethodInfo{ID: p.id, Kind: p.kind}
}
func (p *stubAuthNMethod) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	return ext.AuthNChallenge{ChallengeID: "c"}, nil
}
func (p *stubAuthNMethod) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	return ext.AuthNResult{Username: req.Params["username"], Verified: true}, nil
}

// TestGetAuthNMethodInstance confirms RegisterAuthNMethodInstance is the seam
// GetAuthNMethod resolves through for an in-process double.
func TestGetAuthNMethodInstance(t *testing.T) {
	const id = "com.example.authn.direct"
	RegisterAuthNMethodInstance(id, &stubAuthNMethod{id: id, kind: ext.AuthPrimary})
	t.Cleanup(func() { RegisterAuthNMethodInstance(id, nil) })

	r := NewRegistry()
	m, release, err := r.GetAuthNMethod(id)
	if err != nil {
		t.Fatalf("GetAuthNMethod: %v", err)
	}
	defer release()
	if m.MethodInfo().Kind != ext.AuthPrimary {
		t.Fatalf("got method for wrong kind: %+v", m.MethodInfo())
	}
}

// TestAuthNMethodTrust pins the host-side trust model: only a method registered
// through the trusted (built-in) path is trusted, and a method can never make
// itself trusted. Plain registration, a subprocess/gRPC entry, and an unknown id
// are all untrusted — the admin-login backstop depends on this.
func TestAuthNMethodTrust(t *testing.T) {
	r := NewRegistry()

	// A plain in-process registration is UNtrusted.
	const plainID = "com.example.authn.plain"
	RegisterAuthNMethodInstance(plainID, &stubAuthNMethod{id: plainID, kind: ext.AuthPrimary})
	t.Cleanup(func() { RegisterAuthNMethodInstance(plainID, nil) })
	if r.AuthNMethodTrusted(plainID) {
		t.Fatal("a plainly-registered in-process method must not be trusted")
	}

	// A host-authored built-in registration IS trusted.
	const builtinID = "builtin.example"
	RegisterTrustedAuthNMethodInstance(builtinID, &stubAuthNMethod{id: builtinID, kind: ext.AuthPrimary})
	t.Cleanup(func() { RegisterTrustedAuthNMethodInstance(builtinID, nil) })
	if !r.AuthNMethodTrusted(builtinID) {
		t.Fatal("a built-in (trusted-path) method must be trusted")
	}

	// An unknown id is untrusted (never a nil-map surprise).
	if r.AuthNMethodTrusted("nobody") {
		t.Fatal("an unknown method id must not be trusted")
	}

	// Re-registering the SAME id through the plain path must DROP trust: a method
	// cannot launder itself from untrusted into trusted, nor keep a stale flag.
	RegisterAuthNMethodInstance(builtinID, &stubAuthNMethod{id: builtinID, kind: ext.AuthPrimary})
	if r.AuthNMethodTrusted(builtinID) {
		t.Fatal("plain re-registration must clear a prior trust flag")
	}

	// Deregistration clears trust.
	RegisterTrustedAuthNMethodInstance(builtinID, &stubAuthNMethod{id: builtinID, kind: ext.AuthPrimary})
	RegisterTrustedAuthNMethodInstance(builtinID, nil)
	if r.AuthNMethodTrusted(builtinID) {
		t.Fatal("deregistration must clear trust")
	}
}

// TestGetAuthNMethodUnknownID confirms the registry seam rejects an unknown id
// cleanly rather than returning a nil method that would later panic.
func TestGetAuthNMethodUnknownID(t *testing.T) {
	r := NewRegistry()
	if _, _, err := r.GetAuthNMethod("nope"); err == nil {
		t.Fatal("expected an error for an unregistered authn method id")
	}
}

// TestGetAuthNMethodRejectsDisabled pins the invariant that a disabled method
// can never assert an identity: even though the entry exists in the registry,
// GetAuthNMethod must refuse it. (Instance doubles bypass the registry, so this
// exercises a registered gRPC-mode entry that is then disabled.)
func TestGetAuthNMethodRejectsDisabled(t *testing.T) {
	const id = "com.example.authn.disabled"
	r := NewRegistry()
	if err := r.RegisterPluginMode(ext.Manifest{
		ID:    id,
		Kinds: []ext.Kind{ext.KindAuthNMethod},
	}, "authnplugin", ExecGRPC); err != nil {
		t.Fatalf("RegisterPluginMode: %v", err)
	}
	if err := r.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, _, err := r.GetAuthNMethod(id); err == nil {
		t.Fatal("a disabled authn method must not resolve")
	}
}

// TestGetAuthNMethodRejectsWrongKind confirms an id registered under a different
// Kind is not resolvable as an authn method (no cross-Kind confusion).
func TestGetAuthNMethodRejectsWrongKind(t *testing.T) {
	const id = "com.example.notauthn"
	r := NewRegistry()
	if err := r.RegisterPluginMode(ext.Manifest{
		ID:    id,
		Kinds: []ext.Kind{ext.KindStorageDriver},
	}, "somebin", ExecGRPC); err != nil {
		t.Fatalf("RegisterPluginMode: %v", err)
	}
	if _, _, err := r.GetAuthNMethod(id); err == nil {
		t.Fatal("a StorageDriver id must not resolve as an authn method")
	}
}

// TestAuthNMethodsEnumeratesEnabledOnly confirms the enumeration used to drive
// the login page lists enabled Kind=AuthNMethod entries and excludes disabled
// ones.
func TestAuthNMethodsEnumeratesEnabledOnly(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"com.example.m1", "com.example.m2"} {
		if err := r.RegisterPluginMode(ext.Manifest{
			ID:    id,
			Kinds: []ext.Kind{ext.KindAuthNMethod},
		}, "bin", ExecGRPC); err != nil {
			t.Fatalf("RegisterPluginMode(%s): %v", id, err)
		}
	}
	if err := r.Disable("com.example.m2"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	methods := r.AuthNMethods()
	if len(methods) != 1 || methods[0].ID() != "com.example.m1" {
		t.Fatalf("AuthNMethods should list only the enabled method, got %v", methods)
	}
}
