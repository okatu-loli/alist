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
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// Red-team regressions for G3 §1 (secret as a first-class citizen, step 1)
// on the plugin config_form side: a property marked x-secret must never
// round-trip through GetPluginConfig in full, and SetPluginConfig must not
// wipe a stored secret when the admin UI resubmits the redacted blank.
// Mirrors storage_secret_redteam_test.go one config shape over.

// writeSecretGuardPluginPackage is writeGuardPluginPackage (plugins_test.go)
// plus a config_form declaring one x-secret property ("api_token") and one
// plain property ("label"), so GetPluginConfig/SetPluginConfig have
// something real to redact/merge against.
func writeSecretGuardPluginPackage(t *testing.T, id string) string {
	t.Helper()
	return writeSecretGuardPluginPackageVersion(t, id, "1.0.0")
}

// writeSecretGuardPluginPackageVersion is writeSecretGuardPluginPackage with
// an explicit version, so a test can install v1 then Update to v2 of the
// same plugin id (mgr.Update requires the incoming version to differ from
// what's already installed).
func writeSecretGuardPluginPackageVersion(t *testing.T, id, version string) string {
	t.Helper()
	dir := t.TempDir()
	man := ext.Manifest{
		ID:      id,
		Name:    map[string]string{"en": id},
		Version: version,
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{
			Source:  "third-party",
			License: "MIT",
			Pricing: "free",
		},
		Contributions: ext.Contributions{
			ConfigForm: &ext.JSONSchema{
				Type: "object",
				Properties: map[string]*ext.JSONSchema{
					"api_token": {Type: "string", Secret: true},
					"label":     {Type: "string"},
				},
			},
		},
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			t.Fatal(err)
		}
		hdr.Name = e.Name()
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

const pluginSecretRedteamValue = "plugin-s3cr3t-must-not-leak"

func getPluginConfig(id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins/"+id+"/config", nil)
	GetPluginConfig(c)
	return w
}

func postPluginConfig(id, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/config", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	SetPluginConfig(c)
	return w
}

// TestRedteam_PluginConfigNeverEchoesSecretValue pins GetPluginConfig: after
// saving a config with a real api_token, reading it back must never contain
// the value, only the api_token_set sentinel — the non-secret "label" field
// passes through unchanged.
func TestRedteam_PluginConfigNeverEchoesSecretValue(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}

	w := postPluginConfig(id, `{"config":{"api_token":"`+pluginSecretRedteamValue+`","label":"prod"}}`)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("POST config: want 200, got %d: %s", code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, pluginSecretRedteamValue) {
		t.Fatalf("REGRESSION: secret leaked into the POST response: %s", body)
	}
	if !strings.Contains(body, `"api_token_set":true`) {
		t.Fatalf("POST response must report api_token_set:true: %s", body)
	}

	w = getPluginConfig(id)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("GET config: want 200, got %d: %s", code, w.Body.String())
	}
	body = w.Body.String()
	if strings.Contains(body, pluginSecretRedteamValue) {
		t.Fatalf("REGRESSION: secret leaked into the GET response: %s", body)
	}
	if !strings.Contains(body, `"api_token_set":true`) {
		t.Fatalf("GET response must report api_token_set:true: %s", body)
	}
	if !strings.Contains(body, "prod") {
		t.Fatalf("non-secret label field must still round-trip: %s", body)
	}

	// Sanity: the value really is persisted (admin-invisible, not
	// missing/broken) — read the raw stored config straight from the
	// manager, bypassing the redaction the handler applies.
	raw, err := mgr.GetConfig(id)
	if err != nil {
		t.Fatalf("mgr.GetConfig: %v", err)
	}
	if !strings.Contains(string(raw), pluginSecretRedteamValue) {
		t.Fatalf("the secret must actually be persisted, got %s", raw)
	}
}

// TestRedteam_PluginConfigAuditLogNeverContainsSecretValue pins the
// "日志/审计不含secret" red-team requirement: SetPluginConfig's audit.Event
// call only ever carries "plugin:"+id as its detail (server/handles/
// plugins.go) — never the submitted config JSON — so the secret can never
// reach the audit trail regardless of what was saved.
func TestRedteam_PluginConfigAuditLogNeverContainsSecretValue(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin.audit"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}

	audit.Start()
	w := postPluginConfig(id, `{"config":{"api_token":"`+pluginSecretRedteamValue+`"}}`)
	audit.Stop() // flushes everything buffered before returning
	if respCode(t, w) != 200 {
		t.Fatalf("POST config: want 200, got %d: %s", respCode(t, w), w.Body.String())
	}

	logs, _, err := db.GetAuditLogs(db.AuditLogFilter{ActionPrefix: "plugin.config"}, 1, 50)
	if err != nil {
		t.Fatalf("GetAuditLogs: %v", err)
	}
	found := false
	for _, l := range logs {
		if l.TargetRef == "plugin:"+id {
			found = true
		}
		if strings.Contains(l.TargetRef, pluginSecretRedteamValue) ||
			strings.Contains(l.Action, pluginSecretRedteamValue) ||
			strings.Contains(l.Actor, pluginSecretRedteamValue) {
			t.Fatalf("REGRESSION: secret leaked into an audit log row: %+v", l)
		}
	}
	if !found {
		t.Fatal("expected an audit row for plugin.config on plugin:" + id)
	}
}

// TestRedteam_PluginConfigUpdateWithoutSecretDoesNotWipeIt pins the "blank
// means keep" merge rule: saving again with a blank api_token (exactly what
// the admin UI resubmits after a GET, since it never received the real
// value) while changing the non-secret label must NOT wipe the stored
// token.
func TestRedteam_PluginConfigUpdateWithoutSecretDoesNotWipeIt(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin.update"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}

	if w := postPluginConfig(id, `{"config":{"api_token":"`+pluginSecretRedteamValue+`","label":"prod"}}`); respCode(t, w) != 200 {
		t.Fatalf("initial POST: want 200, got %d: %s", respCode(t, w), w.Body.String())
	}

	// Resubmit exactly what a GET would have shown: blank api_token, changed
	// label.
	w := postPluginConfig(id, `{"config":{"api_token":"","label":"staging"}}`)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("update POST: want 200, got %d: %s", code, w.Body.String())
	}

	raw, err := mgr.GetConfig(id)
	if err != nil {
		t.Fatalf("mgr.GetConfig: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	if m["api_token"] != pluginSecretRedteamValue {
		t.Fatalf("REGRESSION: update with a blank api_token wiped the stored secret, got %v", m["api_token"])
	}
	if m["label"] != "staging" {
		t.Fatalf("the deliberately-changed label field must still take effect, got %v", m["label"])
	}
}

// TestRedteam_PluginConfigUpdateWithNewSecretOverwritesIt confirms the merge
// rule only protects BLANK resubmissions.
func TestRedteam_PluginConfigUpdateWithNewSecretOverwritesIt(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin.change"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}

	if w := postPluginConfig(id, `{"config":{"api_token":"`+pluginSecretRedteamValue+`"}}`); respCode(t, w) != 200 {
		t.Fatalf("initial POST: want 200, got %d: %s", respCode(t, w), w.Body.String())
	}

	const newToken = "brand-new-plugin-secret"
	w := postPluginConfig(id, `{"config":{"api_token":"`+newToken+`"}}`)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("update POST: want 200, got %d: %s", code, w.Body.String())
	}

	raw, err := mgr.GetConfig(id)
	if err != nil {
		t.Fatalf("mgr.GetConfig: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	if m["api_token"] != newToken {
		t.Fatalf("an explicitly-submitted new token must be honored, got %v", m["api_token"])
	}
}

// TestRedteam_ListPluginsNeverEchoesSecretValue pins review round 1's
// CRITICAL finding: ListPlugins used to return mgr.List() straight through
// common.SuccessResp — extension.PluginState.Config is the raw, unredacted
// config_form JSON, so "list installed plugins" echoed every plugin's
// x-secret fields in full, entirely bypassing GetPluginConfig's redaction
// (which only ever guarded its own endpoint). Now routed through
// toPluginStateView, which always redacts.
func TestRedteam_ListPluginsNeverEchoesSecretValue(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin.list"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.SetConfig(id, json.RawMessage(`{"api_token":"`+pluginSecretRedteamValue+`","label":"prod"}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins", nil)
	ListPlugins(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("ListPlugins: want 200, got %d: %s", code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, pluginSecretRedteamValue) {
		t.Fatalf("REGRESSION: secret leaked into ListPlugins response: %s", body)
	}
	if !strings.Contains(body, `"api_token_set":true`) {
		t.Fatalf("ListPlugins response must report api_token_set:true for %s: %s", id, body)
	}
	if !strings.Contains(body, "\""+id+"\"") {
		t.Fatalf("ListPlugins response must still list the plugin: %s", body)
	}
}

// TestRedteam_UpdatePluginResponseNeverEchoesSecretValue pins review round
// 1's CRITICAL finding on the install/update path: installOrUpdate used to
// return mgr.Get(man.ID) straight through common.SuccessResp. A plugin
// updated to a new version keeps its previously-saved Config (Config is
// keyed by plugin id, not by version), so the update response echoed the
// stored secret in full. Now routed through toPluginStateView.
func TestRedteam_UpdatePluginResponseNeverEchoesSecretValue(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin.update-response"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackageVersion(t, id, "1.0.0")); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	if err := mgr.SetConfig(id, json.RawMessage(`{"api_token":"`+pluginSecretRedteamValue+`","label":"prod"}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	body, err := json.Marshal(PluginInstallReq{Path: writeSecretGuardPluginPackageVersion(t, id, "1.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/update", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	UpdatePlugin(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("UpdatePlugin: want 200, got %d: %s", code, w.Body.String())
	}
	respBody := w.Body.String()
	if strings.Contains(respBody, pluginSecretRedteamValue) {
		t.Fatalf("REGRESSION: secret leaked into UpdatePlugin response: %s", respBody)
	}
	if !strings.Contains(respBody, `"api_token_set":true`) {
		t.Fatalf("UpdatePlugin response must report api_token_set:true: %s", respBody)
	}
}

// TestRedteam_PluginConfigEncryptedAtRestOnDisk pins G4 §3: the plugin's
// config_form secret field must not be plaintext in state.json ON DISK
// either — GetConfig/mgr.GetConfig legitimately decrypt for in-process Go
// callers (see TestRedteam_PluginConfigNeverEchoesSecretValue's sanity
// check, which uses mgr.GetConfig deliberately), but the persisted file
// itself must carry the enc:v1: prefix.
func TestRedteam_PluginConfigEncryptedAtRestOnDisk(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin.ondisk"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.SetConfig(id, json.RawMessage(`{"api_token":"`+pluginSecretRedteamValue+`","label":"prod"}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(mgr.Dir(), extension.StateFile))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if strings.Contains(string(raw), pluginSecretRedteamValue) {
		t.Fatalf("REGRESSION (G4): secret stored in plaintext in state.json, want enc:v1:-encrypted: %s", raw)
	}
	if !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("state.json must carry an enc:v1:-prefixed value for the configured secret: %s", raw)
	}

	// Sanity: GetConfig (the in-process API) still returns the real value —
	// encryption at rest must not make the config unusable.
	cfg, err := mgr.GetConfig(id)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil {
		t.Fatal(err)
	}
	if m["api_token"] != pluginSecretRedteamValue {
		t.Fatalf("mgr.GetConfig must decrypt back to the real value, got %v", m["api_token"])
	}
}

// TestRedteam_PluginConfigRejectsValueCollidingWithReservedPrefix pins
// Minor 10 at the HTTP handler level: a fresh api_token value that literally
// starts with the reserved crypto.EncPrefixV1 ("enc:v1:") must be rejected
// by SetPluginConfig, not silently accepted and stored unencrypted (it would
// otherwise sail through encryptSecretConfigFields' "already encrypted,
// skip" check).
func TestRedteam_PluginConfigRejectsValueCollidingWithReservedPrefix(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "secret.redteam.plugin.reservedprefix"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeSecretGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}

	w := postPluginConfig(id, `{"config":{"api_token":"enc:v1:not-really-ciphertext","label":"prod"}}`)
	if respCode(t, w) == 200 {
		t.Fatal("SetPluginConfig must reject a value colliding with the reserved enc:v1: prefix")
	}

	raw, err := os.ReadFile(filepath.Join(mgr.Dir(), extension.StateFile))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if strings.Contains(string(raw), "not-really-ciphertext") {
		t.Fatalf("REGRESSION (Minor 10): rejected config must not have been written at all, got %s", raw)
	}
}
