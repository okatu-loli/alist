package extension

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
)

func TestBuildPluginDriverInfo(t *testing.T) {
	m := ext.Manifest{
		ID: "cn.example.drv",
		Info: &ext.Info{
			Config:     ext.DriverConfig{Name: "Example", DefaultRoot: "0"},
			Additional: []ext.Item{{Name: "client_id", Type: "string", Required: true}, {Name: "secret", Type: "password"}},
		},
	}
	info := BuildPluginDriverInfo(m, "en", nil)
	if len(info.Common) == 0 {
		t.Fatal("common items must be synthesized (mount_path etc.)")
	}
	var hasMount bool
	for _, it := range info.Common {
		if it.Name == "mount_path" {
			hasMount = true
		}
	}
	if !hasMount {
		t.Fatal("mount_path expected in common")
	}
	if len(info.Additional) != 2 || info.Additional[0].Name != "client_id" || !info.Additional[0].Required {
		t.Fatalf("additional mapped wrong: %+v", info.Additional)
	}
	if info.Additional[1].Type != "password" {
		t.Fatal("password type must survive mapping")
	}
	if info.Config.Name != "Example" {
		t.Fatalf("config name wrong: %s", info.Config.Name)
	}
}

func TestBuildPluginDriverInfoNilIsZero(t *testing.T) {
	info := BuildPluginDriverInfo(ext.Manifest{ID: "x"}, "en", nil)
	if len(info.Common) != 0 || len(info.Additional) != 0 {
		t.Fatal("nil Info → zero driver.Info")
	}
}

// TestBuildPluginDriverInfoLocalizesLabelAndHelp is the Task ② TDD case:
// zh-CN bundle overrides Label/Help, en falls back to the bundle's en entry,
// and a field with no bundle key at all keeps the manifest's English Help
// with an empty Label.
func TestBuildPluginDriverInfoLocalizesLabelAndHelp(t *testing.T) {
	m := ext.Manifest{
		ID: "cn.example.i18n",
		Info: &ext.Info{
			Config: ext.DriverConfig{Name: "Example"},
			Additional: []ext.Item{
				{Name: "client_id", Type: "string", Help: "Client ID (English default)"},
				{Name: "no_bundle", Type: "string", Help: "No bundle entry (English default)"},
			},
		},
	}
	bundle := map[string]map[string]string{
		"zh-CN": {
			"field.client_id.label": "客户端 ID",
			"field.client_id.help":  "123 开放平台应用的 Client ID",
		},
		"en": {
			"field.client_id.label": "Client ID",
		},
	}

	zh := BuildPluginDriverInfo(m, "zh-CN", bundle)
	if zh.Additional[0].Label != "客户端 ID" || zh.Additional[0].Help != "123 开放平台应用的 Client ID" {
		t.Fatalf("zh-CN bundle not applied: %+v", zh.Additional[0])
	}
	if zh.Additional[1].Label != "" || zh.Additional[1].Help != "No bundle entry (English default)" {
		t.Fatalf("field with no bundle key must keep manifest English Help and empty Label: %+v", zh.Additional[1])
	}

	en := BuildPluginDriverInfo(m, "en", bundle)
	if en.Additional[0].Label != "Client ID" {
		t.Fatalf("en bundle label not applied: %+v", en.Additional[0])
	}
	if en.Additional[0].Help != "Client ID (English default)" {
		t.Fatalf("en bundle has no help override, must keep manifest English Help: %+v", en.Additional[0])
	}
}

// TestRegisterPluginModeSynthesizesInfo is the end-to-end assertion (Step 5):
// RegisterPluginMode on a manifest carrying Info populates Entry.info so
// StorageDriverInfoMap yields a populated form for the plugin driver. Uses an
// independent Registry so DefaultRegistry is untouched.
func TestRegisterPluginModeSynthesizesInfo(t *testing.T) {
	r := NewRegistry()
	m := ext.Manifest{
		ID:    "cn.example.drv2",
		Kinds: []ext.Kind{ext.KindStorageDriver},
		Info: &ext.Info{
			Config:     ext.DriverConfig{Name: "Example2"},
			Additional: []ext.Item{{Name: "client_id", Type: "string", Required: true}},
		},
	}
	if err := r.RegisterPluginMode(m, "/bin/example", ExecGRPC); err != nil {
		t.Fatalf("register: %v", err)
	}
	infoMap := r.StorageDriverInfoMap()
	got, ok := infoMap[m.ID]
	if !ok {
		t.Fatalf("expected %q in StorageDriverInfoMap, got %v", m.ID, infoMap)
	}
	var hasMount bool
	for _, it := range got.Common {
		if it.Name == "mount_path" {
			hasMount = true
		}
	}
	if !hasMount {
		t.Fatal("mount_path expected in common for plugin driver")
	}
	if len(got.Additional) != 1 || got.Additional[0].Name != "client_id" {
		t.Fatalf("additional mapped wrong: %+v", got.Additional)
	}
}

// mountItem returns the mount_path common item from info, or fails.
func mountItem(t *testing.T, common []driver.Item) driver.Item {
	t.Helper()
	for _, it := range common {
		if it.Name == "mount_path" {
			return it
		}
	}
	t.Fatal("mount_path not found in common items")
	return driver.Item{}
}

// TestBuildPluginDriverInfoLocalizesCommonFields is the follow-up fix: the
// host-generated common fields (mount_path, ...) get a localized Label and
// Help from the shared host i18n catalog, so the v4 frontend (which renders
// item.Label || item.Name and item.Help verbatim) no longer shows raw
// snake_case names / English help for a plugin driver's common fields.
func TestBuildPluginDriverInfoLocalizesCommonFields(t *testing.T) {
	m := ext.Manifest{
		ID: "cn.example.commoni18n",
		Info: &ext.Info{
			Config:     ext.DriverConfig{Name: "Example"},
			Additional: []ext.Item{{Name: "client_id", Type: "string"}},
		},
	}

	zh := BuildPluginDriverInfo(m, "zh-CN", nil)
	zhMount := mountItem(t, zh.Common)
	if zhMount.Label != "挂载路径" {
		t.Errorf("zh-CN mount_path Label = %q, want 挂载路径", zhMount.Label)
	}
	if zhMount.Help != "你要挂载到的路径，唯一且不可重复" {
		t.Errorf("zh-CN mount_path Help = %q, want translated help", zhMount.Help)
	}

	en := BuildPluginDriverInfo(m, "en", nil)
	enMount := mountItem(t, en.Common)
	if enMount.Label != "Mount path" {
		t.Errorf("en mount_path Label = %q, want 'Mount path'", enMount.Label)
	}
}
