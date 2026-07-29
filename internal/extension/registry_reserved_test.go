package extension

import (
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// These tests close the reserved-`builtin.`-namespace security finding: an admin
// installing a disk/third-party plugin whose manifest id collides with a trusted
// built-in id (e.g. builtin.passkey) must not overwrite the host's built-in
// Feature entry (which would silently re-enable a disabled feature and let a
// plugin impersonate a trusted id). Two defense-in-depth guards are asserted:
// (1) Register refuses a non-builtin entry that would overwrite a builtin one;
// (2) the plugin registration path rejects any `builtin.`-prefixed id up front.

// TestRegisterPluginModeRejectsBuiltinPrefix asserts the plugin path refuses a
// `builtin.`-prefixed id with a clear error and leaves any pre-existing built-in
// entry for that id untouched.
func TestRegisterPluginModeRejectsBuiltinPrefix(t *testing.T) {
	r := NewRegistry()
	// Pre-existing trusted built-in passkey feature, admin-disabled.
	if err := RegisterBuiltinExtension(r, ext.Manifest{
		ID:    "builtin.passkey",
		Kinds: []ext.Kind{ext.KindFeature},
	}); err != nil {
		t.Fatalf("register built-in passkey: %v", err)
	}
	if err := r.Disable("builtin.passkey"); err != nil {
		t.Fatalf("disable built-in passkey: %v", err)
	}

	// A third-party plugin claiming the same reserved id.
	err := r.RegisterPluginMode(
		ext.Manifest{ID: "builtin.passkey", Kinds: []ext.Kind{ext.KindStorageDriver}},
		"plugin.bin", ExecGRPC)
	if err == nil {
		t.Fatal("expected error registering plugin with reserved builtin. id, got nil")
	}
	if !strings.Contains(err.Error(), "builtin.") || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("error should mention reserved builtin. namespace, got: %v", err)
	}

	// Pre-existing built-in entry must be untouched.
	e, ok := r.Get("builtin.passkey")
	if !ok {
		t.Fatal("built-in passkey entry vanished after rejected plugin registration")
	}
	if !e.builtin {
		t.Fatal("built-in passkey entry lost its builtin==true flag")
	}
	if e.enabled {
		t.Fatal("built-in passkey re-enabled by rejected plugin registration (the exact vuln)")
	}
	if e.Kind != ext.KindFeature {
		t.Fatalf("built-in passkey Kind mutated to %v", e.Kind)
	}
}

// TestRegisterOverwriteGuardBuiltin asserts the core guard: a non-builtin entry
// cannot overwrite an existing builtin entry with the same id via Register, and
// the original built-in entry survives.
func TestRegisterOverwriteGuardBuiltin(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltinExtension(r, ext.Manifest{
		ID:    "builtin.share",
		Kinds: []ext.Kind{ext.KindFeature},
	}); err != nil {
		t.Fatalf("register built-in share: %v", err)
	}
	orig, _ := r.Get("builtin.share")

	// Attempt to overwrite with a non-builtin entry of the same id.
	imposter := &Entry{
		Manifest: ext.Manifest{ID: "builtin.share", Kinds: []ext.Kind{ext.KindStorageDriver}},
		Kind:     ext.KindStorageDriver,
		builtin:  false,
		enabled:  true,
	}
	if err := r.Register(imposter); err == nil {
		t.Fatal("expected error overwriting builtin entry with non-builtin, got nil")
	}

	got, ok := r.Get("builtin.share")
	if !ok {
		t.Fatal("built-in share entry vanished after rejected overwrite")
	}
	if got != orig {
		t.Fatal("built-in share entry was replaced by the imposter")
	}
	if !got.builtin || got.Kind != ext.KindFeature {
		t.Fatal("built-in share entry mutated after rejected overwrite")
	}
}

// TestRegisterBuiltinOverBuiltinAllowed is the regression guard: a built-in
// registering its own `builtin.*` id at boot (builtin==true both sides) succeeds.
func TestRegisterBuiltinOverBuiltinAllowed(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltinExtension(r, ext.Manifest{
		ID:    "builtin.webdav",
		Kinds: []ext.Kind{ext.KindFeature},
	}); err != nil {
		t.Fatalf("first built-in webdav registration failed: %v", err)
	}
	// Re-register the same built-in id with builtin==true — must be allowed
	// (idempotent boot / re-register).
	if err := RegisterBuiltinExtension(r, ext.Manifest{
		ID:    "builtin.webdav",
		Kinds: []ext.Kind{ext.KindFeature},
	}); err != nil {
		t.Fatalf("re-registering built-in over built-in should be allowed: %v", err)
	}
}

// TestRegisterPluginUpdateAllowed is the don't-over-block guard: a non-builtin
// entry replacing an existing non-builtin entry with the same id (a plugin
// update) still succeeds.
func TestRegisterPluginUpdateAllowed(t *testing.T) {
	r := NewRegistry()
	m := ext.Manifest{ID: "com.example.demo", Kinds: []ext.Kind{ext.KindStorageDriver}}
	if err := r.RegisterPluginMode(m, "v1.bin", ExecGRPC); err != nil {
		t.Fatalf("initial plugin registration: %v", err)
	}
	// Update to a new version — same id, still non-builtin.
	if err := r.RegisterPluginMode(m, "v2.bin", ExecGRPC); err != nil {
		t.Fatalf("plugin update (non-builtin over non-builtin) should be allowed: %v", err)
	}
	e, ok := r.Get("com.example.demo")
	if !ok {
		t.Fatal("plugin entry missing after update")
	}
	if e.binPath != "v2.bin" {
		t.Fatalf("plugin update did not take effect, binPath=%q", e.binPath)
	}
}

// TestRegisterPluginNormalIDAllowed asserts a plugin with a normal id registers
// fine (the guards do not affect the common path).
func TestRegisterPluginNormalIDAllowed(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterPluginMode(
		ext.Manifest{ID: "com.example.demo", Kinds: []ext.Kind{ext.KindStorageDriver}},
		"plugin.bin", ExecGRPC); err != nil {
		t.Fatalf("normal plugin id should register: %v", err)
	}
	if _, ok := r.Get("com.example.demo"); !ok {
		t.Fatal("normal plugin id not registered")
	}
}
