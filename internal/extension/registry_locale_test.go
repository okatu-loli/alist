package extension

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AlistGo/alist-ext"
)

// TestRegisterPluginModeLoadsLocalesFromBinDir is the Task ② end-to-end case:
// RegisterPluginMode loads the plugin's bundled i18n/*.json from the
// directory containing its binary (the version dir, per lifecycle.go's
// findBinary/versionDir layout), and StorageDriverInfoMapLocale localizes
// that plugin's Additional item Label/Help per request locale — while a
// built-in (constructor-mode) entry registered in the same registry is
// untouched by locale.
func TestRegisterPluginModeLoadsLocalesFromBinDir(t *testing.T) {
	dir := t.TempDir()
	i18nDir := filepath.Join(dir, "i18n")
	if err := os.MkdirAll(i18nDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(i18nDir, "zh-CN.json"), `{
		"field.client_id.label": "客户端 ID",
		"field.client_id.help": "123 开放平台应用的 Client ID"
	}`)
	binPath := filepath.Join(dir, "plugin")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	m := ext.Manifest{
		ID:    "cn.example.locale",
		Kinds: []ext.Kind{ext.KindStorageDriver},
		Info: &ext.Info{
			Config:     ext.DriverConfig{Name: "ExampleLocale"},
			Additional: []ext.Item{{Name: "client_id", Type: "string", Help: "Client ID (English default)"}},
		},
	}
	if err := r.RegisterPluginMode(m, binPath, ExecGRPC); err != nil {
		t.Fatalf("register: %v", err)
	}

	zhMap := r.StorageDriverInfoMapLocale("zh-CN")
	zh, ok := zhMap[m.ID]
	if !ok {
		t.Fatalf("expected %q in zh-CN map, got %v", m.ID, zhMap)
	}
	if len(zh.Additional) != 1 || zh.Additional[0].Label != "客户端 ID" || zh.Additional[0].Help != "123 开放平台应用的 Client ID" {
		t.Fatalf("zh-CN localization not applied: %+v", zh.Additional)
	}

	enMap := r.StorageDriverInfoMapLocale("en")
	en, ok := enMap[m.ID]
	if !ok {
		t.Fatalf("expected %q in en map, got %v", m.ID, enMap)
	}
	if en.Additional[0].Label != "" || en.Additional[0].Help != "Client ID (English default)" {
		t.Fatalf("en (no en bundle entry) must fall back to manifest English Help with empty Label: %+v", en.Additional[0])
	}

	// Default (no-locale) StorageDriverInfoMap must match DefaultLocale ("en").
	def := r.StorageDriverInfoMap()[m.ID]
	if def.Additional[0].Label != en.Additional[0].Label || def.Additional[0].Help != en.Additional[0].Help {
		t.Fatalf("StorageDriverInfoMap must match StorageDriverInfoMapLocale(DefaultLocale): %+v vs %+v", def, en)
	}
}
