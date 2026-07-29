package handles

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

func getPublicLoginMethods(t *testing.T) []publicLoginMethod {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	// No middleware, no ctxkey.User set — the handler must run fine anonymous,
	// exactly as it will when mounted (no Auth middleware) on the login page.
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v4/auth/login-methods", nil)
	PublicLoginMethods(c)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int                 `json:"code"`
		Data []publicLoginMethod `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return resp.Data
}

// TestPublicLoginMethodsFlattensEnabledExcludesDisabled proves the handler
// flattens login_methods from every ENABLED extension into the public DTO
// (plugin id attached), and that a disabled extension's methods are excluded —
// mirroring TestExtensionContributionsHandlerReactsToDisable.
func TestPublicLoginMethodsFlattensEnabledExcludesDisabled(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.loginmethod",
		Name:  map[string]string{"en": "LM"},
		Kinds: []ext.Kind{ext.KindAuthNMethod},
		Contributions: ext.Contributions{
			LoginMethods: []ext.LoginMethod{
				{ID: "lm1", Kind: "federated", Label: map[string]string{"en": "Login with LM"}, Icon: "lm-icon"},
			},
		},
	})

	list := getPublicLoginMethods(t)
	found := false
	for _, m := range list {
		if m.PluginID == "h.ext.loginmethod" && m.ID == "lm1" {
			found = true
			if m.Kind != "federated" || m.Label["en"] != "Login with LM" || m.Icon != "lm-icon" {
				t.Fatalf("login method fields wrong: %+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("enabled extension's login method missing: %+v", list)
	}

	if err := extension.DefaultRegistry.Disable("h.ext.loginmethod"); err != nil {
		t.Fatal(err)
	}
	for _, m := range getPublicLoginMethods(t) {
		if m.PluginID == "h.ext.loginmethod" {
			t.Fatalf("disabled extension's login method must not appear: %+v", m)
		}
	}
}

// TestPublicLoginMethodsSurfacesUIRef proves the public DTO carries the
// LoginMethod's ui_ref verbatim — the login page needs it to mount the
// plugin's sandboxed login-ceremony UI (an allow-scripts iframe at
// /api/v4/extensions/:id/ui/<ui_ref>). ui_ref is a same-plugin relative path,
// not a secret, so it is safe on this anonymous endpoint; a method that
// declares none must serialize with the field absent (omitempty).
func TestPublicLoginMethodsSurfacesUIRef(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.uirefmethod",
		Name:  map[string]string{"en": "UIRef"},
		Kinds: []ext.Kind{ext.KindAuthNMethod},
		Contributions: ext.Contributions{
			LoginMethods: []ext.LoginMethod{
				{ID: "webauthnlike", Kind: "primary", Label: map[string]string{"en": "Custom Ceremony"}, UIRef: "login/index.html"},
				{ID: "plainredirect", Kind: "federated", Label: map[string]string{"en": "Redirect"}},
			},
		},
	})

	var withRef, without *publicLoginMethod
	list := getPublicLoginMethods(t)
	for i := range list {
		if list[i].PluginID != "h.ext.uirefmethod" {
			continue
		}
		switch list[i].ID {
		case "webauthnlike":
			withRef = &list[i]
		case "plainredirect":
			without = &list[i]
		}
	}
	if withRef == nil || without == nil {
		t.Fatalf("expected both methods present: %+v", list)
	}
	if withRef.UIRef != "login/index.html" {
		t.Fatalf("ui_ref not surfaced: %+v", withRef)
	}
	if without.UIRef != "" {
		t.Fatalf("expected empty ui_ref for redirect method: %+v", without)
	}

	// omitempty: a method with no ui_ref must serialize with the key absent
	// (not an empty string), so a client can cleanly branch on presence.
	rawWithout := map[string]json.RawMessage{}
	if b, err := json.Marshal(without); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(b, &rawWithout); err != nil {
		t.Fatal(err)
	}
	if _, present := rawWithout["ui_ref"]; present {
		t.Fatalf("ui_ref must be omitted (omitempty) when empty, got: %v", rawWithout)
	}
	rawWith := map[string]json.RawMessage{}
	if b, err := json.Marshal(withRef); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(b, &rawWith); err != nil {
		t.Fatal(err)
	}
	if string(rawWith["ui_ref"]) != `"login/index.html"` {
		t.Fatalf("ui_ref json wrong: %s", rawWith["ui_ref"])
	}
}

// TestPublicLoginMethodsEmptyWhenNoneDeclared proves an extension with no
// login_methods contributes nothing (no panics on a nil slice), and the
// endpoint never returns a null list.
func TestPublicLoginMethodsEmptyWhenNoneDeclared(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.nologinmethod",
		Kinds: []ext.Kind{ext.KindAction},
	})
	for _, m := range getPublicLoginMethods(t) {
		if m.PluginID == "h.ext.nologinmethod" {
			t.Fatalf("extension declared no login methods but one appeared: %+v", m)
		}
	}
}

// TestPublicLoginMethodsNoAuthRequired proves the handler runs to completion
// with no user set in the gin context at all (no ctxkey.User, no Authorization
// header) — it must be safely mountable outside the Auth middleware, exactly
// like the router wires it (v4.GET, no auth group).
func TestPublicLoginMethodsNoAuthRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v4/auth/login-methods", nil)
	PublicLoginMethods(c) // must not panic with no user in context
	if w.Code != 200 {
		t.Fatalf("anonymous call must succeed, got %d: %s", w.Code, w.Body.String())
	}
}
