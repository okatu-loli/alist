package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// writePluginJSON writes a minimal but ValidateManifest-shaped plugin.json
// (info.additional only — the surface expectedKeys reads) to dir.
func writePluginJSON(t *testing.T, dir string, items []ext.Item) {
	t.Helper()
	m := ext.Manifest{
		ID:      "com.example.mydrive",
		Name:    map[string]string{"en": "My Drive", "zh": "我的网盘"},
		Version: "0.1.0",
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Info:    &ext.Info{Additional: items},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLocale(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func TestExpectedKeys(t *testing.T) {
	m := ext.Manifest{Info: &ext.Info{Additional: []ext.Item{
		{Name: "client_id"}, {Name: "root_folder_id"},
	}}}
	got := expectedKeys(m)
	want := []string{
		"field.client_id.help", "field.client_id.label",
		"field.root_folder_id.help", "field.root_folder_id.label",
	}
	if len(got) != len(want) {
		t.Fatalf("expectedKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("expectedKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExpectedKeysNilInfo(t *testing.T) {
	if got := expectedKeys(ext.Manifest{}); got != nil {
		t.Errorf("expectedKeys(no Info) = %v, want nil", got)
	}
}

func TestMergeLocalePreservesExistingAndFillsMissing(t *testing.T) {
	existing := map[string]string{
		"field.client_id.label": "Client ID (custom)",
	}
	expected := []string{"field.client_id.label", "field.client_id.help"}
	fallback := func(key string) string { return "FALLBACK:" + key }

	got := mergeLocale(existing, expected, fallback)

	if got["field.client_id.label"] != "Client ID (custom)" {
		t.Errorf("existing translation was not preserved: %q", got["field.client_id.label"])
	}
	if got["field.client_id.help"] != "FALLBACK:field.client_id.help" {
		t.Errorf("missing key was not filled via fallback: %q", got["field.client_id.help"])
	}
}

func TestMergeLocaleKeepsStaleKeys(t *testing.T) {
	existing := map[string]string{"field.removed_field.label": "stale but kept"}
	got := mergeLocale(existing, nil, func(string) string { return "" })
	if got["field.removed_field.label"] != "stale but kept" {
		t.Errorf("mergeLocale must not drop keys absent from expected: got %v", got)
	}
}

func TestMissingKeys(t *testing.T) {
	existing := map[string]string{"field.a.label": "A", "field.a.help": ""}
	expected := []string{"field.a.label", "field.a.help", "field.b.label"}
	got := missingKeys(existing, expected)
	if len(got) != 1 || got[0] != "field.b.label" {
		t.Errorf("missingKeys = %v, want [field.b.label]", got)
	}
}

func TestMissingKeysTreatsEmptyStringAsPresent(t *testing.T) {
	existing := map[string]string{"field.a.label": ""}
	got := missingKeys(existing, []string{"field.a.label"})
	if len(got) != 0 {
		t.Errorf("an explicit empty-string placeholder should count as present, got missing=%v", got)
	}
}

func TestExtraKeys(t *testing.T) {
	existing := map[string]string{"field.a.label": "A", "field.gone.label": "G"}
	expected := []string{"field.a.label"}
	got := extraKeys(existing, expected)
	if len(got) != 1 || got[0] != "field.gone.label" {
		t.Errorf("extraKeys = %v, want [field.gone.label]", got)
	}
}

func TestParseFieldKey(t *testing.T) {
	name, kind, ok := parseFieldKey("field.root_folder_id.help")
	if !ok || name != "root_folder_id" || kind != "help" {
		t.Errorf("parseFieldKey = (%q, %q, %v), want (root_folder_id, help, true)", name, kind, ok)
	}
	if _, _, ok := parseFieldKey("not-a-field-key"); ok {
		t.Error("parseFieldKey should reject keys without the field. prefix")
	}
}

func TestRunPluginI18nGeneratesAllExpectedKeysAndPreservesTranslations(t *testing.T) {
	dir := t.TempDir()
	writePluginJSON(t, dir, []ext.Item{
		{Name: "client_id", Help: "The client id"},
		{Name: "root_folder_id"},
	})
	// Pre-seed en.json with one real translation; i18n should keep it.
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "i18n", "en.json"),
		[]byte(`{"field.client_id.label":"Client ID"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	ok, err := runPluginI18n(pluginI18nOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginI18n: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true for generate mode, output:\n%s", out.String())
	}

	en := readLocale(t, filepath.Join(dir, "i18n", "en.json"))
	if en["field.client_id.label"] != "Client ID" {
		t.Errorf("pre-seeded translation was not preserved: %q", en["field.client_id.label"])
	}
	if en["field.client_id.help"] != "The client id" {
		t.Errorf("en help should fall back to manifest Help: %q", en["field.client_id.help"])
	}
	if en["field.root_folder_id.label"] != "Root Folder Id" {
		t.Errorf("en label should fall back to humanized field name: %q", en["field.root_folder_id.label"])
	}
	for _, key := range []string{"field.client_id.label", "field.client_id.help", "field.root_folder_id.label", "field.root_folder_id.help"} {
		if _, ok := en[key]; !ok {
			t.Errorf("en.json missing expected key %q", key)
		}
	}

	zh := readLocale(t, filepath.Join(dir, "i18n", "zh-CN.json"))
	for _, key := range []string{"field.client_id.label", "field.client_id.help", "field.root_folder_id.label", "field.root_folder_id.help"} {
		v, ok := zh[key]
		if !ok {
			t.Errorf("zh-CN.json missing expected key %q", key)
		}
		if v != "" {
			t.Errorf("zh-CN.json[%q] = %q, want empty placeholder (untranslated)", key, v)
		}
	}
}

func TestRunPluginI18nCheckReportsMissingAndFailsNonZero(t *testing.T) {
	dir := t.TempDir()
	writePluginJSON(t, dir, []ext.Item{{Name: "client_id"}})
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Missing field.client_id.help entirely.
	if err := os.WriteFile(filepath.Join(dir, "i18n", "en.json"),
		[]byte(`{"field.client_id.label":"Client ID"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	ok, err := runPluginI18n(pluginI18nOptions{Dir: dir, Langs: []string{"en"}, Check: true}, &out)
	if err != nil {
		t.Fatalf("runPluginI18n: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when a key is missing")
	}
	if !bytes.Contains(out.Bytes(), []byte("field.client_id.help")) {
		t.Errorf("--check output should name the missing key, got:\n%s", out.String())
	}

	// --check must not have written the file.
	b, _ := os.ReadFile(filepath.Join(dir, "i18n", "en.json"))
	if bytes.Contains(b, []byte("field.client_id.help")) {
		t.Error("--check must not modify the locale file on disk")
	}
}

func TestRunPluginI18nCheckPassesWhenComplete(t *testing.T) {
	dir := t.TempDir()
	writePluginJSON(t, dir, []ext.Item{{Name: "client_id"}})
	// First, generate.
	if _, err := runPluginI18n(pluginI18nOptions{Dir: dir, Langs: []string{"en"}}, &bytes.Buffer{}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	var out bytes.Buffer
	ok, err := runPluginI18n(pluginI18nOptions{Dir: dir, Langs: []string{"en"}, Check: true}, &out)
	if err != nil {
		t.Fatalf("runPluginI18n: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true for a complete locale file, output:\n%s", out.String())
	}
}

func TestRunPluginI18nCheckReportsExtraKeys(t *testing.T) {
	dir := t.TempDir()
	writePluginJSON(t, dir, []ext.Item{{Name: "client_id"}})
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "i18n", "en.json"), []byte(`{
		"field.client_id.label": "Client ID",
		"field.client_id.help": "help text",
		"field.removed_field.label": "stale"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	ok, err := runPluginI18n(pluginI18nOptions{Dir: dir, Langs: []string{"en"}, Check: true}, &out)
	if err != nil {
		t.Fatalf("runPluginI18n: %v", err)
	}
	if !ok {
		t.Errorf("extra keys alone should not fail --check (only missing keys do), output:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("field.removed_field.label")) {
		t.Errorf("--check output should name the extra key, got:\n%s", out.String())
	}
}

func TestRunPluginI18nRequiresDir(t *testing.T) {
	if _, err := runPluginI18n(pluginI18nOptions{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error when --dir is empty")
	}
}

// TestPluginNewOutputPassesI18nCheck locks in the cross-task contract: a
// freshly scaffolded plugin's i18n/*.json (Task 1's scaffoldLocale) must
// already satisfy `plugin i18n --check` (Task 2's expectedKeys/missingKeys)
// with zero missing keys, without ever running `plugin i18n` on it.
func TestPluginNewOutputPassesI18nCheck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mydrive")
	if err := runPluginNew(pluginNewOptions{
		ID:     "com.example.mydrive",
		Dir:    dir,
		Fields: []string{"api_key", "root_folder_id"},
	}); err != nil {
		t.Fatalf("runPluginNew: %v", err)
	}

	var out bytes.Buffer
	ok, err := runPluginI18n(pluginI18nOptions{Dir: dir, Check: true}, &out)
	if err != nil {
		t.Fatalf("runPluginI18n --check: %v", err)
	}
	if !ok {
		t.Errorf("a freshly scaffolded plugin should pass `plugin i18n --check`, output:\n%s", out.String())
	}
}
