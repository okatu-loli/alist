package handles

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

func pluginSourcesAdmin() *model.User {
	return &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}
}

// resetPluginSources clears the plugin_sources setting before and after the
// test so source-import tests don't leak state between each other.
func resetPluginSources(t *testing.T) {
	t.Helper()
	clear := func() {
		if err := op.SaveSettingItem(&model.SettingItem{Key: conf.PluginSources, Value: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	t.Cleanup(clear)
}

// TestAddPluginSourceRejectsBadPubkey drives AddPluginSource with an
// undecodable ed25519 hex key and asserts it is rejected before anything is
// persisted.
func TestAddPluginSourceRejectsBadPubkey(t *testing.T) {
	resetPluginSources(t)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, pluginSourcesAdmin())
	body := `{"name":"myrepo","index_url":"https://example.com/index.json","pubkey":"zz"}`
	req := httptest.NewRequest("POST", "/api/v4/admin/plugins/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	AddPluginSource(c)

	if code := respCode(t, w); code != 400 {
		t.Fatalf("bad pubkey: expected 400, got %d: %s", code, w.Body.String())
	}
}

// TestAddPluginSourceThenListThenDeleteOfficial drives the full admin flow:
// a well-formed source imports (200, with a reachability warning since the
// index is unreachable), GET /sources then lists both the builtin official
// source and the new one, and DELETE on the official name is rejected (the
// built-in trust root cannot be removed).
func TestAddPluginSourceThenListThenDeleteOfficial(t *testing.T) {
	resetPluginSources(t)
	admin := pluginSourcesAdmin()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)

	gin.SetMode(gin.TestMode)

	// Add a valid source. Its index_url points at a closed local port so the
	// best-effort reachability probe fails fast without needing real network
	// access — the add must still succeed (200) with a non-empty warning.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, admin)
	body := fmt.Sprintf(`{"name":"myrepo","index_url":"https://127.0.0.1:1/index.json","pubkey":"%s"}`, pubHex)
	req := httptest.NewRequest("POST", "/api/v4/admin/plugins/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	AddPluginSource(c)

	if code := respCode(t, w); code != 200 {
		t.Fatalf("add valid source: expected 200, got %d: %s", code, w.Body.String())
	}
	var addResp struct {
		Data AddPluginSourceResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &addResp); err != nil {
		t.Fatalf("decode add response: %v (%s)", err, w.Body.String())
	}
	if addResp.Data.Warning == "" {
		t.Fatalf("expected a reachability warning for an unreachable index, got none: %+v", addResp.Data)
	}

	// List must include both the builtin official source and the new one.
	wList := httptest.NewRecorder()
	cList, _ := gin.CreateTestContext(wList)
	cList.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins/sources", nil)

	ListPluginSources(cList)

	var listResp struct {
		Data []PluginSourceView `json:"data"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list response: %v (%s)", err, wList.Body.String())
	}
	var foundBuiltin, foundNew bool
	for _, s := range listResp.Data {
		if s.Name == extension.OfficialSourceName && s.Builtin {
			foundBuiltin = true
		}
		if s.Name == "myrepo" && !s.Builtin {
			foundNew = true
		}
	}
	if !foundBuiltin || !foundNew {
		t.Fatalf("expected builtin official + myrepo in list, got %+v", listResp.Data)
	}

	// Deleting the built-in official source must be rejected.
	wDel := httptest.NewRecorder()
	cDel, _ := gin.CreateTestContext(wDel)
	cDel.Set(ctxkey.User, admin)
	cDel.Params = gin.Params{{Key: "name", Value: extension.OfficialSourceName}}
	cDel.Request = httptest.NewRequest("DELETE", "/api/v4/admin/plugins/sources/"+extension.OfficialSourceName, nil)

	DeletePluginSource(cDel)

	if code := respCode(t, wDel); code != 400 {
		t.Fatalf("delete official source: expected 400, got %d: %s", code, wDel.Body.String())
	}
}

// TestAddPluginSourceRejectsCaseInsensitiveDuplicateName drives AddPluginSource
// twice with names that differ only in case ("myrepo" then "MyRepo") and
// asserts the second add is rejected as a duplicate — the dup check must
// fold case the same way Validate does for reserved names.
func TestAddPluginSourceRejectsCaseInsensitiveDuplicateName(t *testing.T) {
	resetPluginSources(t)
	admin := pluginSourcesAdmin()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)

	gin.SetMode(gin.TestMode)

	addSource := func(name string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(ctxkey.User, admin)
		body := fmt.Sprintf(`{"name":%q,"index_url":"https://127.0.0.1:1/index.json","pubkey":"%s"}`, name, pubHex)
		req := httptest.NewRequest("POST", "/api/v4/admin/plugins/sources", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		c.Request = req

		AddPluginSource(c)
		return respCode(t, w)
	}

	if code := addSource("myrepo"); code != 200 {
		t.Fatalf("add first source: expected 200, got %d", code)
	}
	if code := addSource("MyRepo"); code != 400 {
		t.Fatalf("add case-insensitive duplicate: expected 400, got %d", code)
	}
}
