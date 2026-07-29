package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/spf13/cobra"
)

var pluginI18nCmd = &cobra.Command{
	Use:   "i18n --dir <plugin-dir> [--lang en,zh-CN] [--check]",
	Short: "Generate or validate a plugin's bundled i18n/<lang>.json locale files",
	Run: func(cmd *cobra.Command, args []string) {
		dir, _ := cmd.Flags().GetString("dir")
		langRaw, _ := cmd.Flags().GetString("lang")
		check, _ := cmd.Flags().GetBool("check")

		opts := pluginI18nOptions{
			Dir:   dir,
			Langs: splitCSV(langRaw),
			Check: check,
		}
		ok, err := runPluginI18n(opts, os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plugin i18n:", err)
			os.Exit(1)
		}
		if !ok {
			os.Exit(1)
		}
	},
}

func init() {
	pluginCmd.AddCommand(pluginI18nCmd)

	pluginI18nCmd.Flags().String("dir", "", "plugin directory containing plugin.json (and i18n/) [required]")
	pluginI18nCmd.Flags().String("lang", "en,zh-CN", "comma-separated locale tags to generate/check")
	pluginI18nCmd.Flags().Bool("check", false, "validate only: report missing/extra keys and exit non-zero if any key is missing, without writing files")
}

// pluginI18nOptions are the resolved `alist plugin i18n` inputs, independent
// of cobra flag parsing so runPluginI18n is directly unit-testable.
type pluginI18nOptions struct {
	Dir   string
	Langs []string
	Check bool
}

// runPluginI18n reads dir/plugin.json, derives the expected
// "field.<name>.label"/"field.<name>.help" keys from its Info.Additional
// fields (expectedKeys), and for each lang either:
//   - (default) merges them into dir/i18n/<lang>.json, preserving existing
//     translations and filling gaps (mergeLocale), then writes the file back;
//   - (--check) reports missing/extra keys against the existing file without
//     writing anything.
//
// It returns (false, nil) — not an error — when --check finds missing keys,
// matching the "non-zero exit but no exceptional failure" contract the cobra
// Run func maps to os.Exit(1). report receives progress/result lines (the
// cobra Run func passes os.Stdout; tests pass a buffer).
func runPluginI18n(opts pluginI18nOptions, report io.Writer) (ok bool, err error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return false, fmt.Errorf("--dir is required")
	}
	langs := opts.Langs
	if len(langs) == 0 {
		langs = []string{"en", "zh-CN"}
	}

	manifest, err := readManifest(dir)
	if err != nil {
		return false, err
	}
	expected := expectedKeys(manifest)
	fallback := i18nFallback(manifest)

	allOK := true
	for _, lang := range langs {
		path := filepath.Join(dir, "i18n", lang+".json")
		existing, err := readLocaleFile(path)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", path, err)
		}

		if opts.Check {
			miss := missingKeys(existing, expected)
			extra := extraKeys(existing, expected)
			if len(miss) == 0 && len(extra) == 0 {
				fmt.Fprintf(report, "%s: ok (%d keys)\n", lang, len(expected))
				continue
			}
			if len(miss) > 0 {
				allOK = false
				fmt.Fprintf(report, "%s: missing %d key(s): %s\n", lang, len(miss), strings.Join(miss, ", "))
			}
			if len(extra) > 0 {
				fmt.Fprintf(report, "%s: extra %d key(s) (no longer in manifest): %s\n", lang, len(extra), strings.Join(extra, ", "))
			}
			continue
		}

		merged := mergeLocale(existing, expected, fallback(lang))
		if err := writeLocaleFile(path, merged); err != nil {
			return false, fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Fprintf(report, "%s: wrote %d keys to %s\n", lang, len(merged), path)
	}

	return allOK, nil
}

// readManifest loads dir/plugin.json directly (json.Unmarshal into
// ext.Manifest), deliberately bypassing extension.ValidateManifest's
// core_api-compatible-with-this-host check: `plugin i18n` only needs the
// Info.Additional field list and should keep working against a plugin.json
// targeting a different host core_api version (e.g. while iterating on a
// plugin ahead of a host upgrade).
func readManifest(dir string) (ext.Manifest, error) {
	var m ext.Manifest
	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return m, fmt.Errorf("read plugin.json: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse plugin.json: %w", err)
	}
	if m.ID == "" {
		return m, fmt.Errorf("plugin.json missing id")
	}
	return m, nil
}

// expectedKeys returns the sorted, de-duplicated set of
// "field.<name>.label"/"field.<name>.help" keys a plugin's bundled locale
// files are expected to carry, one pair per manifest.Info.Additional item —
// the key convention LoadPluginLocales/resolve read (internal/extension/
// locales.go, info.go's localizeItems). A manifest with no Info (or no
// Additional fields) expects no keys.
func expectedKeys(manifest ext.Manifest) []string {
	if manifest.Info == nil || len(manifest.Info.Additional) == 0 {
		return nil
	}
	keys := make([]string, 0, len(manifest.Info.Additional)*2)
	for _, item := range manifest.Info.Additional {
		prefix := "field." + item.Name
		keys = append(keys, prefix+".label", prefix+".help")
	}
	sort.Strings(keys)
	return keys
}

// mergeLocale returns a new map containing every key of existing (so no
// bundled translation is ever silently dropped, even one for a field the
// manifest no longer declares — see extraKeys) plus every key in expected
// that existing doesn't already have, populated by fallback. existing is not
// mutated.
func mergeLocale(existing map[string]string, expected []string, fallback func(key string) string) map[string]string {
	out := make(map[string]string, len(existing)+len(expected))
	for k, v := range existing {
		out[k] = v
	}
	for _, k := range expected {
		if _, ok := out[k]; !ok {
			out[k] = fallback(k)
		}
	}
	return out
}

// missingKeys returns the expected keys absent from existing (by key
// presence, not translation non-emptiness — an explicit empty-string
// placeholder, as mergeLocale writes for non-English locales, counts as
// present), sorted.
func missingKeys(existing map[string]string, expected []string) []string {
	var out []string
	for _, k := range expected {
		if _, ok := existing[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// extraKeys returns the keys of existing that are not in expected, sorted —
// stale translations for fields the manifest no longer declares (e.g. after
// a renamed or removed Additional field). --check reports these; neither
// --check nor the default generate mode deletes them, since a rename means
// the "extra" entry may just need renaming rather than discarding.
func extraKeys(existing map[string]string, expected []string) []string {
	want := make(map[string]struct{}, len(expected))
	for _, k := range expected {
		want[k] = struct{}{}
	}
	var out []string
	for k := range existing {
		if _, ok := want[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// i18nFallback builds, for a given lang, the per-key fallback mergeLocale
// uses when a bundled locale file has no entry yet: "en" falls back to the
// field's manifest-declared Help (for a ".help" key, when non-empty) or
// otherwise the humanized field name; every other lang gets an empty-string
// placeholder, matching `alist plugin new`'s scaffoldLocale (Task 1) so a
// freshly-generated plugin round-trips through `plugin i18n` with zero
// missing keys.
func i18nFallback(manifest ext.Manifest) func(lang string) func(key string) string {
	help := make(map[string]string)
	if manifest.Info != nil {
		for _, item := range manifest.Info.Additional {
			help[item.Name] = item.Help
		}
	}
	return func(lang string) func(key string) string {
		if lang != "en" {
			return func(string) string { return "" }
		}
		return func(key string) string {
			name, kind, ok := parseFieldKey(key)
			if !ok {
				return ""
			}
			if kind == "help" {
				if h := help[name]; h != "" {
					return h
				}
			}
			return humanizeIdent(name)
		}
	}
}

// parseFieldKey splits a "field.<name>.label" or "field.<name>.help" key
// into (name, kind, true); any other shape returns ("", "", false). name may
// itself contain dots (SplitN caps at 3 parts), kind is always the final
// segment.
func parseFieldKey(key string) (name, kind string, ok bool) {
	if !strings.HasPrefix(key, "field.") {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, "field.")
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// readLocaleFile reads path as a flat string map, matching
// extension.LoadPluginLocales' per-file format. A missing file is not an
// error: it returns an empty, non-nil map (nothing bundled yet).
func readLocaleFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("not a flat string map: %w", err)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// writeLocaleFile writes m to path as indented JSON (encoding/json sorts map
// keys, so output key order is stable), creating path's parent directory if
// needed.
func writeLocaleFile(path string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
