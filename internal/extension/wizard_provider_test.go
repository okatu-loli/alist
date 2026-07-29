package extension

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AlistGo/alist-ext"
)

// This file exercises validateWizardOAuthProviders (wizard.go) — the
// registration/enable-time check that a wizard oauth step's Provider names an
// extension that both exists and declares Kind=AuthProvider (WS-8), rather
// than only failing much later, deep inside the wizard engine's
// beginOAuthLocked, with a far less actionable error.

func providerEntry(id string, kinds []ext.Kind) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: kinds},
		Kind:     kinds[0],
		enabled:  true,
	}
}

func oauthWizardManifest(id, provider string, extraKinds ...ext.Kind) ext.Manifest {
	return ext.Manifest{
		ID:    id,
		Kinds: append([]ext.Kind{ext.KindStorageDriver}, extraKinds...),
		Wizard: ext.Wizard{Steps: []ext.WizardStep{
			{ID: "oauth", Type: ext.WizardOAuth, Provider: provider},
			{ID: "done", Type: ext.WizardDone},
		}},
	}
}

func TestRegisterRejectsWizardOAuthUnknownProvider(t *testing.T) {
	r := NewRegistry()
	err := r.Register(&Entry{Manifest: oauthWizardManifest("ext.wiz1", "ext.nosuchprovider"), Kind: ext.KindStorageDriver})
	if err == nil {
		t.Fatal("expected an error for a wizard oauth step referencing an unregistered provider")
	}
}

func TestRegisterRejectsWizardOAuthWrongKindProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(providerEntry("ext.notaprovider", []ext.Kind{ext.KindAction})); err != nil {
		t.Fatal(err)
	}
	err := r.Register(&Entry{Manifest: oauthWizardManifest("ext.wiz2", "ext.notaprovider"), Kind: ext.KindStorageDriver})
	if err == nil {
		t.Fatal("expected an error for a wizard oauth step referencing a non-AuthProvider extension")
	}
}

func TestRegisterAcceptsWizardOAuthValidProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(providerEntry("ext.provider1", []ext.Kind{ext.KindAuthProvider})); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&Entry{Manifest: oauthWizardManifest("ext.wiz3", "ext.provider1"), Kind: ext.KindStorageDriver}); err != nil {
		t.Fatalf("expected registration to succeed with a valid provider: %v", err)
	}
}

// A manifest whose wizard oauth step references its OWN id is checked against
// its own declared Kinds directly (no registry lookup, no ordering
// dependency): declaring KindAuthProvider itself is sufficient.
func TestRegisterAcceptsWizardOAuthSelfReferenceWithAuthProviderKind(t *testing.T) {
	r := NewRegistry()
	m := oauthWizardManifest("ext.selfprovider", "ext.selfprovider", ext.KindAuthProvider)
	if err := r.Register(&Entry{Manifest: m, Kind: ext.KindStorageDriver}); err != nil {
		t.Fatalf("expected self-referencing provider with KindAuthProvider to register: %v", err)
	}
}

// The same self-reference WITHOUT declaring KindAuthProvider must be
// rejected — a plugin cannot vouch for itself as a provider it never declared.
func TestRegisterRejectsWizardOAuthSelfReferenceWithoutAuthProviderKind(t *testing.T) {
	r := NewRegistry()
	m := oauthWizardManifest("ext.selfprovider2", "ext.selfprovider2") // no KindAuthProvider
	err := r.Register(&Entry{Manifest: m, Kind: ext.KindStorageDriver})
	if err == nil {
		t.Fatal("expected an error: self-referencing provider without KindAuthProvider must be rejected")
	}
}

// An oauth step with an empty Provider is refused at registration too — it can
// never be resolved, so failing fast beats a runtime-only error.
func TestRegisterRejectsWizardOAuthEmptyProvider(t *testing.T) {
	r := NewRegistry()
	m := oauthWizardManifest("ext.wiznoprovider", "")
	if err := r.Register(&Entry{Manifest: m, Kind: ext.KindStorageDriver}); err == nil {
		t.Fatal("expected an error for a wizard oauth step declaring no provider")
	}
}

// A non-oauth manifest (or one with no Wizard at all) is completely untouched
// by this validation.
func TestRegisterUnaffectedWithoutWizard(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&Entry{Manifest: ext.Manifest{ID: "ext.plain", Kinds: []ext.Kind{ext.KindStorageDriver}}, Kind: ext.KindStorageDriver}); err != nil {
		t.Fatalf("expected a plain manifest to register cleanly: %v", err)
	}
}

func TestRegisterPluginModeRejectsWizardOAuthUnknownProvider(t *testing.T) {
	r := NewRegistry()
	m := oauthWizardManifest("cn.example.wiz", "cn.example.nosuchprovider")
	if err := r.RegisterPluginMode(m, "/tmp/plugin", ExecGRPC); err == nil {
		t.Fatal("expected RegisterPluginMode to reject a wizard oauth step referencing an unregistered provider")
	}
}

func TestRegisterPluginModeAcceptsWizardOAuthValidProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(providerEntry("cn.example.provider", []ext.Kind{ext.KindAuthProvider})); err != nil {
		t.Fatal(err)
	}
	m := oauthWizardManifest("cn.example.wiz2", "cn.example.provider")
	if err := r.RegisterPluginMode(m, "/tmp/plugin", ExecGRPC); err != nil {
		t.Fatalf("expected RegisterPluginMode to succeed with a valid provider: %v", err)
	}
}

// Enable re-validates: a provider present at registration but removed while
// this entry sat disabled must block re-enabling, not silently resurrect a
// wizard that can no longer resolve its provider.
func TestEnableRejectsWizardOAuthProviderRemovedWhileDisabled(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(providerEntry("ext.willvanish", []ext.Kind{ext.KindAuthProvider})); err != nil {
		t.Fatal(err)
	}
	m := oauthWizardManifest("ext.wizdep", "ext.willvanish")
	if err := r.Register(&Entry{Manifest: m, Kind: ext.KindStorageDriver}); err != nil {
		t.Fatalf("initial register must succeed: %v", err)
	}
	if err := r.Disable("ext.wizdep"); err != nil {
		t.Fatal(err)
	}
	r.Deregister("ext.willvanish")

	if err := r.Enable("ext.wizdep"); err == nil {
		t.Fatal("expected Enable to reject a wizard oauth step whose provider was removed while disabled")
	}
}

// TestValidateWizardOAuthProvidersUnderLockNoDeadlock pins the invariant
// review round 1 established: Register/RegisterPluginMode/Enable all call
// validateWizardOAuthProviders WHILE ALREADY HOLDING r.mu, passing the
// lock-free r.getLocked — never the exported, self-locking r.Get, which would
// deadlock against Go's non-reentrant sync.RWMutex. This exercises that exact
// call shape directly (lock held, getLocked passed in) rather than only
// observing it indirectly through Register's return value, so a future
// regression that swaps getLocked back for Get would hang this test instead
// of silently reintroducing the TOCTOU the fix closed.
func TestValidateWizardOAuthProvidersUnderLockNoDeadlock(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(providerEntry("ext.lockedprovider", []ext.Kind{ext.KindAuthProvider})); err != nil {
		t.Fatal(err)
	}
	m := oauthWizardManifest("ext.lockedwiz", "ext.lockedprovider")

	done := make(chan error, 1)
	go func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		done <- validateWizardOAuthProviders(m, r.getLocked)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected success under an already-held lock, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("validateWizardOAuthProviders deadlocked when called with r.mu already held — it must use the lock-free getLocked, never r.Get")
	}
}

// TestRegisterWizardOAuthProviderConcurrentDeregisterRace stresses the TOCTOU
// window review round 1 closed: Register now validates a wizard oauth step's
// provider reference and inserts the dependent entry inside the SAME r.mu
// critical section, so a concurrent Deregister of the referenced provider can
// never interleave between the check and the insert. Run under -race: the
// original (pre-fix) shape — validate with r.Get before r.mu.Lock(), insert
// after — compiles and passes a sequential test just fine, so this hammers
// Register/Deregister of the SAME provider id from another goroutine while
// registering dependents, to have -race and the absence of a deadlock/panic
// stand in for the "hard to assert post-hoc" property that no dependent entry
// is ever inserted against a provider check that was already stale by the
// time the insert happened.
func TestRegisterWizardOAuthProviderConcurrentDeregisterRace(t *testing.T) {
	r := NewRegistry()
	const providerID = "ext.flappingprovider"

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.Register(providerEntry(providerID, []ext.Kind{ext.KindAuthProvider}))
			r.Deregister(providerID)
		}
	}()

	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("ext.wizdep.%d", i)
		m := oauthWizardManifest(id, providerID)
		// Whichever way this resolves (provider present or already flapped
		// away at lock-acquisition time), it must never deadlock or panic —
		// the error return itself is not asserted since the provider's
		// presence is inherently racing with the other goroutine.
		_ = r.Register(&Entry{Manifest: m, Kind: ext.KindStorageDriver})
	}
	close(stop)
	wg.Wait()
}

// The normal re-enable path (provider still present) still works.
func TestEnableAcceptsWizardOAuthProviderStillPresent(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(providerEntry("ext.stillhere", []ext.Kind{ext.KindAuthProvider})); err != nil {
		t.Fatal(err)
	}
	m := oauthWizardManifest("ext.wizdep2", "ext.stillhere")
	if err := r.Register(&Entry{Manifest: m, Kind: ext.KindStorageDriver}); err != nil {
		t.Fatalf("initial register must succeed: %v", err)
	}
	if err := r.Disable("ext.wizdep2"); err != nil {
		t.Fatal(err)
	}
	if err := r.Enable("ext.wizdep2"); err != nil {
		t.Fatalf("expected Enable to succeed while the provider is still registered: %v", err)
	}
}
