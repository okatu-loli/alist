package labels

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

// labelsSettingsPage returns the built-in labels bundle's contributed native
// settings page from the aggregate contributions document, or false if the
// extension (or the page) is absent.
func labelsSettingsPage(doc extension.ContributionsDoc) (ext.SettingsPage, bool) {
	for _, e := range doc.Extensions {
		if e.ID != BundleID {
			continue
		}
		for _, p := range e.Contributions.SettingsPages {
			if p.ID == "labels" {
				return p, true
			}
		}
	}
	return ext.SettingsPage{}, false
}

// TestLabelsContributesNativeSettingsPage pins the W1 contract: the built-in
// labels bundle declares a settings_pages entry pointing at its native host
// management page (/@manage/...) ALONGSIDE its existing FileColumn/MenuAction
// contributions, so the merged contributions document surfaces it while the
// bundle is ENABLED and drops the whole extension when it is DISABLED.
func TestLabelsContributesNativeSettingsPage(t *testing.T) {
	// The manifest carries the new settings page without dropping the existing
	// FileColumn/MenuAction contributions (merge, not overwrite).
	m := instance.Manifest()
	if len(m.Contributions.FileColumns) != 1 {
		t.Fatalf("labels manifest must keep its FileColumn, got %+v", m.Contributions.FileColumns)
	}
	if len(m.Contributions.MenuActions) != 1 {
		t.Fatalf("labels manifest must keep its MenuAction, got %+v", m.Contributions.MenuActions)
	}
	if len(m.Contributions.SettingsPages) != 1 {
		t.Fatalf("labels manifest must declare exactly one settings page, got %+v", m.Contributions.SettingsPages)
	}

	r := extension.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Enabled → the aggregate document surfaces the native page.
	p, ok := labelsSettingsPage(extension.AggregateContributions(r))
	if !ok {
		t.Fatal("enabled labels bundle must surface its settings page in AggregateContributions")
	}
	if p.Route != "/@manage/extensions/labels" {
		t.Fatalf("labels settings page route = %q, want /@manage/extensions/labels", p.Route)
	}
	if p.Title["en"] != "Labels" || p.Title["zh"] != "标签" {
		t.Fatalf("labels settings page title = %+v, want bilingual Labels/标签", p.Title)
	}

	// Disabled → the extension (and thus its settings page) drops out entirely.
	if err := r.Disable(BundleID); err != nil {
		t.Fatalf("Disable %s: %v", BundleID, err)
	}
	if _, ok := labelsSettingsPage(extension.AggregateContributions(r)); ok {
		t.Fatal("disabled labels bundle must not surface its settings page")
	}
}
