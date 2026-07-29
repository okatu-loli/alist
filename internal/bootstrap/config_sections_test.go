package bootstrap

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/offline_download/aria2"
	"github.com/alist-org/alist/v3/internal/offline_download/qbittorrent"
	"github.com/alist-org/alist/v3/internal/offline_download/transmission"
)

// configSectionsOf returns extension extID's contributed config sections from the
// aggregate contributions document (empty if the extension is absent/disabled).
func configSectionsOf(doc extension.ContributionsDoc, extID string) []ext.ConfigSection {
	for _, e := range doc.Extensions {
		if e.ID == extID {
			return e.Contributions.ConfigSections
		}
	}
	return nil
}

// sectionsForParent gathers every ConfigSection across the whole aggregate
// document whose Parent == parent — this is exactly the set the frontend's
// FeatureConfigPage(parent) renders (base general section + each enabled child).
func sectionsForParent(doc extension.ContributionsDoc, parent string) []ext.ConfigSection {
	var out []ext.ConfigSection
	for _, e := range doc.Extensions {
		for _, s := range e.Contributions.ConfigSections {
			if s.Parent == parent {
				out = append(out, s)
			}
		}
	}
	return out
}

// TestOfflineDownloadGeneralConfigSection pins the C1 contract for the base: the
// built-in offline_download feature declares its own "general" ConfigSection with
// Parent=self and the four shared setting keys, in order 0.
func TestOfflineDownloadGeneralConfigSection(t *testing.T) {
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

	e, ok := extension.DefaultRegistry.Get("builtin.offline_download")
	if !ok {
		t.Fatal("builtin.offline_download must be registered")
	}
	secs := e.Manifest.Contributions.ConfigSections
	if len(secs) != 1 {
		t.Fatalf("builtin.offline_download must declare exactly one config section, got %+v", secs)
	}
	s := secs[0]
	if s.Parent != "builtin.offline_download" {
		t.Fatalf("base general section Parent = %q, want builtin.offline_download (self)", s.Parent)
	}
	if s.Title["en"] != "General" || s.Title["zh"] != "通用配置" {
		t.Fatalf("base general section title = %+v, want General/通用配置", s.Title)
	}
	if s.Order != 0 {
		t.Fatalf("base general section order = %d, want 0", s.Order)
	}
	wantKeys := []string{
		"offline_download_tool",
		"offline_download_task_threads_num",
		"offline_download_transfer_threads_num",
		"offline_download_max_size_gb",
	}
	if len(s.SettingKeys) != len(wantKeys) {
		t.Fatalf("base general section keys = %v, want %v", s.SettingKeys, wantKeys)
	}
	for i, k := range wantKeys {
		if s.SettingKeys[i] != k {
			t.Fatalf("base general section key[%d] = %q, want %q", i, s.SettingKeys[i], k)
		}
	}

	// The section flows through AggregateContributions unchanged.
	if got := configSectionsOf(extension.AggregateContributions(extension.DefaultRegistry), "builtin.offline_download"); len(got) != 1 || got[0].Parent != "builtin.offline_download" {
		t.Fatalf("enabled base must surface its general section in AggregateContributions, got %+v", got)
	}
}

// TestOfflineDownloadToolConfigSections pins the C1 contract for the three tools:
// each declares a ConfigSection with Parent="builtin.offline_download" and its own
// setting keys, in ascending order (aria2=1, qbit=2, transmission=3).
func TestOfflineDownloadToolConfigSections(t *testing.T) {
	t.Cleanup(cleanupOfflineDownloadTools)

	InitOfflineDownloadTools()

	cases := []struct {
		extID   string
		titleEN string
		titleZH string
		keys    []string
		order   int
	}{
		{aria2.ExtensionID, "aria2", "aria2", []string{"aria2_uri", "aria2_secret"}, 1},
		{qbittorrent.ExtensionID, "qBittorrent", "qBittorrent", []string{"qbittorrent_url", "qbittorrent_seedtime"}, 2},
		{transmission.ExtensionID, "Transmission", "Transmission", []string{"transmission_uri", "transmission_seedtime"}, 3},
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
		if s.Parent != "builtin.offline_download" {
			t.Errorf("%s section Parent = %q, want builtin.offline_download", tc.extID, s.Parent)
		}
		if s.Title["en"] != tc.titleEN || s.Title["zh"] != tc.titleZH {
			t.Errorf("%s section title = %+v, want %s/%s", tc.extID, s.Title, tc.titleEN, tc.titleZH)
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

		// C3: the tool no longer contributes a standalone settings page — its
		// config is a section on the parent feature's config page, reached via the
		// tool detail's parent-jump. Only the config_section remains.
		if len(e.Manifest.Contributions.SettingsPages) != 0 {
			t.Errorf("%s must not declare a SettingsPages contribution (config lives on the parent feature page), got %+v", tc.extID, e.Manifest.Contributions.SettingsPages)
		}
	}
}

// TestOfflineDownloadConfigSectionsAggregate pins the whole-page C1 contract: with
// the base + all three tools enabled, FeatureConfigPage("builtin.offline_download")
// sees the base's general section AND each tool's section, all with Parent=base.
// Disabling a tool extension removes its section from the merged document
// (contract membership == enabled); re-enabling restores it.
func TestOfflineDownloadConfigSectionsAggregate(t *testing.T) {
	coreIDs := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range coreIDs {
			extension.DefaultRegistry.Deregister(id)
		}
	})
	t.Cleanup(cleanupOfflineDownloadTools)

	InitCoreFeatureExtensions()
	InitOfflineDownloadTools()

	const parent = "builtin.offline_download"

	// hasSectionTitled reports whether the merged doc carries a Parent==base
	// section with the given English title.
	hasSectionTitled := func(en string) bool {
		for _, s := range sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent) {
			if s.Title["en"] == en {
				return true
			}
		}
		return false
	}

	// All enabled → base general + 3 tool sections all attribute to the base.
	got := sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent)
	if len(got) != 4 {
		t.Fatalf("all enabled: FeatureConfigPage(base) must see 4 sections, got %d: %+v", len(got), got)
	}
	for _, want := range []string{"General", "aria2", "qBittorrent", "Transmission"} {
		if !hasSectionTitled(want) {
			t.Fatalf("all enabled: missing %q section for parent %s", want, parent)
		}
	}

	// Disable aria2 → its section drops; base + the other two remain.
	if err := extension.DefaultRegistry.Disable(aria2.ExtensionID); err != nil {
		t.Fatalf("disable aria2: %v", err)
	}
	if hasSectionTitled("aria2") {
		t.Fatal("disabled aria2 must not surface its config section")
	}
	if got := sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent); len(got) != 3 {
		t.Fatalf("after disabling aria2: want 3 sections, got %d: %+v", len(got), got)
	}
	if !hasSectionTitled("General") || !hasSectionTitled("qBittorrent") || !hasSectionTitled("Transmission") {
		t.Fatal("disabling aria2 must not affect the base/qbit/transmission sections")
	}

	// Re-enable aria2 → its section returns.
	if err := extension.DefaultRegistry.Enable(aria2.ExtensionID); err != nil {
		t.Fatalf("enable aria2: %v", err)
	}
	if !hasSectionTitled("aria2") {
		t.Fatal("re-enabled aria2 must restore its config section")
	}
	if got := sectionsForParent(extension.AggregateContributions(extension.DefaultRegistry), parent); len(got) != 4 {
		t.Fatalf("after re-enabling aria2: want 4 sections, got %d: %+v", len(got), got)
	}
}
