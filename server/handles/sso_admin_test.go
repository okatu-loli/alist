package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/authn/sso"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// resetSSOProviders clears the persisted provider list after a CRUD test.
func resetSSOProviders(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = op.SaveSettingItem(&model.SettingItem{
			Key: conf.SSOProviders, Value: "[]", Type: conf.TypeString, Group: model.GLOBAL, Flag: model.PRIVATE,
		})
		resyncSSOMethods()
	})
}

func ctxWithJSON(method, path string, body any, params gin.Params) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Params = params
	return w, c
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"message"`
	Data json.RawMessage `json:"data"`
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, w.Body.String())
	}
	return e
}

func TestSSOProviderCRUDAndSecretRedaction(t *testing.T) {
	resetSSOProviders(t)

	// Create.
	w, c := ctxWithJSON("POST", "/api/v4/admin/sso/providers", map[string]any{
		"key": "acme", "name": "Acme", "issuer": "https://idp.acme.test",
		"client_id": "cid", "client_secret": "SUPER-SECRET", "enabled": true,
	}, nil)
	CreateSSOProvider(c)
	if e := decodeEnvelope(t, w); e.Code != 200 {
		t.Fatalf("create failed: %s", w.Body.String())
	}

	// The stored config must carry the secret...
	pc, ok := findSSOProvider("acme")
	if !ok || pc.ClientSecret != "SUPER-SECRET" {
		t.Fatalf("secret must be persisted, got %+v ok=%v", pc, ok)
	}

	// ...but the list endpoint must NEVER echo it back.
	w, c = ctxWithJSON("GET", "/api/v4/admin/sso/providers", nil, nil)
	ListSSOProviders(c)
	body := w.Body.String()
	if strings.Contains(body, "SUPER-SECRET") {
		t.Fatalf("list must not leak the client secret: %s", body)
	}
	if !strings.Contains(body, `"client_secret_set":true`) {
		t.Fatalf("list must report client_secret_set=true: %s", body)
	}

	// Update with a BLANK secret keeps the stored one (write-only credential).
	w, c = ctxWithJSON("PUT", "/api/v4/admin/sso/providers/acme", map[string]any{
		"name": "Acme Renamed", "issuer": "https://idp.acme.test", "client_id": "cid", "client_secret": "",
	}, gin.Params{{Key: "key", Value: "acme"}})
	UpdateSSOProvider(c)
	if e := decodeEnvelope(t, w); e.Code != 200 {
		t.Fatalf("update failed: %s", w.Body.String())
	}
	pc, _ = findSSOProvider("acme")
	if pc.ClientSecret != "SUPER-SECRET" {
		t.Fatal("a blank secret on update must preserve the stored secret")
	}
	if pc.Name != "Acme Renamed" {
		t.Fatalf("update must apply other fields, got name %q", pc.Name)
	}

	// Update with a NEW secret replaces it.
	w, c = ctxWithJSON("PUT", "/api/v4/admin/sso/providers/acme", map[string]any{
		"name": "Acme", "issuer": "https://idp.acme.test", "client_id": "cid", "client_secret": "NEW-SECRET",
	}, gin.Params{{Key: "key", Value: "acme"}})
	UpdateSSOProvider(c)
	pc, _ = findSSOProvider("acme")
	if pc.ClientSecret != "NEW-SECRET" {
		t.Fatalf("a non-blank secret must replace the stored one, got %q", pc.ClientSecret)
	}

	// Delete removes it.
	w, c = ctxWithJSON("DELETE", "/api/v4/admin/sso/providers/acme", nil, gin.Params{{Key: "key", Value: "acme"}})
	DeleteSSOProvider(c)
	if e := decodeEnvelope(t, w); e.Code != 200 {
		t.Fatalf("delete failed: %s", w.Body.String())
	}
	if _, ok := findSSOProvider("acme"); ok {
		t.Fatal("provider must be gone after delete")
	}
}

func TestSSOProviderDuplicateKeyRejected(t *testing.T) {
	resetSSOProviders(t)
	body := map[string]any{"key": "dup", "name": "Dup", "issuer": "https://i", "client_id": "c", "enabled": true}
	_, c := ctxWithJSON("POST", "/x", body, nil)
	CreateSSOProvider(c)

	w, c := ctxWithJSON("POST", "/x", body, nil)
	CreateSSOProvider(c)
	if e := decodeEnvelope(t, w); e.Code == 200 {
		t.Fatal("a duplicate provider key must be rejected")
	}
}

func TestSSOProviderInvalidRejected(t *testing.T) {
	resetSSOProviders(t)
	// A ':' in the key would corrupt the "<key>:<subject>" link format.
	w, c := ctxWithJSON("POST", "/x", map[string]any{
		"key": "bad:key", "name": "B", "issuer": "https://i", "client_id": "c",
	}, nil)
	CreateSSOProvider(c)
	if e := decodeEnvelope(t, w); e.Code == 200 {
		t.Fatal("an invalid provider key must be rejected")
	}
}

func TestPublicSSOProvidersOnlyEnabledNoSecrets(t *testing.T) {
	resetSSOProviders(t)
	list := []sso.ProviderConfig{
		{Key: "on", Name: "On", Issuer: "https://i1", ClientID: "c1", ClientSecret: "s1", Enabled: true},
		{Key: "off", Name: "Off", Issuer: "https://i2", ClientID: "c2", ClientSecret: "s2", Enabled: false},
	}
	if err := saveSSOProviders(list); err != nil {
		t.Fatal(err)
	}

	w, c := ctxWithJSON("GET", "/api/v4/auth/sso/providers", nil, nil)
	PublicSSOProviders(c)
	body := w.Body.String()
	if strings.Contains(body, "s1") || strings.Contains(body, "s2") || strings.Contains(body, "https://i1") {
		t.Fatalf("public list must expose no secrets or issuer internals: %s", body)
	}
	var e envelope
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	var pubs []ssoPublicProvider
	_ = json.Unmarshal(e.Data, &pubs)
	if len(pubs) != 1 || pubs[0].Key != "on" || pubs[0].ID != "sso:on" {
		t.Fatalf("public list must contain only the enabled provider, got %+v", pubs)
	}
}

func TestSSOProviderEnableDisable(t *testing.T) {
	resetSSOProviders(t)
	if err := saveSSOProviders([]sso.ProviderConfig{
		{Key: "tog", Name: "Tog", Issuer: "https://i", ClientID: "c", Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	_, c := ctxWithJSON("POST", "/x", nil, gin.Params{{Key: "key", Value: "tog"}})
	EnableSSOProvider(c)
	if pc, _ := findSSOProvider("tog"); !pc.Enabled {
		t.Fatal("enable must set Enabled=true")
	}
	_, c = ctxWithJSON("POST", "/x", nil, gin.Params{{Key: "key", Value: "tog"}})
	DisableSSOProvider(c)
	if pc, _ := findSSOProvider("tog"); pc.Enabled {
		t.Fatal("disable must set Enabled=false")
	}
}
