// Command demo-plugin is a real, buildable, signable ext-native StorageDriver
// plugin used to validate the end-to-end plugin path: build -> package -> sign
// -> install -> wizard -> contributions -> gRPC dispatch. It is intentionally
// trivial: List/Link/Get all answer with one fixed stub object, so the test
// surface is the plugin machinery, not driver logic.
package main

import (
	"context"
	"encoding/json"
	"net/http"

	ext "github.com/AlistGo/alist-ext"
)

// newDemoDriver builds the demo driver.
func newDemoDriver() *demoDriver { return &demoDriver{} }

// demoDriver serves a fixed tree: the root contains one file,
// "hello-from-plugin.txt", whose Link is a stub URL.
type demoDriver struct{}

const (
	// FileAnchor is the anchor of the single stub file the driver serves.
	FileAnchor = "hello-from-plugin.txt"
	// FileURL is the stub URL its Link resolves to.
	FileURL = "https://example.com/stub"
)

var (
	_ ext.StorageDriver = (*demoDriver)(nil)
	_ ext.Getter        = (*demoDriver)(nil)
)

// Manifest must stay byte-for-byte equivalent (as JSON) to plugin.json: the
// gRPC GetManifest RPC reads this method, while the install gateway reads the
// packaged plugin.json. A drift between the two would show up as the wizard
// or contributions the frontend rendered not matching what the running driver
// actually declared.
func (d *demoDriver) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.demoplugin",
		Name:    map[string]string{"en": "Demo Plugin", "zh": "演示插件"},
		Version: "0.1.0",
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Capabilities: []ext.Capability{
			ext.CapGetter,
		},
		Provenance: ext.Provenance{
			Source:    "community",
			License:   "MIT",
			SourceURL: "https://github.com/example/demo-plugin",
			Pricing:   "free",
		},
		Permissions: ext.Permissions{
			Filesystem: "none",
		},
		Wizard: ext.Wizard{
			Steps: []ext.WizardStep{
				{
					ID:    "welcome",
					Type:  ext.WizardInfo,
					Title: map[string]string{"en": "Welcome", "zh": "欢迎"},
					Body: map[string]string{
						"en": "This wizard configures the demo plugin.",
						"zh": "本向导用于配置演示插件。",
					},
				},
				{
					ID:    "config",
					Type:  ext.WizardForm,
					Title: map[string]string{"en": "Configure", "zh": "配置"},
					Schema: &ext.JSONSchema{
						Type:     "object",
						Required: []string{"label"},
						Properties: map[string]*ext.JSONSchema{
							"label": {
								Type:  "string",
								Title: map[string]string{"en": "Label", "zh": "标签"},
							},
						},
					},
				},
				{
					ID:      "probe",
					Type:    ext.WizardTest,
					Title:   map[string]string{"en": "Connectivity", "zh": "连通性测试"},
					ProbeID: "connect",
				},
				{
					ID:    "finish",
					Type:  ext.WizardDone,
					Title: map[string]string{"en": "Done", "zh": "完成"},
				},
			},
		},
		Contributions: ext.Contributions{
			ConfigForm: &ext.JSONSchema{
				Type:     "object",
				Required: []string{"greeting"},
				Properties: map[string]*ext.JSONSchema{
					"greeting": {
						Type:    "string",
						Default: "hello",
						Title:   map[string]string{"en": "Greeting", "zh": "问候语"},
					},
				},
			},
			MenuActions: []ext.MenuAction{
				{
					ID:     "ping",
					Label:  map[string]string{"en": "Ping", "zh": "测试连通"},
					Icon:   "wifi",
					Target: "global",
				},
			},
		},
	}
}

func (d *demoDriver) DriverConfig() ext.DriverConfig {
	return ext.DriverConfig{Name: "DemoPlugin", OnlyProxy: false}
}

func (d *demoDriver) Init(ctx context.Context, host ext.Host) error { return nil }
func (d *demoDriver) Shutdown(ctx context.Context) error            { return nil }

func (d *demoDriver) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	return nil
}
func (d *demoDriver) DropInstance(ctx context.Context, storageID string) error { return nil }

func (d *demoDriver) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	return []ext.Obj{{
		Name:     FileAnchor,
		Size:     0,
		IsDir:    false,
		ID:       FileAnchor,
		Path:     "/" + FileAnchor,
		Modified: 1_700_000_000_000,
	}}, nil
}

func (d *demoDriver) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	// Accept both the bare anchor and the "/"-prefixed form: List/Get hand back
	// an ext.Obj with both ID (bare) and Path ("/"+bare) populated, and the
	// bridge's refAnchor prefers Path when set (internal/extension/bridge/grpcdriver.go),
	// so a round trip through List -> Link presents the "/"-prefixed form here.
	// Get already tolerated both forms; Link matches that so the read path
	// (list -> link/download) round-trips consistently end to end.
	if file.Anchor != FileAnchor && file.Anchor != "/"+FileAnchor {
		return nil, ext.NewError(ext.ErrCodeObjectNotFound, file.Anchor)
	}
	return &ext.Link{
		ByteSource: ext.ByteSourceURL,
		URL:        FileURL,
		Header:     http.Header{},
	}, nil
}

func (d *demoDriver) Get(ctx context.Context, storageID, path string) (ext.Obj, error) {
	if path != FileAnchor && path != "/"+FileAnchor {
		return ext.Obj{}, ext.NewError(ext.ErrCodeObjectNotFound, path)
	}
	return ext.Obj{Name: FileAnchor, Size: 0, ID: FileAnchor, Path: "/" + FileAnchor}, nil
}
