package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// LocaleDir is the fixed subdirectory name, inside a plugin's version dir,
// where bundled locale files live: <DataDir>/plugins/<id>/<version>/i18n/.
const LocaleDir = "i18n"

// DefaultLocale is the locale a plugin's driver.Info is built with when no
// request-specific locale is available (e.g. the Entry.info cached at
// registration time, built once for callers that don't need per-locale
// output). Matches the pre-i18n behavior, where every plugin Additional
// item's Help was always its manifest-declared English text.
const DefaultLocale = "en"

// LoadPluginLocales reads a plugin's bundled locale files from dir/i18n/*.json
// (SDK "per-plugin bundled locales" convention, plan doc ②). Each file's
// basename with the .json suffix stripped is the locale tag it carries (e.g.
// "en.json" → "en", "zh-CN.json" → "zh-CN"); the file content is a flat
// key→translation map using the "field.<name>.label" / "field.<name>.help"
// key convention. A plugin that ships no i18n/ directory — the common case,
// since bundled locales are opt-in — returns an empty, non-nil map; this is
// NOT an error. A file that fails to parse is skipped rather than aborting the
// whole load, so one malformed locale file cannot take down every other
// locale (or the plugin's registration).
func LoadPluginLocales(dir string) map[string]map[string]string {
	out := make(map[string]map[string]string)
	entries, err := os.ReadDir(filepath.Join(dir, LocaleDir))
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		lang := strings.TrimSuffix(name, ".json")
		if lang == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, LocaleDir, name))
		if err != nil {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out[lang] = m
	}
	return out
}

// resolve looks up key in bundle, walking the fallback chain for locale (see
// localeCandidates), and returns the first non-empty translation found. It
// returns "" when no candidate locale in bundle carries a non-empty value for
// key — callers treat that as "no bundled translation" and keep whatever
// manifest-declared (English) default they already have.
func resolve(bundle map[string]map[string]string, locale, key string) string {
	for _, l := range localeCandidates(locale) {
		if m, ok := bundle[l]; ok {
			if v, ok := m[key]; ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// localeCandidates returns the locale fallback chain to try, in order: locale
// itself, then its primary subtag if it carries a region (e.g. "zh-CN" → "zh"),
// then "en" as the final fallback (omitted if locale already is "en", so the
// chain never repeats). This is the general form of the plan's documented
// zh-CN → zh → en → (manifest English default) chain.
func localeCandidates(locale string) []string {
	var out []string
	if locale != "" {
		out = append(out, locale)
		if i := strings.IndexByte(locale, '-'); i >= 0 {
			if primary := locale[:i]; primary != "" && primary != locale {
				out = append(out, primary)
			}
		}
	}
	if locale != "en" {
		out = append(out, "en")
	}
	return out
}
