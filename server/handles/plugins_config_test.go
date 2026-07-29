package handles

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// TestPluginConfig_SetThenGet drives GetPluginConfig/SetPluginConfig directly
// (mirrors the grant handler tests' pattern: setupPluginGuardEnv installs a
// plugin via writeGuardPluginPackage + mgr.Install, then a manually built
// gin.Context invokes the handler and respCode reads the envelope code).
// GET before any save must report an empty {"config":...} object, POST must
// persist and round-trip through a second GET, and a raw invalid-JSON body
// must be rejected with 400.
func TestPluginConfig_SetThenGet(t *testing.T) {
	setupPluginGuardEnv(t)
	gin.SetMode(gin.TestMode)
	const id = "config.roundtrip.plugin"

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Uninstall(id) })
	if _, err := mgr.Install(writeGuardPluginPackage(t, id)); err != nil {
		t.Fatalf("install: %v", err)
	}

	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(ctxkey.User, guardPluginAdmin())
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Request = httptest.NewRequest("GET", "/api/v4/admin/plugins/"+id+"/config", nil)
		GetPluginConfig(c)
		return w
	}
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(ctxkey.User, guardPluginAdmin())
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Request = httptest.NewRequest("POST", "/api/v4/admin/plugins/"+id+"/config", bytes.NewReader([]byte(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		SetPluginConfig(c)
		return w
	}

	// GET before set -> 200 with an empty config object.
	w := get()
	if code := respCode(t, w); code != 200 || !strings.Contains(w.Body.String(), `"config"`) {
		t.Fatalf("GET config before set: code=%d body=%s", code, w.Body.String())
	}

	// POST valid config -> 200.
	w = post(`{"config":{"greeting":"hi"}}`)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("POST config: want 200, got %d body=%s", code, w.Body.String())
	}

	// GET again -> contains the saved value.
	w = get()
	if !strings.Contains(w.Body.String(), "greeting") || !strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("GET after set should contain greeting/hi; body=%s", w.Body.String())
	}

	// POST with a raw invalid-JSON token -> 400.
	w = post(`{"config":[}}`)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("POST invalid JSON: want 400, got %d body=%s", code, w.Body.String())
	}
}
