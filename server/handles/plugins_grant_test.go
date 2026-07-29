package handles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// writePermPluginPackage builds an unsigned plugin package declaring perms,
// packed into a .tar.gz. Mirrors writeGuardPluginPackage but lets a consent
// test control the manifest's requested permissions.
func writePermPluginPackage(t *testing.T, id string, perms ext.Permissions) string {
	t.Helper()
	dir := t.TempDir()
	man := ext.Manifest{
		ID:          id,
		Name:        map[string]string{"en": id},
		Version:     "1.0.0",
		CoreAPI:     "^0.1.0",
		Kinds:       []ext.Kind{ext.KindStorageDriver},
		Provenance:  ext.Provenance{Source: "third-party", License: "MIT", Pricing: "free"},
		Permissions: perms,
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, extension.ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
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
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		hdr, _ := tar.FileInfoHeader(info, "")
		hdr.Name = e.Name()
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		content, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestInstallPluginWithGrantRecordsConsent drives the install handler with an
// explicit consent Grant that is NARROWER than the manifest requests, then reads
// it back through GetPluginPermissions: the requested side shows the manifest,
// the granted side shows only what the admin approved.
func TestInstallPluginWithGrantRecordsConsent(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "consent.install.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })

	pkg := writePermPluginPackage(t, id, ext.Permissions{HostAPIs: []string{"events", "store"}})
	body, _ := json.Marshal(PluginInstallReq{
		Path:  pkg,
		Grant: &extension.Grant{HostAPIs: []extension.GrantItem{{Name: "events"}}},
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/install", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	InstallPlugin(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("install: expected 200, got %d: %s", code, w.Body.String())
	}

	// The recorded consent must be the narrowed set, not the manifest.
	st, ok := mgr.Get(id)
	if !ok || st.Grant == nil || st.Grant.HasAPI("store") || !st.Grant.HasAPI("events") {
		t.Fatalf("install did not record the submitted grant: %+v", st.Grant)
	}

	// GetPluginPermissions returns {requested, granted}.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins/"+id+"/permissions", nil)
	GetPluginPermissions(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("permissions: expected 200, got %d: %s", code, w.Body.String())
	}
	var resp struct {
		Data extension.PermissionView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if !containsString(resp.Data.Requested.HostAPIs, "store") {
		t.Fatalf("requested should show manifest's 'store': %+v", resp.Data.Requested)
	}
	if resp.Data.Granted == nil || resp.Data.Granted.HasAPI("store") {
		t.Fatalf("granted should NOT include revoked 'store': %+v", resp.Data.Granted)
	}
}

// TestSetPluginGrantRevokes drives the post-install grant handler to revoke a
// capability and confirms the returned view reflects it.
func TestSetPluginGrantRevokes(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "consent.adjust.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writePermPluginPackage(t, id, ext.Permissions{HostAPIs: []string{"events", "store"}})); err != nil {
		t.Fatalf("install: %v", err)
	}

	body, _ := json.Marshal(PluginGrantReq{Grant: &extension.Grant{HostAPIs: []extension.GrantItem{{Name: "events"}}}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/grant", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	SetPluginGrant(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("grant: expected 200, got %d: %s", code, w.Body.String())
	}
	st, _ := mgr.Get(id)
	if st.Grant.HasAPI("store") {
		t.Fatal("post-install grant did not revoke 'store'")
	}
}

// TestSetPluginGrant_ExtraNetworkRequiresAck drives the post-install grant
// handler with a network host the manifest never declared: without
// acknowledged=true it must refuse (400) rather than silently drop or accept
// the extra host; with acknowledged=true it must persist it and echo it back
// in the granted view (design §6.5/§6.7, the one axis — network — an admin
// may extend beyond the manifest, gated on an explicit risk ack).
func TestSetPluginGrant_ExtraNetworkRequiresAck(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "consent.extranet.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writePermPluginPackage(t, id, ext.Permissions{Network: []string{"a.com"}})); err != nil {
		t.Fatalf("install: %v", err)
	}

	post := func(body []byte) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(ctxkey.User, guardPluginAdmin())
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/grant", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		SetPluginGrant(c)
		return w
	}

	// Unacknowledged: extra host "b.com" beyond the manifest's declared "a.com".
	body, _ := json.Marshal(PluginGrantReq{
		Grant:        &extension.Grant{Network: []string{"a.com", "b.com"}},
		Acknowledged: false,
	})
	w := post(body)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("extra domain without ack: want 400, got %d: %s", code, w.Body.String())
	}
	if st, _ := mgr.Get(id); containsString(st.Grant.Network, "b.com") {
		t.Fatal("extra domain without ack must not persist")
	}

	// Acknowledged: same extra host, now accepted and persisted.
	body, _ = json.Marshal(PluginGrantReq{
		Grant:        &extension.Grant{Network: []string{"a.com", "b.com"}},
		Acknowledged: true,
	})
	w = post(body)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("extra domain with ack: want 200, got %d: %s", code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "b.com") {
		t.Fatalf("granted.network should contain b.com; body=%s", w.Body.String())
	}
	st, _ := mgr.Get(id)
	if !containsString(st.Grant.Network, "b.com") || !containsString(st.Grant.Network, "a.com") {
		t.Fatalf("extra domain with ack must persist alongside the declared one: %+v", st.Grant.Network)
	}
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
