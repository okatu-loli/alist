package extension

import (
	"testing"

	"github.com/AlistGo/alist-ext"
)

// enabledEntry builds a minimal enabled registry entry carrying the given
// contributions, for the aggregation tests.
func enabledEntry(id string, contrib ext.Contributions) *Entry {
	return &Entry{
		Manifest: ext.Manifest{
			ID:            id,
			Name:          map[string]string{"en": id},
			Kinds:         []ext.Kind{ext.KindAction},
			Contributions: contrib,
		},
		Kind:    ext.KindAction,
		Mode:    InstanceModeSingleton,
		enabled: true,
	}
}

func TestAggregateContributionsMergesEnabled(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(enabledEntry("ext.a", ext.Contributions{
		MenuActions: []ext.MenuAction{{ID: "a1", Label: map[string]string{"en": "A1"}, Target: "file"}},
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(enabledEntry("ext.b", ext.Contributions{
		DashboardCards: []ext.DashboardCard{{ID: "b1", Title: map[string]string{"en": "B1"}}},
	})); err != nil {
		t.Fatal(err)
	}

	doc := AggregateContributions(r)
	if doc.ContributionsVersion != ContributionsVersion {
		t.Fatalf("version = %d, want %d", doc.ContributionsVersion, ContributionsVersion)
	}
	if len(doc.Extensions) != 2 {
		t.Fatalf("expected 2 extensions, got %d", len(doc.Extensions))
	}
	// id-sorted: ext.a first
	if doc.Extensions[0].ID != "ext.a" || len(doc.Extensions[0].Contributions.MenuActions) != 1 {
		t.Fatalf("ext.a menu action missing: %+v", doc.Extensions[0])
	}
	if doc.Extensions[1].ID != "ext.b" || len(doc.Extensions[1].Contributions.DashboardCards) != 1 {
		t.Fatalf("ext.b dashboard card missing: %+v", doc.Extensions[1])
	}
}

// bundledEntry builds an enabled Kind=Action entry carrying contrib plus a
// loaded per-locale bundle (as RegisterPluginMode would populate from a
// plugin's on-disk i18n/*.json files) — the fixture for the channel-A-feeds-
// channel-B auto-fill tests below.
func bundledEntry(id string, contrib ext.Contributions, bundle map[string]map[string]string) *Entry {
	e := enabledEntry(id, contrib)
	e.bundle = bundle
	return e
}

// A plugin that ships i18n/zh-CN.json but never adds a matching
// Contributions.I18n entry must still have that locale surface in the
// aggregated document — channel A (the loaded bundle) auto-feeds channel B.
func TestAggregateContributionsAutoFillsUndeclaredLocale(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bundledEntry("ext.i18n.undeclared", ext.Contributions{}, map[string]map[string]string{
		"zh-CN": {"field.x.label": "X"},
	})); err != nil {
		t.Fatal(err)
	}

	doc := AggregateContributions(r)
	if len(doc.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %+v", doc.Extensions)
	}
	i18n := doc.Extensions[0].Contributions.I18n
	if len(i18n) != 1 || i18n[0].Locale != "zh-CN" || i18n[0].Route != "i18n/zh-CN.json" {
		t.Fatalf("expected auto-filled zh-CN i18n bundle, got %+v", i18n)
	}

	// The registry's OWN Manifest must be untouched — only the aggregated
	// (in-memory) copy gained the entry.
	e, _ := r.Get("ext.i18n.undeclared")
	if len(e.Manifest.Contributions.I18n) != 0 {
		t.Fatalf("registry Manifest.Contributions.I18n must not be mutated, got %+v", e.Manifest.Contributions.I18n)
	}
}

// A locale the manifest ALREADY declares (even under a custom route) must not
// be duplicated or overwritten by the bundle-derived auto-fill.
func TestAggregateContributionsDoesNotDuplicateDeclaredLocale(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bundledEntry("ext.i18n.declared", ext.Contributions{
		I18n: []ext.I18nBundle{{Locale: "zh-CN", Route: "custom/zh.json"}},
	}, map[string]map[string]string{
		"zh-CN": {"field.x.label": "X"},
		"en":    {"field.x.label": "X"},
	})); err != nil {
		t.Fatal(err)
	}

	doc := AggregateContributions(r)
	i18n := doc.Extensions[0].Contributions.I18n
	if len(i18n) != 2 {
		t.Fatalf("expected 2 entries (declared zh-CN + auto-filled en), got %+v", i18n)
	}
	var zh, en *ext.I18nBundle
	for i := range i18n {
		switch i18n[i].Locale {
		case "zh-CN":
			zh = &i18n[i]
		case "en":
			en = &i18n[i]
		}
	}
	if zh == nil || zh.Route != "custom/zh.json" {
		t.Fatalf("declared zh-CN entry must keep its own route, unduplicated: %+v", i18n)
	}
	if en == nil || en.Route != "i18n/en.json" {
		t.Fatalf("undeclared en must still be auto-filled: %+v", i18n)
	}
}

// A plugin with no loaded bundle at all (the common case — bundled locales
// are opt-in) is completely unaffected: no phantom entries appear.
func TestAggregateContributionsNoBundleNoAutoFill(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bundledEntry("ext.i18n.nobundle", ext.Contributions{}, nil)); err != nil {
		t.Fatal(err)
	}
	doc := AggregateContributions(r)
	if len(doc.Extensions[0].Contributions.I18n) != 0 {
		t.Fatalf("expected no i18n entries without a loaded bundle, got %+v", doc.Extensions[0].Contributions.I18n)
	}
}

func TestAggregateContributionsDropsDisabled(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(enabledEntry("ext.a", ext.Contributions{}))
	_ = r.Register(enabledEntry("ext.b", ext.Contributions{}))

	if got := len(AggregateContributions(r).Extensions); got != 2 {
		t.Fatalf("expected 2 before disable, got %d", got)
	}
	if err := r.Disable("ext.a"); err != nil {
		t.Fatal(err)
	}
	doc := AggregateContributions(r)
	if len(doc.Extensions) != 1 || doc.Extensions[0].ID != "ext.b" {
		t.Fatalf("after disabling ext.a, aggregate = %+v", doc.Extensions)
	}
}
