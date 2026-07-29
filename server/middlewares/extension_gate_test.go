package middlewares

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// gateEngine mounts RequireExtensionEnabled(id) ahead of a sentinel handler that
// writes 200 only when the gate lets the request through.
func gateEngine(id string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RequireExtensionEnabled(id), func(c *gin.Context) {
		c.JSON(200, gin.H{"code": 200, "message": "ok"})
	})
	return r
}

// bodyCode reads the envelope {"code":...} alist error/success responses carry
// (both are HTTP 200 with the real code in the body).
func bodyCode(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse body: %v (%s)", err, w.Body.String())
	}
	return env.Code
}

func hitGate(r *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	r.ServeHTTP(w, req)
	return w
}

// TestRequireExtensionEnabledGate pins the three states: an unregistered id is
// rejected, an enabled built-in passes through to the handler, and a disabled
// built-in is rejected with a 403 "feature disabled".
func TestRequireExtensionEnabledGate(t *testing.T) {
	const id = "builtin.gatetest"
	// Unknown id → 403 before any registration.
	if got := bodyCode(t, hitGate(gateEngine(id))); got != 403 {
		t.Fatalf("unknown feature id must 403, got %d", got)
	}

	// Register into the process-wide DefaultRegistry (what the middleware reads).
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID: id, Version: "builtin", Kinds: []ext.Kind{ext.KindFeature},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(id) })

	// Enabled → handler runs (200).
	if got := bodyCode(t, hitGate(gateEngine(id))); got != 200 {
		t.Fatalf("enabled feature must pass the gate (200), got %d", got)
	}

	// Disabled → 403 "feature disabled".
	if err := extension.DisableBuiltin(id); err != nil {
		t.Fatalf("disable: %v", err)
	}
	t.Cleanup(func() { _ = extension.EnableBuiltin(id) })
	w := hitGate(gateEngine(id))
	if got := bodyCode(t, w); got != 403 {
		t.Fatalf("disabled feature must 403, got %d", got)
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "feature disabled") {
		t.Fatalf("403 body must say feature disabled, got %s", w.Body.String())
	}

	// Re-enabled → passes again.
	if err := extension.EnableBuiltin(id); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if got := bodyCode(t, hitGate(gateEngine(id))); got != 200 {
		t.Fatalf("re-enabled feature must pass the gate, got %d", got)
	}
}
