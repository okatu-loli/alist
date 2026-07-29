package bootstrap

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/search/bleve"
	"github.com/alist-org/alist/v3/internal/search/database"
	"github.com/alist-org/alist/v3/internal/search/meilisearch"
)

// TestSearchBaseGeneralConfigSection pins the C4 contract for the search base:
// builtin.search declares a "general" ConfigSection with Parent=self and the four
// shared, backend-agnostic setting keys, in order 0 — mirroring offline_download's
// general section (C1).
func TestSearchBaseGeneralConfigSection(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
	})

	InitCoreFeatureExtensions()

	e, ok := extension.DefaultRegistry.Get("builtin.search")
	if !ok {
		t.Fatal("builtin.search must be registered")
	}
	secs := e.Manifest.Contributions.ConfigSections
	if len(secs) != 1 {
		t.Fatalf("builtin.search must declare exactly one config section, got %+v", secs)
	}
	s := secs[0]
	if s.Parent != "builtin.search" {
		t.Fatalf("base general section Parent = %q, want builtin.search (self)", s.Parent)
	}
	if s.Title["en"] != "General" || s.Title["zh"] != "通用配置" {
		t.Fatalf("base general section title = %+v, want General/通用配置", s.Title)
	}
	if s.Order != 0 {
		t.Fatalf("base general section order = %d, want 0", s.Order)
	}
	wantKeys := []string{"search_index", "auto_update_index", "search_max_depth", "search_result_limit"}
	if len(s.SettingKeys) != len(wantKeys) {
		t.Fatalf("base general section keys = %v, want %v", s.SettingKeys, wantKeys)
	}
	for i, k := range wantKeys {
		if s.SettingKeys[i] != k {
			t.Fatalf("base general section key[%d] = %q, want %q", i, s.SettingKeys[i], k)
		}
	}
}

// TestSearchBackendConfigSections pins the C4 contract for the three backends:
// bleve and meilisearch each declare a ConfigSection with Parent="builtin.search"
// and their own DB setting keys (in ascending order 2/3); the database backend
// declares NONE (no connection config). None of the three contribute a
// SettingsPages link any more — the search base feature owns the single page.
func TestSearchBackendConfigSections(t *testing.T) {
	t.Cleanup(cleanupSearchBackends)

	InitSearchBackends()

	// database: no config section, no settings page.
	dbE, ok := extension.DefaultRegistry.Get(database.ExtensionID)
	if !ok {
		t.Fatalf("%s must be registered", database.ExtensionID)
	}
	if len(dbE.Manifest.Contributions.ConfigSections) != 0 {
		t.Fatalf("%s must declare no config section, got %+v", database.ExtensionID, dbE.Manifest.Contributions.ConfigSections)
	}
	if len(dbE.Manifest.Contributions.SettingsPages) != 0 {
		t.Fatalf("%s must declare no settings page, got %+v", database.ExtensionID, dbE.Manifest.Contributions.SettingsPages)
	}

	cases := []struct {
		extID string
		title string
		keys  []string
		order int
	}{
		{bleve.ExtensionID, "Bleve", []string{"bleve_index_path"}, 2},
		{meilisearch.ExtensionID, "Meilisearch", []string{"meilisearch_host", "meilisearch_api_key", "meilisearch_index_prefix"}, 3},
	}
	for _, tc := range cases {
		e, ok := extension.DefaultRegistry.Get(tc.extID)
		if !ok {
			t.Fatalf("%s must be registered", tc.extID)
		}
		secs := e.Manifest.Contributions.ConfigSections
		if len(secs) != 1 {
			t.Fatalf("%s must declare exactly one config section, got %+v", tc.extID, secs)
		}
		s := secs[0]
		if s.Parent != "builtin.search" {
			t.Errorf("%s section Parent = %q, want builtin.search", tc.extID, s.Parent)
		}
		if s.Title["en"] != tc.title || s.Title["zh"] != tc.title {
			t.Errorf("%s section title = %+v, want %s/%s", tc.extID, s.Title, tc.title, tc.title)
		}
		if s.Order != tc.order {
			t.Errorf("%s section order = %d, want %d", tc.extID, s.Order, tc.order)
		}
		if len(s.SettingKeys) != len(tc.keys) {
			t.Fatalf("%s section keys = %v, want %v", tc.extID, s.SettingKeys, tc.keys)
		}
		for i, k := range tc.keys {
			if s.SettingKeys[i] != k {
				t.Errorf("%s section key[%d] = %q, want %q", tc.extID, i, s.SettingKeys[i], k)
			}
		}
		// C4: the backend no longer contributes a standalone settings page — its
		// config is a section on the search base feature's config page.
		if len(e.Manifest.Contributions.SettingsPages) != 0 {
			t.Errorf("%s must not declare a SettingsPages contribution, got %+v", tc.extID, e.Manifest.Contributions.SettingsPages)
		}
	}
}

// TestSearchConfigSectionsAggregate pins the whole-page C4 contract: with the
// search base + bleve + meilisearch enabled, FeatureConfigPage("builtin.search")
// sees the base's general section AND the bleve/meilisearch sections (database
// contributes none), all attributing to the base. Disabling meilisearch drops its
// section from the merged document (contract membership == enabled); re-enabling
// restores it.
func TestSearchConfigSectionsAggregate(t *testing.T) {
	coreIDs := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range coreIDs {
			extension.DefaultRegistry.Deregister(id)
		}
	})
	t.Cleanup(cleanupSearchBackends)

	InitCoreFeatureExtensions()
	InitSearchBackends()

	const parent = "builtin.search"

	hasSectionTitled := func(en string) bool {
		for _, s := range sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent) {
			if s.Title["en"] == en {
				return true
			}
		}
		return false
	}

	// All enabled → base general + bleve + meilisearch sections (database none).
	got := sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent)
	if len(got) != 3 {
		t.Fatalf("all enabled: FeatureConfigPage(search) must see 3 sections, got %d: %+v", len(got), got)
	}
	for _, want := range []string{"General", "Bleve", "Meilisearch"} {
		if !hasSectionTitled(want) {
			t.Fatalf("all enabled: missing %q section for parent %s", want, parent)
		}
	}
	// database contributes no section to the page.
	if got := configSectionsOf(extension.AggregateContributions(extension.DefaultRegistry), database.ExtensionID); len(got) != 0 {
		t.Fatalf("database backend must contribute no config section, got %+v", got)
	}

	// Disable meilisearch → its section drops; base + bleve remain.
	if err := extension.DefaultRegistry.Disable(meilisearch.ExtensionID); err != nil {
		t.Fatalf("disable meilisearch: %v", err)
	}
	if hasSectionTitled("Meilisearch") {
		t.Fatal("disabled meilisearch must not surface its config section")
	}
	if got := sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent); len(got) != 2 {
		t.Fatalf("after disabling meilisearch: want 2 sections, got %d: %+v", len(got), got)
	}
	if !hasSectionTitled("General") || !hasSectionTitled("Bleve") {
		t.Fatal("disabling meilisearch must not affect the base/bleve sections")
	}

	// Re-enable meilisearch → its section returns.
	if err := extension.DefaultRegistry.Enable(meilisearch.ExtensionID); err != nil {
		t.Fatalf("enable meilisearch: %v", err)
	}
	if !hasSectionTitled("Meilisearch") {
		t.Fatal("re-enabled meilisearch must restore its config section")
	}
	if got := sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent); len(got) != 3 {
		t.Fatalf("after re-enabling meilisearch: want 3 sections, got %d: %+v", len(got), got)
	}
}
