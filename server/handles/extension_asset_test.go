package handles

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// writeThemePluginPackage builds an unsigned plugin package carrying theme
// assets (a token json, a css file, a woff2, and a disallowed .html) so the
// PluginAsset allowlist + traversal guard can be exercised end to end.
func writeThemePluginPackage(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	man := ext.Manifest{
		ID:         id,
		Name:       map[string]string{"en": id},
		Version:    "1.0.0",
		CoreAPI:    "^0.1.0",
		Kinds:      []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{Source: "third-party", License: "MIT", Pricing: "free"},
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		extension.ManifestFile: b,
		"plugin":               []byte("#!/bin/sh\nexit 0\n"),
		"themes/nord.json":     []byte(`{"$schema":"alist-theme/v1"}`),
		"themes/nord.css":      []byte(".card{color:red}"),
		"fonts/Inter.woff2":    {0x77, 0x4f, 0x46, 0x32, 1, 2, 3, 4},
		"themes/evil.html":     []byte("<script>alert(1)</script>"),
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "pkg.tar.gz")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// serveAsset drives the PluginAsset handler exactly as the anonymous route does.
func serveAsset(id, filepath string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}, {Key: "filepath", Value: filepath}}
	c.Request = httptest.NewRequest("GET", "/plugin-assets/"+id+"/"+filepath, nil)
	PluginAsset(c)
	return w
}

func TestPluginAssetServesAllowlistedFilesAndBlocksTheRest(t *testing.T) {
	setupPluginGuardEnv(t)
	const id = "com.example.themeasset"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Install(writeThemePluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	// PluginAsset also requires the plugin to be ENABLED in the registry.
	registerEnabledExtension(t, ext.Manifest{ID: id, Name: map[string]string{"en": id}, Kinds: []ext.Kind{ext.KindAction}})

	// allowlisted assets serve 200 with nosniff.
	if w := serveAsset(id, "themes/nord.json"); w.Code != 200 {
		t.Fatalf("token json should serve 200, got %d: %s", w.Code, w.Body.String())
	} else if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("asset must carry nosniff, headers=%v", w.Header())
	}
	if w := serveAsset(id, "fonts/Inter.woff2"); w.Code != 200 {
		t.Fatalf("woff2 should serve 200, got %d", w.Code)
	}
	if w := serveAsset(id, "themes/nord.css"); w.Code != 200 {
		t.Fatalf("css should serve 200, got %d", w.Code)
	}
	// A served asset carries a script-killing CSP so a directly-navigated SVG
	// (the one allowlisted type that can embed <script>) cannot execute in the
	// app origin — the app-document CSP does not cover this route.
	if w := serveAsset(id, "themes/nord.json"); w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("asset must carry a sandbox CSP, headers=%v", w.Header())
	}

	// a disallowed type (HTML) is refused even though the file exists on disk.
	if w := serveAsset(id, "themes/evil.html"); w.Code != 404 {
		t.Fatalf("html asset must be 404 (allowlist), got %d", w.Code)
	}
	// path traversal is rejected.
	if w := serveAsset(id, "../plugin"); w.Code != 404 {
		t.Fatalf("traversal must be 404, got %d", w.Code)
	}
	// an unknown / not-enabled plugin id is 404 (before touching the manager).
	if w := serveAsset("com.example.notenabled", "themes/nord.json"); w.Code != 404 {
		t.Fatalf("not-enabled plugin must be 404, got %d", w.Code)
	}
}
