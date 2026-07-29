package cmd

import (
	"encoding/json"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

func TestRunPluginNewGeneratesExpectedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mydrive")
	opts := pluginNewOptions{
		ID:      "com.example.mydrive",
		Dir:     dir,
		Fields:  []string{"api_key", "root_folder_id"},
		ExtPath: "../ext",
	}
	if err := runPluginNew(opts); err != nil {
		t.Fatalf("runPluginNew: %v", err)
	}

	wantFiles := []string{
		"main.go", "driver.go", "plugin.json",
		"i18n/en.json", "i18n/zh-CN.json",
		"pack.sh", "serve.sh", "README.md", ".gitignore", "go.mod",
	}
	for _, rel := range wantFiles {
		p := filepath.Join(dir, rel)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("expected file %s to exist: %v", rel, err)
		}
		if info.Size() == 0 {
			t.Fatalf("expected file %s to be non-empty", rel)
		}
	}

	// Scripts must be executable.
	for _, rel := range []string{"pack.sh", "serve.sh"} {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s should be executable, got mode %v", rel, info.Mode())
		}
	}
}

func TestRunPluginNewRefusesNonEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runPluginNew(pluginNewOptions{ID: "com.example.mydrive", Dir: dir})
	if err == nil {
		t.Fatal("expected an error scaffolding into a non-empty directory, got nil")
	}
}

func TestRunPluginNewRequiresID(t *testing.T) {
	err := runPluginNew(pluginNewOptions{Dir: filepath.Join(t.TempDir(), "x")})
	if err == nil {
		t.Fatal("expected an error when --id is empty, got nil")
	}
}

// TestPluginNewGoFilesParseAndFormat asserts the generated .go files are
// go/parser-parseable and gofmt-clean (format.Source is idempotent on them),
// independent of whether a full `go build` can succeed offline.
func TestPluginNewGoFilesParseAndFormat(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mydrive")
	opts := pluginNewOptions{
		ID:     "com.example.mydrive",
		Dir:    dir,
		Fields: []string{"api_key", "root_folder_id"},
	}
	if err := runPluginNew(opts); err != nil {
		t.Fatalf("runPluginNew: %v", err)
	}

	for _, rel := range []string{"main.go", "driver.go"} {
		p := filepath.Join(dir, rel)
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		fset := token.NewFileSet()
		if _, err := parser.ParseFile(fset, p, src, parser.AllErrors); err != nil {
			t.Fatalf("%s: go/parser.ParseFile failed: %v", rel, err)
		}

		formatted, err := format.Source(src)
		if err != nil {
			t.Fatalf("%s: format.Source failed: %v", rel, err)
		}
		if string(formatted) != string(src) {
			t.Errorf("%s: on-disk content is not gofmt-clean (format.Source changed it)", rel)
		}

		// format.Source should be idempotent: formatting twice == formatting once.
		formattedAgain, err := format.Source(formatted)
		if err != nil {
			t.Fatalf("%s: second format.Source failed: %v", rel, err)
		}
		if string(formattedAgain) != string(formatted) {
			t.Errorf("%s: format.Source is not idempotent", rel)
		}
	}
}

// TestPluginNewManifestMatchesPluginJSON decodes the generated plugin.json
// and compares it against buildScaffoldManifest's output — the same data
// driver.go.tmpl renders a Go literal of — with reflect.DeepEqual. This is
// the drift guard demo-plugin/123open-plugin's hand-written tests use
// (driver_test.go's TestManifestMatchesPluginJSON), applied to the generator.
func TestPluginNewManifestMatchesPluginJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mydrive")
	opts := pluginNewOptions{
		ID:     "com.example.mydrive",
		Name:   "My Drive",
		Dir:    dir,
		Fields: []string{"api_key", "root_folder_id"},
	}
	if err := runPluginNew(opts); err != nil {
		t.Fatalf("runPluginNew: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var got ext.Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}

	if got.ID != "com.example.mydrive" {
		t.Errorf("id = %q, want com.example.mydrive", got.ID)
	}
	if got.Version != "0.1.0" {
		t.Errorf("version = %q, want 0.1.0", got.Version)
	}

	data := pluginScaffoldData{
		ID: "com.example.mydrive", Name: "My Drive", Slug: "mydrive",
		Version: "0.1.0", CoreAPI: "^0.1.0",
		Fields:          []pluginScaffoldField{{Name: "api_key"}, {Name: "root_folder_id"}},
		HasRootFolderID: true,
	}
	want := buildScaffoldManifest(data)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plugin.json does not match buildScaffoldManifest (the data driver.go's Manifest() literal is rendered from):\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestPluginNewI18nHasFieldKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mydrive")
	opts := pluginNewOptions{
		ID:     "com.example.mydrive",
		Dir:    dir,
		Fields: []string{"api_key", "root_folder_id"},
	}
	if err := runPluginNew(opts); err != nil {
		t.Fatalf("runPluginNew: %v", err)
	}

	enBytes, err := os.ReadFile(filepath.Join(dir, "i18n", "en.json"))
	if err != nil {
		t.Fatalf("read i18n/en.json: %v", err)
	}
	var en map[string]string
	if err := json.Unmarshal(enBytes, &en); err != nil {
		t.Fatalf("i18n/en.json is not valid JSON: %v", err)
	}

	for _, key := range []string{
		"field.api_key.label", "field.api_key.help",
		"field.root_folder_id.label", "field.root_folder_id.help",
	} {
		if _, ok := en[key]; !ok {
			t.Errorf("i18n/en.json missing key %q", key)
		}
	}
	if en["field.api_key.label"] != "Api Key" {
		t.Errorf(`en label for api_key = %q, want "Api Key"`, en["field.api_key.label"])
	}

	zhBytes, err := os.ReadFile(filepath.Join(dir, "i18n", "zh-CN.json"))
	if err != nil {
		t.Fatalf("read i18n/zh-CN.json: %v", err)
	}
	var zh map[string]string
	if err := json.Unmarshal(zhBytes, &zh); err != nil {
		t.Fatalf("i18n/zh-CN.json is not valid JSON: %v", err)
	}
	if _, ok := zh["field.api_key.label"]; !ok {
		t.Errorf("i18n/zh-CN.json missing key field.api_key.label")
	}
}

func TestRunPluginNewDefaultsNameAndDir(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if err := runPluginNew(pluginNewOptions{ID: "com.example.mydrive"}); err != nil {
		t.Fatalf("runPluginNew: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "mydrive", "plugin.json")); err != nil {
		t.Fatalf("expected default dir ./mydrive to have been created: %v", err)
	}
}

func TestGoIdentFromSegment(t *testing.T) {
	cases := map[string]string{
		"mydrive":  "Mydrive",
		"my-drive": "MyDrive",
		"my_drive": "MyDrive",
		"123drive": "P123drive",
	}
	for in, want := range cases {
		if got := goIdentFromSegment(in); got != want {
			t.Errorf("goIdentFromSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanizeIdent(t *testing.T) {
	cases := map[string]string{
		"api_key":        "Api Key",
		"root_folder_id": "Root Folder Id",
		"mydrive":        "Mydrive",
	}
	for in, want := range cases {
		if got := humanizeIdent(in); got != want {
			t.Errorf("humanizeIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
