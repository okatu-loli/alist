package handles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/server/common"
)

// TestGetDriverInfoLocalizesPluginFields is the Task ② handler-level case: a
// plugin driver's Additional item Label/Help are localized per the request's
// Accept-Language, resolved through the plugin's bundled i18n/*.json locale
// files loaded at registration (RegisterPluginMode reads them from the
// binary's directory — see registry.go).
func TestGetDriverInfoLocalizesPluginFields(t *testing.T) {
	dir := t.TempDir()
	i18nDir := filepath.Join(dir, "i18n")
	if err := os.MkdirAll(i18nDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i18nDir, "zh-CN.json"), []byte(`{
		"field.client_id.label": "客户端 ID",
		"field.client_id.help": "123 开放平台应用的 Client ID"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "plugin")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := ext.Manifest{
		ID:    "h.driver.i18n",
		Kinds: []ext.Kind{ext.KindStorageDriver},
		Info: &ext.Info{
			Config:     ext.DriverConfig{Name: "HDriverI18n"},
			Additional: []ext.Item{{Name: "client_id", Type: "string", Help: "Client ID (English default)"}},
		},
	}
	if err := extension.DefaultRegistry.RegisterPluginMode(m, binPath, extension.ExecGRPC); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(m.ID) })

	for _, tc := range []struct {
		acceptLanguage string
		wantLabel      string
		wantHelp       string
	}{
		{"zh-CN", "客户端 ID", "123 开放平台应用的 Client ID"},
		{"en", "", "Client ID (English default)"},
		{"", "", "Client ID (English default)"},
	} {
		c, w := i18nCtx(tc.acceptLanguage)
		c.Request.URL.RawQuery = "driver=" + m.ID
		GetDriverInfo(c)

		var resp common.Resp[driver.Info]
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("[Accept-Language=%q] unmarshal body %q: %v", tc.acceptLanguage, w.Body.String(), err)
		}
		if len(resp.Data.Additional) != 1 {
			t.Fatalf("[Accept-Language=%q] expected 1 additional item, got %+v", tc.acceptLanguage, resp.Data.Additional)
		}
		got := resp.Data.Additional[0]
		if got.Label != tc.wantLabel || got.Help != tc.wantHelp {
			t.Errorf("[Accept-Language=%q] label=%q help=%q, want label=%q help=%q",
				tc.acceptLanguage, got.Label, got.Help, tc.wantLabel, tc.wantHelp)
		}
	}
}
