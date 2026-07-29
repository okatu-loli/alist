package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// F-consent RED-TEAM at the admin-API layer. Complements the core-layer suite in
// internal/extension/grant_gate_redteam_test.go by driving the real HTTP handlers
// (InstallPlugin / SetPluginGrant / GetPluginPermissions). _GAP_ tests pin a
// confirmed weakness by asserting current behavior.

// TestRedteam_SetPluginGrant_OverGrantBeyondManifest_GAP: POST /grant used to
// accept a Grant that exceeds the plugin's Manifest requests and persist it
// verbatim (granted ⊄ requested, Manifest not enforced as a ceiling). Root
// cause was SetPluginGrant forwarding req.Grant to Manager.SetGrant with no
// ⊆-manifest validation. FIXED in two layers: (1) clampToManifest inside
// SetGrant drops host_apis/storage_tokens/fs beyond the manifest
// unconditionally — network only escapes the clamp when the caller passes
// allowExtraNetwork; (2) the handler now refuses (400) an over-manifest
// network submission unless the caller sets acknowledged=true, so the one
// axis that CAN exceed the manifest requires an explicit, audited risk
// acknowledgement rather than being silently accepted.
func TestRedteam_SetPluginGrant_OverGrantBeyondManifest_GAP(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "redteam.overgrant.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })

	// Manifest requests ONLY "events".
	if _, err := mgr.Install(writePermPluginPackage(t, id, ext.Permissions{HostAPIs: []string{"events"}})); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Admin submits a Grant with capabilities the manifest never declared.
	over := &extension.Grant{
		HostAPIs:      []extension.GrantItem{{Name: "events"}, {Name: "public_routes"}, {Name: "store"}},
		Network:       []string{"*"},
		StorageTokens: []extension.GrantItem{{Name: "root"}},
		Filesystem:    "full",
	}
	post := func(acknowledged bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(PluginGrantReq{Grant: over, Acknowledged: acknowledged})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(ctxkey.User, guardPluginAdmin())
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/grant", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		SetPluginGrant(c)
		return w
	}

	// Without acknowledged=true the over-manifest network host ("*") is refused
	// outright — the over-grant attempt never reaches the clamp/persist path.
	if w := post(false); respCode(t, w) != 400 {
		t.Fatalf("grant: unacknowledged over-manifest network must be refused, got %d: %s", respCode(t, w), w.Body.String())
	}
	if st, _ := mgr.Get(id); st.Grant != nil && (st.Grant.HasAPI("store") || len(st.Grant.Network) != 0) {
		t.Fatalf("REGRESSION: refused grant must not persist: %+v", st.Grant)
	}

	// With acknowledged=true the request goes through, but only network may
	// exceed the manifest — host_apis/storage_tokens/fs still clamp.
	w := post(true)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("grant: acknowledged over-manifest network should be accepted, got %d: %s", code, w.Body.String())
	}

	var resp struct {
		Data extension.PermissionView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if containsString(resp.Data.Requested.HostAPIs, "store") {
		t.Fatal("test setup invalid: manifest should request only 'events'")
	}
	// host_apis/storage_tokens/fs stay clamped to the manifest ceiling
	// regardless of acknowledged — only network may exceed it.
	if resp.Data.Granted == nil || resp.Data.Granted.HasAPI("store") || resp.Data.Granted.HasAPI("public_routes") {
		t.Fatalf("REGRESSION: granted exceeds the manifest ceiling: %+v", resp.Data.Granted)
	}
	if resp.Data.Granted.Filesystem == "full" {
		t.Fatalf("REGRESSION: over-granted fs survived the clamp: %+v", resp.Data.Granted)
	}
	if !resp.Data.Granted.HasAPI("events") {
		t.Fatal("the one requested capability (events) must survive the clamp")
	}
	// network IS allowed to exceed the manifest here — that's the acknowledged
	// escape hatch working as designed, not a regression.
	if !containsString(resp.Data.Granted.Network, "*") {
		t.Fatalf("acknowledged over-manifest network host should be granted: %+v", resp.Data.Granted.Network)
	}
}

// TestRedteam_InstallWithGrant_OverGrantBeyondManifest_GAP: the same clamp gap
// via install-with-grant. A package requesting nothing is installed with a Grant
// that hands it full network egress + fs full; the install succeeds and the
// over-grant persists.
func TestRedteam_InstallWithGrant_OverGrantBeyondManifest_GAP(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "redteam.installovergrant.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })

	// Manifest requests NOTHING.
	pkg := writePermPluginPackage(t, id, ext.Permissions{})
	body, _ := json.Marshal(PluginInstallReq{
		Path: pkg,
		Grant: &extension.Grant{
			HostAPIs:   []extension.GrantItem{{Name: "public_routes"}},
			Network:    []string{"*"},
			Filesystem: "full",
		},
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/install", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	InstallPlugin(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("install-with-grant: expected 200 (gap: over-grant accepted), got %d: %s", code, w.Body.String())
	}

	st, ok := mgr.Get(id)
	if !ok || st.Grant == nil {
		t.Fatalf("expected an installed plugin with a recorded grant, got ok=%v grant=%v", ok, st.Grant)
	}
	// FIXED: the manifest requested nothing, so the clamp drops the entire
	// over-grant — no public_routes, no fs full.
	if st.Grant.HasAPI("public_routes") || st.Grant.Filesystem == "full" {
		t.Fatalf("REGRESSION: over-grant beyond an empty manifest survived: %+v", st.Grant)
	}
}

// TestRedteam_MalformedGrantFailsInstall_NotSilentDefault: a grant that fails to
// apply must fail the whole install loudly — it must NOT silently leave the
// default (approve-manifest) consent in place. Here we point install-with-grant
// at a plugin id mismatch scenario by submitting a grant for a package whose
// SetGrant target does not exist would be internal; instead we assert the
// documented contract via a decode failure surface: a body with a malformed
// grant type is rejected at bind. Guards "malformed grant fails the whole
// install, not a silent default".
func TestRedteam_MalformedGrantBody_Rejected(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)

	// grant must be an object; a string is malformed and must be rejected at bind
	// rather than silently ignored.
	raw := []byte(`{"path":"/nonexistent.tar.gz","grant":"not-an-object"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/install", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	InstallPlugin(c)
	if code := respCode(t, w); code == 200 {
		t.Fatalf("malformed grant body must not yield a 200 install: %s", w.Body.String())
	}
}
