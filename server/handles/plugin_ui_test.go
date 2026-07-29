package handles

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// TestPluginUiGrantedCapabilities drives all three UI-bridge endpoints for a
// plugin whose approved Grant carries storage_tokens/sign/store: scoped-token
// and the kv put/get round-trip must succeed (200). sign-link is asserted only
// up to "the capability gate let it through" (not 403): the underlying
// Signer.SignLink additionally authorizes the ref against the plugin's bound
// FS principal (invariant 4, hostcap/sign.go), and hostBuilderFor's
// toHostcapGrant (internal/bootstrap/plugin_host_bridge.go) deliberately never
// populates FS scopes for a StorageDriver plugin — "a StorageDriver borrows no
// AList-fs subtree by default" — so a real sign succeeds only once that
// separate FS-grant-translation seam is filled in (out of scope here). This
// test pins that our own grant gate (the "sign" host_api check) is not what
// blocks it.
func TestPluginUiGrantedCapabilities(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "pluginui.granted.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })

	pkg := writePermPluginPackage(t, id, ext.Permissions{
		StorageTokens: []string{"self"},
		HostAPIs:      []string{"sign", "store"},
	})
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Enable(id); err != nil {
		t.Fatalf("enable: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Disable(id) })

	// scoped-token: 200 with a non-empty token.
	body, _ := json.Marshal(PluginUiScopedTokenReq{Scopes: []pluginUiScopeReq{{Path: "/self/x", Write: false}}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/scoped-token", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	IssuePluginUiScopedToken(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("scoped-token: want 200, got %d: %s", code, w.Body.String())
	}
	var tokResp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tokResp); err != nil {
		t.Fatalf("decode scoped-token resp: %v (%s)", err, w.Body.String())
	}
	if tokResp.Data.Token == "" {
		t.Fatal("scoped-token: empty token in response")
	}

	// kv put -> 200.
	putBody, _ := json.Marshal(PluginUiKvValueReq{Value: base64.StdEncoding.EncodeToString([]byte("hello"))})
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}, {Key: "key", Value: "k1"}}
	c.Request = httptest.NewRequest("PUT", "/api/v4/admin/plugins/"+id+"/kv/k1", bytes.NewReader(putBody))
	c.Request.Header.Set("Content-Type", "application/json")
	SetPluginUiKv(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("kv put: want 200, got %d: %s", code, w.Body.String())
	}

	// kv get -> 200, round-trips the value written above.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}, {Key: "key", Value: "k1"}}
	c.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins/"+id+"/kv/k1", nil)
	GetPluginUiKv(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("kv get: want 200, got %d: %s", code, w.Body.String())
	}
	var kvResp struct {
		Data struct {
			Value string `json:"value"`
			Found bool   `json:"found"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &kvResp); err != nil {
		t.Fatalf("decode kv get resp: %v (%s)", err, w.Body.String())
	}
	if !kvResp.Data.Found {
		t.Fatal("kv get: found=false, want the value just written")
	}
	got, err := base64.StdEncoding.DecodeString(kvResp.Data.Value)
	if err != nil || string(got) != "hello" {
		t.Fatalf("kv get: round-trip mismatch, got %q err=%v", got, err)
	}

	// sign-link: the "sign" grant gate must let the request through (not 403).
	signBody, _ := json.Marshal(PluginUiSignLinkReq{
		Ref:   pluginUiObjRef{StorageID: "1", Anchor: "/x"},
		Scope: "download",
		TTL:   60,
	})
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/sign-link", bytes.NewReader(signBody))
	c.Request.Header.Set("Content-Type", "application/json")
	SignPluginUiLink(c)
	if code := respCode(t, w); code == 403 {
		t.Fatalf("sign-link: grant includes \"sign\", must not be blocked by our own gate (403): %s", w.Body.String())
	}
}

// TestPluginUiUngrantedCapabilitiesForbidden drives all three endpoints for a
// plugin whose Grant carries none of storage_tokens/sign/store: each must
// refuse with 403 before touching the underlying capability.
func TestPluginUiUngrantedCapabilitiesForbidden(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "pluginui.ungranted.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })

	pkg := writePermPluginPackage(t, id, ext.Permissions{HostAPIs: []string{"events"}})
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Enable(id); err != nil {
		t.Fatalf("enable: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Disable(id) })

	// scoped-token: no storage_tokens granted -> 403.
	body, _ := json.Marshal(PluginUiScopedTokenReq{Scopes: []pluginUiScopeReq{{Path: "/x"}}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/scoped-token", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	IssuePluginUiScopedToken(c)
	if code := respCode(t, w); code != 403 {
		t.Fatalf("scoped-token without storage_tokens: want 403, got %d: %s", code, w.Body.String())
	}

	// sign-link: no "sign" granted -> 403.
	signBody, _ := json.Marshal(PluginUiSignLinkReq{Ref: pluginUiObjRef{Anchor: "/x"}, Scope: "download"})
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/sign-link", bytes.NewReader(signBody))
	c.Request.Header.Set("Content-Type", "application/json")
	SignPluginUiLink(c)
	if code := respCode(t, w); code != 403 {
		t.Fatalf("sign-link without sign: want 403, got %d: %s", code, w.Body.String())
	}

	// kv get: no "store" granted -> 403.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}, {Key: "key", Value: "k1"}}
	c.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins/"+id+"/kv/k1", nil)
	GetPluginUiKv(c)
	if code := respCode(t, w); code != 403 {
		t.Fatalf("kv get without store: want 403, got %d: %s", code, w.Body.String())
	}

	// kv put: no "store" granted -> 403.
	putBody, _ := json.Marshal(PluginUiKvValueReq{Value: base64.StdEncoding.EncodeToString([]byte("x"))})
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}, {Key: "key", Value: "k1"}}
	c.Request = httptest.NewRequest("PUT", "/api/v4/admin/plugins/"+id+"/kv/k1", bytes.NewReader(putBody))
	c.Request.Header.Set("Content-Type", "application/json")
	SetPluginUiKv(c)
	if code := respCode(t, w); code != 403 {
		t.Fatalf("kv put without store: want 403, got %d: %s", code, w.Body.String())
	}
}

// TestPluginUiScopedTokenOverScopeRejected is the red-team clamp check (Task 6):
// a plugin whose Grant DOES carry storage_tokens (so it clears the 403 gate)
// still cannot mint a token for a path outside its manifest-declared
// storage_tokens. The manifest declares only "self"; a request for "/etc/passwd"
// must be rejected by IssueScopedToken's scopeDeclared clamp (non-200,
// specifically 400), never issuing an over-scoped token.
func TestPluginUiScopedTokenOverScopeRejected(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "pluginui.overscope.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })

	pkg := writePermPluginPackage(t, id, ext.Permissions{StorageTokens: []string{"self"}})
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Enable(id); err != nil {
		t.Fatalf("enable: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Disable(id) })

	// A path outside the manifest's declared "self" storage_tokens.
	body, _ := json.Marshal(PluginUiScopedTokenReq{Scopes: []pluginUiScopeReq{{Path: "/etc/passwd", Write: true}}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/scoped-token", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	IssuePluginUiScopedToken(c)
	if code := respCode(t, w); code == 200 {
		t.Fatalf("over-scope scoped-token: must NOT issue a token for a path outside manifest storage_tokens, got 200: %s", w.Body.String())
	}
	if code := respCode(t, w); code != 400 {
		t.Fatalf("over-scope scoped-token: want 400 (clamp reject), got %d: %s", code, w.Body.String())
	}
	// The response must not carry a token.
	var tokResp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tokResp)
	if tokResp.Data.Token != "" {
		t.Fatalf("over-scope scoped-token: response leaked a token %q", tokResp.Data.Token)
	}

	// A well-scoped request under "self" from the SAME plugin still succeeds —
	// proving the rejection above is the scope clamp, not a blanket failure.
	okBody, _ := json.Marshal(PluginUiScopedTokenReq{Scopes: []pluginUiScopeReq{{Path: "/self/ok"}}})
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/scoped-token", bytes.NewReader(okBody))
	c.Request.Header.Set("Content-Type", "application/json")
	IssuePluginUiScopedToken(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("in-scope scoped-token: want 200, got %d: %s", code, w.Body.String())
	}
}

// TestPluginUiNotInstalledReturns404 pins the not-installed path: every
// endpoint refuses with 404 for a plugin id that was never installed.
func TestPluginUiNotInstalledReturns404(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "pluginui.missing.plugin"

	body, _ := json.Marshal(PluginUiScopedTokenReq{Scopes: []pluginUiScopeReq{{Path: "/x"}}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/scoped-token", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	IssuePluginUiScopedToken(c)
	if code := respCode(t, w); code != 404 {
		t.Fatalf("scoped-token for uninstalled plugin: want 404, got %d: %s", code, w.Body.String())
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}, {Key: "key", Value: "k1"}}
	c.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins/"+id+"/kv/k1", nil)
	GetPluginUiKv(c)
	if code := respCode(t, w); code != 404 {
		t.Fatalf("kv get for uninstalled plugin: want 404, got %d: %s", code, w.Body.String())
	}
}
