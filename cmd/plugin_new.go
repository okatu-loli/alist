package cmd

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"

	ext "github.com/AlistGo/alist-ext"
	"github.com/spf13/cobra"
)

// pluginTemplatesFS holds the `alist plugin new` scaffold templates (Task 1).
// Each *.tmpl file is registered under its own base filename (e.g.
// "driver.go.tmpl") and rendered independently.
//
//go:embed templates/plugin/*.tmpl
var pluginTemplatesFS embed.FS

var pluginNewCmd = &cobra.Command{
	Use:   "new --id <reverse-domain> [--name] [--dir] [--fields a,b,c] [--ext-path <local ext>]",
	Short: "Scaffold a buildable ext.StorageDriver plugin skeleton",
	Run: func(cmd *cobra.Command, args []string) {
		id, _ := cmd.Flags().GetString("id")
		name, _ := cmd.Flags().GetString("name")
		dir, _ := cmd.Flags().GetString("dir")
		fieldsRaw, _ := cmd.Flags().GetString("fields")
		extPath, _ := cmd.Flags().GetString("ext-path")

		opts := pluginNewOptions{
			ID:      id,
			Name:    name,
			Dir:     dir,
			Fields:  splitCSV(fieldsRaw),
			ExtPath: extPath,
		}
		if err := runPluginNew(opts); err != nil {
			fmt.Fprintln(os.Stderr, "plugin new:", err)
			os.Exit(1)
		}
	},
}

func init() {
	pluginCmd.AddCommand(pluginNewCmd)

	pluginNewCmd.Flags().String("id", "", "globally unique, reverse-domain plugin id (e.g. com.example.mydrive) [required]")
	pluginNewCmd.Flags().String("name", "", "display name (default: derived from --id's last segment)")
	pluginNewCmd.Flags().String("dir", "", "output directory (default: ./<id's last segment>)")
	pluginNewCmd.Flags().String("fields", "api_key,root_folder_id", "comma-separated storage config field names")
	pluginNewCmd.Flags().String("ext-path", "", "local path to the alist-ext SDK checkout; if set, go.mod gets a replace directive to it (for local iteration against unreleased SDK changes, not required for normal use — github.com/AlistGo/alist-ext is a published module)")
}

// pluginNewOptions are the resolved `alist plugin new` inputs, independent of
// cobra flag parsing so runPluginNew is directly unit-testable.
type pluginNewOptions struct {
	ID      string
	Name    string
	Dir     string
	Fields  []string
	ExtPath string
}

// pluginScaffoldData is what the embedded templates render against. Every
// field the templates use to reconstruct Manifest() (ID/Name/Version/CoreAPI/
// Fields/HasRootFolderID) is also fed to buildScaffoldManifest, so plugin.json
// (marshaled from that struct) and driver.go's rendered Manifest() literal
// stay in sync by construction rather than by hand-copied duplication.
type pluginScaffoldData struct {
	ID              string
	Name            string
	Slug            string // id's last segment; used for dir/file/binary naming in scripts
	TypeName        string // unexported Go type name, e.g. "mydriveDriver"
	CtorName        string // constructor func name, e.g. "newMydriveDriver"
	Version         string
	CoreAPI         string
	Fields          []pluginScaffoldField
	HasRootFolderID bool
	Module          string // go.mod module path (derived from ID)
	ExtRequire      string // go.mod require version for alist-ext
	ExtPath         string // optional local replace target
}

type pluginScaffoldField struct {
	Name string
}

// runPluginNew builds a full plugin skeleton at opts.Dir (or its default).
// It is the testable core of `alist plugin new`: no flag parsing, no
// os.Exit — callers report errors themselves.
func runPluginNew(opts pluginNewOptions) error {
	id := strings.TrimSpace(opts.ID)
	if id == "" {
		return fmt.Errorf("--id is required (e.g. com.example.mydrive)")
	}
	if strings.ContainsAny(id, " \t\n/\\") {
		return fmt.Errorf("--id %q must not contain whitespace or path separators", id)
	}

	slug := lastIDSegment(id)
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = humanizeIdent(slug)
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = "./" + slug
	}

	var fields []pluginScaffoldField
	hasRootFolderID := false
	for _, f := range opts.Fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		fields = append(fields, pluginScaffoldField{Name: f})
		if f == "root_folder_id" {
			hasRootFolderID = true
		}
	}

	ident := goIdentFromSegment(slug)
	data := pluginScaffoldData{
		ID:              id,
		Name:            name,
		Slug:            slug,
		TypeName:        lowerFirst(ident) + "Driver",
		CtorName:        "new" + ident + "Driver",
		Version:         "0.1.0",
		CoreAPI:         "^0.1.0",
		Fields:          fields,
		HasRootFolderID: hasRootFolderID,
		Module:          id,
		ExtRequire:      "v0.1.0",
		ExtPath:         strings.TrimSpace(opts.ExtPath),
	}

	if err := ensureEmptyDir(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		return err
	}

	tmpl, err := template.ParseFS(pluginTemplatesFS, "templates/plugin/*.tmpl")
	if err != nil {
		return fmt.Errorf("parse embedded templates: %w", err)
	}

	// .go files: render then gofmt (go/format.Source) before writing, so the
	// scaffold is always gofmt-clean and guaranteed go/parser-parseable.
	for tmplName, outRel := range map[string]string{
		"main.go.tmpl":   "main.go",
		"driver.go.tmpl": "driver.go",
	} {
		src, err := renderTemplate(tmpl, tmplName, data)
		if err != nil {
			return err
		}
		formatted, err := format.Source(src)
		if err != nil {
			return fmt.Errorf("generated %s does not gofmt/parse: %w", outRel, err)
		}
		if err := os.WriteFile(filepath.Join(dir, outRel), formatted, 0o644); err != nil {
			return err
		}
	}

	// Plain-text templates: rendered as-is.
	for tmplName, outRel := range map[string]string{
		"README.md.tmpl": "README.md",
		"gitignore.tmpl": ".gitignore",
		"go.mod.tmpl":    "go.mod",
	} {
		out, err := renderTemplate(tmpl, tmplName, data)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, outRel), out, 0o644); err != nil {
			return err
		}
	}

	// Shell scripts: rendered, then marked executable.
	for tmplName, outRel := range map[string]string{
		"pack.sh.tmpl":  "pack.sh",
		"serve.sh.tmpl": "serve.sh",
	} {
		out, err := renderTemplate(tmpl, tmplName, data)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, outRel), out, 0o755); err != nil {
			return err
		}
	}

	// plugin.json is marshaled from the same Go struct the driver.go template
	// renders a literal of (buildScaffoldManifest), not templated: this keeps
	// the two provably equivalent instead of duplicating the manifest shape
	// by hand in a JSON template.
	manifest := buildScaffoldManifest(data)
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal plugin.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), append(manifestJSON, '\n'), 0o644); err != nil {
		return err
	}

	// i18n/en.json + i18n/zh-CN.json: one field.<name>.label/.help pair per
	// --fields entry. en falls back to the humanized field name (Help is
	// always "" in a fresh scaffold, see buildScaffoldManifest); other
	// locales start as empty-string placeholders awaiting translation.
	for _, lang := range []string{"en", "zh-CN"} {
		locale := scaffoldLocale(fields, lang)
		b, err := json.MarshalIndent(locale, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal i18n/%s.json: %w", lang, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "i18n", lang+".json"), append(b, '\n'), 0o644); err != nil {
			return err
		}
	}

	fmt.Println("scaffolded plugin", id, "at", dir)
	return nil
}

// buildScaffoldManifest builds the ext.Manifest a fresh scaffold declares.
// driver.go's rendered Manifest() literal (driver.go.tmpl) must stay
// structurally identical to this — see plugin_new_test.go's
// TestPluginNewManifestMatchesPluginJSON, which decodes plugin.json and
// compares it against this function's output with reflect.DeepEqual.
func buildScaffoldManifest(d pluginScaffoldData) ext.Manifest {
	items := make([]ext.Item, 0, len(d.Fields))
	for _, f := range d.Fields {
		it := ext.Item{Name: f.Name, Type: "string"}
		if f.Name == "root_folder_id" {
			it.Default = "0"
		}
		items = append(items, it)
	}
	cfg := ext.DriverConfig{Name: d.Name}
	if d.HasRootFolderID {
		cfg.DefaultRoot = "0"
	}
	return ext.Manifest{
		ID:      d.ID,
		Name:    map[string]string{"en": d.Name, "zh": d.Name},
		Version: d.Version,
		CoreAPI: d.CoreAPI,
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Capabilities: []ext.Capability{
			ext.CapGetter,
		},
		Provenance: ext.Provenance{
			Source:  "community",
			License: "MIT",
		},
		Permissions: ext.Permissions{
			Filesystem: "none",
		},
		Info: &ext.Info{
			Config:     cfg,
			Additional: items,
		},
	}
}

// scaffoldLocale builds a fresh i18n/<lang>.json map for fields: for "en" the
// label falls back to the humanized field name and help is the empty string
// (the scaffolded manifest's Item.Help is always ""); every other locale gets
// empty-string placeholders for both keys, awaiting translation. Keys follow
// the "field.<name>.label"/"field.<name>.help" convention LoadPluginLocales
// reads (internal/extension/locales.go) — the same convention `alist plugin
// i18n` (Task 2) validates against.
func scaffoldLocale(fields []pluginScaffoldField, lang string) map[string]string {
	m := make(map[string]string, len(fields)*2)
	for _, f := range fields {
		label, help := "", ""
		if lang == "en" {
			label = humanizeIdent(f.Name)
		}
		m["field."+f.Name+".label"] = label
		m["field."+f.Name+".help"] = help
	}
	return m
}

func renderTemplate(tmpl *template.Template, name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// ensureEmptyDir creates dir if absent, and errors if it exists but is a
// non-empty directory (or not a directory at all) — `alist plugin new`
// refuses to scaffold into an occupied path.
func ensureEmptyDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0o755)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s already exists and is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s already exists and is not empty", dir)
	}
	return nil
}

// splitCSV splits a comma-separated flag value, trimming whitespace and
// dropping empty entries.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// lastIDSegment returns the last non-empty "."-separated segment of a
// reverse-domain plugin id (e.g. "com.example.mydrive" -> "mydrive"), or id
// itself if it has no dots.
func lastIDSegment(id string) string {
	parts := strings.Split(id, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return id
}

// identParts splits s on runs of non-alphanumeric characters (e.g. "_", "-"),
// the shared tokenizer behind goIdentFromSegment and humanizeIdent.
func identParts(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// goIdentFromSegment turns an id segment or field name into an exported Go
// identifier fragment, e.g. "my-drive" -> "MyDrive", "mydrive" -> "Mydrive".
// Falls back to "Plugin" if s tokenizes to nothing (e.g. it was all
// punctuation).
func goIdentFromSegment(s string) string {
	parts := identParts(strings.ToLower(s))
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	out := b.String()
	if out == "" {
		return "Plugin"
	}
	if r := rune(out[0]); !unicode.IsLetter(r) {
		out = "P" + out
	}
	return out
}

// humanizeIdent turns a snake/kebab-case identifier into a human-readable,
// title-cased label, e.g. "root_folder_id" -> "Root Folder Id". Used both for
// the default display Name (derived from the id's last segment) and for a
// field's i18n en label fallback when the author hasn't set one explicitly.
func humanizeIdent(s string) string {
	parts := identParts(s)
	if len(parts) == 0 {
		return s
	}
	words := make([]string, 0, len(parts))
	for _, p := range parts {
		words = append(words, strings.ToUpper(p[:1])+p[1:])
	}
	return strings.Join(words, " ")
}

// lowerFirst lower-cases the first rune of s, leaving the rest untouched
// (e.g. "MyDrive" -> "myDrive").
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
