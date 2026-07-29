// Command 123open-plugin packages the built-in 123_open storage driver as an
// installable gRPC plugin (bridge.StorageBridge over drivers/123_open.Open123).
// It replaces the compiled-in registration removed from drivers/all.go: 123
// Open is now available only by installing this plugin.
//
// The driver itself is untouched — Init/List/Link/Mkdir/Move/Rename/Remove/Put
// are plain HTTP against the 123 open platform API and do not touch the host
// DB, so (unlike testdata/localplugin) this binary needs no sqlite/gorm
// bootstrap.
package main

import (
	ext "github.com/AlistGo/alist-ext"
	open123 "github.com/alist-org/alist/v3/drivers/123_open"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension/bridge"
)

// manifest must stay semantically equivalent (as JSON) to plugin.json in this
// directory: the gRPC GetManifest RPC reads this method at runtime, the
// install gateway reads plugin.json from the packaged archive. A drift
// between the two would show up as the wizard or "add storage" form the
// frontend rendered not matching what the running driver actually declared.
// driver_test.go asserts this equivalence by round-tripping plugin.json
// through json.Unmarshal and comparing it against manifest() with
// reflect.DeepEqual.
func manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "cn.alist.123open",
		Name:    map[string]string{"en": "123 Open", "zh": "123 云盘"},
		Version: "0.1.0",
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		// Capabilities is hand-trimmed, not derived: 123_open's Copy and
		// Archive* methods exist (driver.Driver requires them) but always
		// return errs.NotSupport, so an automatic capability probe would
		// falsely advertise them. Put is real (create -> single/slice ->
		// complete against the 123 open platform upload API, see
		// drivers/123_open/upload.go) and is wired via StorageBridge.Put's
		// driver.Put case, so CapPut is declared here. PutRapid is likewise
		// real (create(etag) against the same upload API — a hash match
		// registers the file with zero bytes transferred) and wired via
		// StorageBridge.TryRapidUpload's driver.PutRapid case, so
		// CapPutRapid is declared here too.
		Capabilities: []ext.Capability{ext.CapMkdir, ext.CapMove, ext.CapRename, ext.CapRemove, ext.CapPut, ext.CapPutRapid},
		Provenance: ext.Provenance{
			Source:    "community",
			License:   "AGPL-3.0",
			SourceURL: "https://github.com/alist-org/alist",
			Pricing:   "free",
		},
		Permissions: ext.Permissions{Network: []string{"open-api.123pan.com"}},
		// Wizard collects no credentials: those belong to each storage
		// instance (added later, per account), not to installing the driver
		// once. info + done only.
		Wizard: ext.Wizard{Steps: []ext.WizardStep{
			{
				ID:    "welcome",
				Type:  ext.WizardInfo,
				Title: map[string]string{"en": "Install 123 Open", "zh": "安装 123 云盘"},
				Body: map[string]string{
					"en": "This installs the 123 Open storage driver. After installing, add a storage with your 123 Open credentials.",
					"zh": "这将安装 123 云盘存储驱动。安装后,在「添加存储」里填入你的 123 开放平台凭证即可挂载。",
				},
			},
			{ID: "done", Type: ext.WizardDone, Title: map[string]string{"en": "Done", "zh": "完成"}},
		}},
		// Info declares the "add storage" form: the host synthesizes the
		// generic common fields (mount_path, order, ...) itself and copies
		// these driver-specific Additional items verbatim, mirroring
		// drivers/123_open/meta.go's Addition struct field-for-field.
		Info: &ext.Info{
			Config: ext.DriverConfig{Name: "123 Open", DefaultRoot: "0"},
			Additional: []ext.Item{
				{Name: "client_id", Type: "string", Required: true, Help: "123 open platform Client ID"},
				{Name: "client_secret", Type: "password", Required: true, Help: "123 open platform Client Secret"},
				{Name: "private_key", Type: "password", Help: "Optional signing private key"},
				{Name: "uid", Type: "number", Help: "User ID"},
				{Name: "valid_duration", Type: "number", Default: "30", Help: "minutes"},
				{Name: "root_folder_id", Type: "string", Default: "0", Help: "Root folder ID"},
			},
		},
	}
}

// newBridge wraps the unmodified Open123 legacy driver behind the 9.1
// StorageBridge, presenting it as an ext.StorageDriver over the plugin's
// hand-written manifest.
func newBridge() *bridge.StorageBridge {
	return bridge.NewStorageBridge(func() driver.Driver { return &open123.Open123{} }, manifest())
}
