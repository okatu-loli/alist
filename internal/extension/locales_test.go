package extension

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPluginLocalesMissingDirReturnsEmptyMap(t *testing.T) {
	dir := t.TempDir() // no i18n/ subdir at all
	got := LoadPluginLocales(dir)
	if got == nil {
		t.Fatal("missing i18n/ dir must return a non-nil empty map, not nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %+v", got)
	}
}

func TestLoadPluginLocalesParsesFlatJSONPerFile(t *testing.T) {
	dir := t.TempDir()
	i18nDir := filepath.Join(dir, "i18n")
	if err := os.MkdirAll(i18nDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(i18nDir, "en.json"), `{"field.client_id.label":"Client ID"}`)
	writeJSON(t, filepath.Join(i18nDir, "zh-CN.json"), `{"field.client_id.label":"客户端 ID","field.client_id.help":"说明"}`)
	// A non-.json file in i18n/ must be ignored, not error out the whole load.
	if err := os.WriteFile(filepath.Join(i18nDir, "README.md"), []byte("# not a locale file"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := LoadPluginLocales(dir)
	if len(got) != 2 {
		t.Fatalf("expected 2 locales (en, zh-CN), got %+v", got)
	}
	if got["en"]["field.client_id.label"] != "Client ID" {
		t.Fatalf("en bundle wrong: %+v", got["en"])
	}
	if got["zh-CN"]["field.client_id.help"] != "说明" {
		t.Fatalf("zh-CN bundle wrong: %+v", got["zh-CN"])
	}
}

func TestLoadPluginLocalesSkipsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	i18nDir := filepath.Join(dir, "i18n")
	if err := os.MkdirAll(i18nDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i18nDir, "broken.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(i18nDir, "en.json"), `{"field.x.label":"X"}`)

	got := LoadPluginLocales(dir)
	if _, ok := got["broken"]; ok {
		t.Fatal("malformed locale file must be skipped, not surfaced")
	}
	if got["en"]["field.x.label"] != "X" {
		t.Fatalf("valid sibling file must still load: %+v", got)
	}
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFallbackChain(t *testing.T) {
	bundle := map[string]map[string]string{
		"zh-CN": {"field.a.label": "甲(zh-CN)"},
		"zh":    {"field.a.label": "甲(zh)", "field.b.label": "乙(zh)"},
		"en":    {"field.a.label": "A(en)", "field.b.label": "B(en)", "field.c.label": "C(en)"},
	}

	if got := resolve(bundle, "zh-CN", "field.a.label"); got != "甲(zh-CN)" {
		t.Fatalf("zh-CN exact hit: got %q", got)
	}
	// zh-CN entry absent for "b" -> falls back to zh.
	if got := resolve(bundle, "zh-CN", "field.b.label"); got != "乙(zh)" {
		t.Fatalf("zh fallback: got %q", got)
	}
	// zh-CN and zh both absent for "c" -> falls back to en.
	if got := resolve(bundle, "zh-CN", "field.c.label"); got != "C(en)" {
		t.Fatalf("en fallback: got %q", got)
	}
	// No candidate carries "d" at all -> empty.
	if got := resolve(bundle, "zh-CN", "field.d.label"); got != "" {
		t.Fatalf("missing key must resolve empty: got %q", got)
	}
	// A locale with no bundle entries at all still fails through to en.
	if got := resolve(bundle, "fr", "field.c.label"); got != "C(en)" {
		t.Fatalf("unknown locale must still fall back to en: got %q", got)
	}
}
