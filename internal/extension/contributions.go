package extension

import (
	"sort"

	"github.com/AlistGo/alist-ext"
)

// ContributionsVersion is the schema version of the aggregated contributions
// document. The frontend renderer and the desktop shell negotiate against it;
// bumped only on a breaking change to the aggregate shape (SDK §5.7).
const ContributionsVersion = 1

// ContributionsDoc is the versioned document served by
// GET /api/v4/extensions/contributions: the merged front-end surface of every
// enabled extension.
type ContributionsDoc struct {
	ContributionsVersion int                      `json:"contributions_version"`
	Extensions           []ExtensionContributions `json:"extensions"`
}

// ExtensionContributions is one enabled extension's contribution block, tagged
// with its id and display name so the frontend can attribute rendered UI.
type ExtensionContributions struct {
	ID            string            `json:"id"`
	Name          map[string]string `json:"name,omitempty"`
	Contributions ext.Contributions `json:"contributions"`
}

// AggregateContributions merges the Contributions declared by every enabled
// extension in r into one versioned document. Built-in drivers carry a zero
// Contributions block (an empty object), so they appear with no UI surface
// rather than being omitted — the frontend can still list them. Output is
// id-sorted (EnabledEntries) for a stable document.
//
// It also auto-feeds channel B (Contributions.I18n, the declarative locale
// bundles the frontend fetches and merges) from channel A (the plugin's
// bundled i18n/<locale>.json files, already loaded into e.bundle at
// registration — LoadPluginLocales/RegisterPluginMode — for the field-label
// localization StorageDriverInfoMapLocale reads): a plugin that ships
// i18n/zh-CN.json but never adds a matching entry to its manifest's
// Contributions.I18n would otherwise have that locale silently invisible to
// the frontend, even though the host already loaded and parsed it. This is
// STRICTLY additive per-locale (a manifest-declared locale is never touched
// or duplicated) and mutates only this in-memory aggregated copy of
// Contributions — never the registry Entry's own Manifest.
func AggregateContributions(r *Registry) ContributionsDoc {
	entries := r.EnabledEntries()
	doc := ContributionsDoc{
		ContributionsVersion: ContributionsVersion,
		Extensions:           make([]ExtensionContributions, 0, len(entries)),
	}
	for _, e := range entries {
		doc.Extensions = append(doc.Extensions, ExtensionContributions{
			ID:            e.Manifest.ID,
			Name:          e.Manifest.Name,
			Contributions: autoFillI18n(e.Manifest.Contributions, e.bundle),
		})
	}
	return doc
}

// autoFillI18n returns contrib with one ext.I18nBundle entry appended per
// locale present in bundle (a plugin's loaded i18n/*.json files) that
// contrib.I18n does not already declare. It copies contrib's I18n slice
// before appending, so the caller's original slice (and the registry's own
// Manifest, which contrib was copied FROM by the caller) is never mutated in
// place. Locales are iterated in sorted order for a stable, deterministic
// document.
func autoFillI18n(contrib ext.Contributions, bundle map[string]map[string]string) ext.Contributions {
	if len(bundle) == 0 {
		return contrib
	}
	declared := make(map[string]bool, len(contrib.I18n))
	for _, ib := range contrib.I18n {
		declared[ib.Locale] = true
	}
	locales := make([]string, 0, len(bundle))
	for locale := range bundle {
		if !declared[locale] {
			locales = append(locales, locale)
		}
	}
	if len(locales) == 0 {
		return contrib
	}
	sort.Strings(locales)
	merged := make([]ext.I18nBundle, len(contrib.I18n), len(contrib.I18n)+len(locales))
	copy(merged, contrib.I18n)
	for _, locale := range locales {
		merged = append(merged, ext.I18nBundle{Locale: locale, Route: LocaleDir + "/" + locale + ".json"})
	}
	contrib.I18n = merged
	return contrib
}
