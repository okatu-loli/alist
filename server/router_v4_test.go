package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	_ "github.com/alist-org/alist/v3/drivers"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/bootstrap/data"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// v4Env boots a real server (sqlite tempdir, Local storage with one file) and
// logs the admin in — the Step 13 contract tests all drive full HTTP through
// gin, same as a client would.
type v4Env struct {
	e     *gin.Engine
	token string
}

func newV4Env(t *testing.T) *v4Env {
	t.Helper()

	oldDataDir := flags.DataDir
	oldMigrateMode := flags.MigrateMode
	oldStoragesLoaded := conf.StoragesLoaded
	oldGinMode := gin.Mode()
	t.Cleanup(func() {
		flags.DataDir = oldDataDir
		flags.MigrateMode = oldMigrateMode
		conf.StoragesLoaded = oldStoragesLoaded
		gin.SetMode(oldGinMode)
	})

	flags.DataDir = t.TempDir()
	flags.MigrateMode = "auto"
	bootstrap.InitConfig()
	bootstrap.InitDB()
	// Another test in this package may have booted its own database first; op
	// caches would otherwise serve that database's entities — which would both
	// trick InitData's "already seeded" checks into skipping the fresh DB and
	// misidentify roles whose ids shift between seeds.
	op.ResetCachesForTest()
	data.InitData()

	storageDir := t.TempDir()
	if err := os.WriteFile(storageDir+"/hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: "/local",
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, storageDir),
	}); err != nil {
		t.Fatalf("create storage: %v", err)
	}
	conf.StoragesLoaded = true

	gin.SetMode(gin.TestMode)
	e := gin.New()
	Init(e)

	admin, err := op.GetAdmin()
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	admin.SetPassword("v4-contract-test")
	if err := op.UpdateUser(admin); err != nil {
		t.Fatalf("update admin: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"username": admin.Username, "password": "v4-contract-test"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v4/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("v4 login failed: %d %s", w.Code, w.Body.String())
	}
	var loginResp struct {
		Version string `json:"version"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("parse login: %v", err)
	}
	if loginResp.Version != "v4" {
		t.Fatalf("v4 envelope must carry version=v4, got %q", loginResp.Version)
	}
	return &v4Env{e: e, token: loginResp.Data.Token}
}

func (env *v4Env) do(t *testing.T, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return env.doAs(t, env.token, method, path, body)
}

// doAs is env.do with an explicit bearer token, so a test can drive a
// request as a caller other than the seeded admin (e.g. a freshly created
// non-admin user, to exercise an admin-gated route's rejection path).
func (env *v4Env) doAs(t *testing.T, token, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	w := httptest.NewRecorder()
	env.e.ServeHTTP(w, req)
	var envelope map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &envelope)
	return w, envelope
}

// login logs a username/password in against the running v4 server and
// returns the issued token.
func (env *v4Env) login(t *testing.T, username, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v4/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	env.e.ServeHTTP(w, req)
	var loginResp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("parse login for %q: %v", username, err)
	}
	if loginResp.Data.Token == "" {
		t.Fatalf("login for %q returned no token: %s", username, w.Body.String())
	}
	return loginResp.Data.Token
}

func TestV4Contract(t *testing.T) {
	env := newV4Env(t)

	t.Run("envelope version only on v4", func(t *testing.T) {
		_, v4 := env.do(t, "GET", "/api/v4/me", nil)
		if v4["version"] != "v4" {
			t.Errorf("v4 response missing version: %v", v4)
		}
		_, v3 := env.do(t, "GET", "/api/me", nil)
		if _, has := v3["version"]; has {
			t.Errorf("legacy /api response must not carry version: %v", v3)
		}
	})

	t.Run("v3 advertises deprecation", func(t *testing.T) {
		w, _ := env.do(t, "GET", "/api/me", nil)
		if w.Header().Get("Deprecation") != "true" {
			t.Error("legacy /api must send Deprecation header")
		}
		if link := w.Header().Get("Link"); link != `</api/v4>; rel="successor-version"` {
			t.Errorf("unexpected Link header: %q", link)
		}
		w4, _ := env.do(t, "GET", "/api/v4/me", nil)
		if w4.Header().Get("Deprecation") != "" {
			t.Error("v4 must not send Deprecation header")
		}
	})

	t.Run("fs reads are GET; wrong verb is 405", func(t *testing.T) {
		w, envl := env.do(t, "GET", "/api/v4/fs/list?path=/local", nil)
		if w.Code != 200 || envl["code"] != float64(200) {
			t.Fatalf("GET fs/list failed: %d %v", w.Code, envl)
		}
		content := envl["data"].(map[string]any)["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("expected 1 object in /local, got %v", content)
		}
		if w, _ := env.do(t, "POST", "/api/v4/fs/list", map[string]any{"path": "/local"}); w.Code != 405 {
			t.Errorf("POST on v4 read endpoint should be 405, got %d", w.Code)
		}
	})

	t.Run("logout is POST in v4", func(t *testing.T) {
		if w, _ := env.do(t, "GET", "/api/v4/auth/logout", nil); w.Code != 405 {
			t.Errorf("GET logout should be 405 in v4, got %d", w.Code)
		}
	})

	t.Run("path-param resources: user CRUD", func(t *testing.T) {
		// A creatable role id (admin/guest are rejected by CreateUser): pick
		// the preset "viewer" from the seeded roles via the v4 resource itself.
		_, roles := env.do(t, "GET", "/api/v4/admin/roles", nil)
		var roleID float64
		for _, r := range roles["data"].(map[string]any)["content"].([]any) {
			if r.(map[string]any)["name"] == "viewer" {
				roleID = r.(map[string]any)["id"].(float64)
			}
		}
		if roleID == 0 {
			t.Fatalf("seeded viewer role not found: %v", roles)
		}
		_, created := env.do(t, "POST", "/api/v4/admin/users",
			map[string]any{"username": "v4tester", "password": "pw123456", "base_path": "/", "role": []float64{roleID}})
		if created["code"] != float64(200) {
			t.Fatalf("create user failed: %v", created)
		}
		_, list := env.do(t, "GET", "/api/v4/admin/users", nil)
		var id float64
		for _, u := range list["data"].(map[string]any)["content"].([]any) {
			if u.(map[string]any)["username"] == "v4tester" {
				id = u.(map[string]any)["id"].(float64)
			}
		}
		if id == 0 {
			t.Fatalf("created user not in list: %v", list)
		}
		path := fmt.Sprintf("/api/v4/admin/users/%d", int(id))

		_, got := env.do(t, "GET", path, nil)
		gotData, ok := got["data"].(map[string]any)
		if !ok || gotData["username"] != "v4tester" {
			t.Fatalf("GET by path param failed: %v", got)
		}
		// PUT without id in body — the adapter injects it from the path.
		_, updated := env.do(t, "PUT", path,
			map[string]any{"username": "v4tester", "password": "", "disabled": true, "role": []float64{roleID}})
		if updated["code"] != float64(200) {
			t.Fatalf("PUT by path param failed: %v", updated)
		}
		_, after := env.do(t, "GET", path, nil)
		afterData, ok := after["data"].(map[string]any)
		if !ok || afterData["disabled"] != true {
			t.Fatalf("update did not apply: %v", after)
		}
		if w, _ := env.do(t, "DELETE", path, nil); w.Code != 200 {
			t.Errorf("DELETE by path param failed: %d", w.Code)
		}
		_, gone := env.do(t, "GET", path, nil)
		if gone["code"] == float64(200) {
			t.Errorf("user still present after DELETE: %v", gone)
		}
	})

	t.Run("settings resource + token action", func(t *testing.T) {
		// Round-trip: bulk PUT persists a setting, the path-param GET reads it
		// back (seeded keys can be dynamic — e.g. "version" is injected into
		// list responses without a DB row — so write our own first).
		_, saved := env.do(t, "PUT", "/api/v4/admin/settings",
			[]map[string]any{{"key": "v4_contract_probe", "value": "1", "type": "string", "group": 0, "flag": 0}})
		if saved["code"] != float64(200) {
			t.Fatalf("PUT settings failed: %v", saved)
		}
		_, got := env.do(t, "GET", "/api/v4/admin/settings/v4_contract_probe", nil)
		if got["code"] != float64(200) {
			t.Fatalf("GET settings/:key failed: %v", got)
		}
		_, reset := env.do(t, "POST", "/api/v4/admin/settings/token/reset", nil)
		if reset["code"] != float64(200) {
			t.Errorf("token reset failed: %v", reset)
		}
		_, set := env.do(t, "PUT", "/api/v4/admin/settings/token", map[string]any{"token": "v4-fixed-token"})
		if set["code"] != float64(200) {
			t.Errorf("token set failed: %v", set)
		}
	})

	t.Run("fs link admin-gated structurally", func(t *testing.T) {
		_, link := env.do(t, "POST", "/api/v4/admin/fs/link", map[string]any{"path": "/local/hello.txt"})
		if link["code"] != float64(200) {
			t.Errorf("admin fs/link failed: %v", link)
		}
	})

	t.Run("agents: admin-gated, pairing start, CRUD", func(t *testing.T) {
		// A non-admin (viewer) caller, to prove the whole resource is
		// admin-gated end to end through the real middleware chain — not
		// merely by convention in the handler.
		_, roles := env.do(t, "GET", "/api/v4/admin/roles", nil)
		var roleID float64
		for _, r := range roles["data"].(map[string]any)["content"].([]any) {
			if r.(map[string]any)["name"] == "viewer" {
				roleID = r.(map[string]any)["id"].(float64)
			}
		}
		if roleID == 0 {
			t.Fatalf("seeded viewer role not found: %v", roles)
		}
		_, created := env.do(t, "POST", "/api/v4/admin/users",
			map[string]any{"username": "v4agentnonadmin", "password": "pw123456", "base_path": "/", "role": []float64{roleID}})
		if created["code"] != float64(200) {
			t.Fatalf("create non-admin user failed: %v", created)
		}
		nonAdminToken := env.login(t, "v4agentnonadmin", "pw123456")

		for _, req := range []struct{ method, path string }{
			{"POST", "/api/v4/admin/agents/pairing"},
			{"POST", "/api/v4/admin/agents/pairing/reverse/token"},
			{"POST", "/api/v4/admin/agents/pairing/reverse"},
			{"GET", "/api/v4/admin/agents"},
			{"PATCH", "/api/v4/admin/agents/1"},
			{"DELETE", "/api/v4/admin/agents/1"},
			{"GET", "/api/v4/admin/agents/1/logs"},
			{"GET", "/api/v4/admin/agents/1/tasks"},
			{"GET", "/api/v4/admin/agent-tasks"},
		} {
			w, envl := env.doAs(t, nonAdminToken, req.method, req.path, nil)
			if w.Code == 200 && envl["code"] == float64(200) {
				t.Errorf("%s %s must be admin-gated, non-admin got through: %v", req.method, req.path, envl)
			}
		}

		// Admin: start pairing returns a code/expiry/install_hint.
		_, pairing := env.do(t, "POST", "/api/v4/admin/agents/pairing", nil)
		if pairing["code"] != float64(200) {
			t.Fatalf("pairing start failed: %v", pairing)
		}
		pairingData := pairing["data"].(map[string]any)
		if code, _ := pairingData["code"].(string); len(code) < 32 {
			t.Errorf("pairing code must be high-entropy, got %v", pairingData["code"])
		}
		if pairingData["expires_at"] == nil {
			t.Error("pairing response missing expires_at")
		}
		if hint, _ := pairingData["install_hint"].(string); hint == "" {
			t.Error("pairing response missing install_hint")
		}

		// Admin: list starts empty (no admin endpoint creates an Agent row
		// this wave — pairing completion lands in W12-2).
		_, list := env.do(t, "GET", "/api/v4/admin/agents", nil)
		if list["code"] != float64(200) {
			t.Fatalf("list agents failed: %v", list)
		}

		// Admin: task list is reachable (empty is fine -- no task-creation
		// flow exercised in this router-level test, see server/handles for
		// the handler's own filter/paging/ordering coverage, design W12-4).
		_, taskList := env.do(t, "GET", "/api/v4/admin/agent-tasks", nil)
		if taskList["code"] != float64(200) {
			t.Fatalf("list agent tasks failed: %v", taskList)
		}
		if _, ok := taskList["data"].([]any); !ok {
			t.Errorf("list agent tasks data must be an array, got %v", taskList["data"])
		}
	})
}

// TestV4PasskeyGroupGated pins the W5 self-management gate WIRING: the
// /api/v4/auth/passkey group carries middlewares.RequireExtensionEnabled(
// "builtin.passkey"), so disabling the passkey built-in 403s register/list/delete
// at the route — the real boundary, not merely a hidden account card. The router
// test harness does not run cmd.Init's InitCoreFeatureExtensions, so this
// registers the manageable Feature entry the same way boot does, then toggles it.
func TestV4PasskeyGroupGated(t *testing.T) {
	env := newV4Env(t)

	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:      "builtin.passkey",
		Name:    map[string]string{"en": "Passkey", "zh": "通行密钥"},
		Kinds:   []ext.Kind{ext.KindFeature},
		Version: "builtin",
	}); err != nil {
		t.Fatalf("register builtin.passkey: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister("builtin.passkey") })

	// Enabled → the self-management list is reachable (admin has no passkeys → 200).
	if w, envl := env.do(t, "GET", "/api/v4/auth/passkey", nil); w.Code != 200 || envl["code"] != float64(200) {
		t.Fatalf("enabled builtin.passkey: GET /auth/passkey must succeed, got http=%d body=%v", w.Code, envl)
	}

	// Disabled → the whole group 403s (register/list/delete).
	if err := extension.DefaultRegistry.Disable("builtin.passkey"); err != nil {
		t.Fatalf("disable builtin.passkey: %v", err)
	}
	for _, req := range []struct{ method, path string }{
		{"GET", "/api/v4/auth/passkey"},
		{"POST", "/api/v4/auth/passkey/register/begin"},
		{"DELETE", "/api/v4/auth/passkey/some-cred-id"},
	} {
		_, envl := env.do(t, req.method, req.path, nil)
		if envl["code"] != float64(403) {
			t.Errorf("disabled builtin.passkey: %s %s must be 403, got %v", req.method, req.path, envl["code"])
		}
	}

	// LOCKOUT SAFETY (design W5 security review): disabling passkey must NEVER
	// lock an admin out — the password path (/auth/login, handles.Login in
	// auth.go) carries no builtin.passkey gate, so it keeps working. Prove it by
	// logging the admin in by password while passkey is still disabled.
	if tok := env.login(t, "admin", "v4-contract-test"); tok == "" {
		t.Fatal("password login must still succeed while passkey is disabled (no admin lockout)")
	}
}
