package extension

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// featureManifest builds a minimal Kind=Feature manifest for a core feature
// extension (offline_download/search/share/sso). RegisterBuiltinExtension needs
// only ID + at least one Kind; the rest (name/contributions) is optional surface.
func featureManifest(id string) ext.Manifest {
	return ext.Manifest{
		ID:      id,
		Name:    map[string]string{"en": "Test Feature", "zh": "测试功能"},
		Version: "builtin",
		Kinds:   []ext.Kind{ext.KindFeature},
	}
}

// TestRegisterBuiltinExtension pins the plain built-in condition: a Kind=Feature
// entry registered through RegisterBuiltinExtension is builtin==true, enabled,
// listed in BuiltinEntries, toggles through the built-in manager, and its
// presence in AggregateContributions tracks its enabled state (disabled → gone).
func TestRegisterBuiltinExtension(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltinExtension(r, featureManifest("builtin.test")); err != nil {
		t.Fatalf("RegisterBuiltinExtension: %v", err)
	}

	e, ok := r.Get("builtin.test")
	if !ok {
		t.Fatal("builtin.test not registered")
	}
	if !e.Builtin() {
		t.Fatal("Builtin() must be true for a RegisterBuiltinExtension entry")
	}
	if !e.Enabled() {
		t.Fatal("a freshly registered built-in must start enabled")
	}
	if !e.HasKind(ext.KindFeature) {
		t.Fatal("entry must carry its primary Kind=Feature")
	}

	// Listed among built-ins while enabled and in the aggregate.
	if got := r.BuiltinEntries(); len(got) != 1 || got[0].Manifest.ID != "builtin.test" {
		t.Fatalf("BuiltinEntries must list builtin.test, got %+v", got)
	}
	if !contributionsHas(r, "builtin.test") {
		t.Fatal("enabled built-in must appear in AggregateContributions")
	}

	// Toggle through the built-in manager (registry + persistence legs).
	mgr := NewBuiltinManager(r, nil, &inMemoryDisabledStore{})
	if err := mgr.DisableBuiltin("builtin.test"); err != nil {
		t.Fatalf("DisableBuiltin: %v", err)
	}
	if e, _ := r.Get("builtin.test"); e.Enabled() {
		t.Fatal("DisableBuiltin must flip the entry disabled")
	}
	if contributionsHas(r, "builtin.test") {
		t.Fatal("disabled built-in must drop out of AggregateContributions")
	}
	// Still listed among built-ins so it can be re-enabled.
	if got := r.BuiltinEntries(); len(got) != 1 {
		t.Fatalf("disabled built-in must still be listed, got %+v", got)
	}

	if err := mgr.EnableBuiltin("builtin.test"); err != nil {
		t.Fatalf("EnableBuiltin: %v", err)
	}
	if e, _ := r.Get("builtin.test"); !e.Enabled() {
		t.Fatal("EnableBuiltin must flip the entry enabled")
	}
	if !contributionsHas(r, "builtin.test") {
		t.Fatal("re-enabled built-in must reappear in AggregateContributions")
	}
}

// TestRegisterBuiltinExtensionValidation pins the two loud failures: an empty id
// and an empty Kinds slice are both rejected — a half-declared built-in must not
// register.
func TestRegisterBuiltinExtensionValidation(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltinExtension(r, ext.Manifest{Kinds: []ext.Kind{ext.KindFeature}}); err == nil {
		t.Fatal("empty id must be rejected")
	}
	if err := RegisterBuiltinExtension(r, ext.Manifest{ID: "builtin.nokinds"}); err == nil {
		t.Fatal("empty kinds must be rejected")
	}
}

func contributionsHas(r *Registry, id string) bool {
	for _, e := range AggregateContributions(r).Extensions {
		if e.ID == id {
			return true
		}
	}
	return false
}
