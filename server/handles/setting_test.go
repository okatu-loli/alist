package handles

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/gin-gonic/gin"
)

// postNoBody builds a POST test context with no request body and no
// Content-Type header — unlike postJSON, which always forces
// "application/json". This exercises gin's default (Form) binding, which
// reads query-string values into the bound struct, matching how
// ResetToken's reveal flag is meant to be settable via ?reveal=true without
// a JSON body (the endpoint historically took no body at all).
func postNoBody(u *model.User, h func(*gin.Context), rawURL string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", rawURL, nil)
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

type tokenActionResp struct {
	Code int    `json:"code"`
	Data string `json:"data"`
}

type getTokenResp struct {
	Code int `json:"code"`
	Data struct {
		Token     string `json:"token"`
		Plaintext bool   `json:"plaintext"`
	} `json:"data"`
}

// TestResetTokenDefaultsToHashed: ResetToken with no reveal flag must store
// the new token as a salted hash (never the plaintext), flip
// conf.TokenPlaintext to false, still hand back the plaintext once in the
// response, and GetToken must withhold it afterwards.
func TestResetTokenDefaultsToHashed(t *testing.T) {
	w := postNoBody(handlerAdmin(), ResetToken, "/api/admin/setting/reset_token")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("ResetToken must succeed, got %d: %s", code, w.Body.String())
	}
	var resp tokenActionResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	plainToken := resp.Data
	if plainToken == "" {
		t.Fatalf("ResetToken must return the plaintext token once, got empty data")
	}

	item, err := op.GetSettingItemByKey(conf.Token)
	if err != nil {
		t.Fatal(err)
	}
	if item.Value == plainToken {
		t.Fatalf("default (hashed) ResetToken must not persist the token in plaintext")
	}
	if !utils.VerifyToken(item.Value, plainToken) {
		t.Fatalf("stored hash must verify against the plaintext token returned in the response")
	}

	plaintextItem, err := op.GetSettingItemByKey(conf.TokenPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if plaintextItem.Value != "false" {
		t.Fatalf("conf.TokenPlaintext must be false after a default (hashed) reset, got %q", plaintextItem.Value)
	}

	wGet := getJSON(handlerAdmin(), GetToken, "/api/admin/setting/token")
	var getResp getTokenResp
	if err := json.Unmarshal(wGet.Body.Bytes(), &getResp); err != nil {
		t.Fatal(err)
	}
	if getResp.Data.Plaintext {
		t.Fatalf("GetToken must report plaintext:false once the token is stored hashed")
	}
	if getResp.Data.Token != "" {
		t.Fatalf("GetToken must withhold the token when stored hashed, got %q", getResp.Data.Token)
	}
}

// TestResetTokenRevealStoresPlaintext: ?reveal=true must persist the new
// token in plaintext and GetToken must then return it.
func TestResetTokenRevealStoresPlaintext(t *testing.T) {
	w := postNoBody(handlerAdmin(), ResetToken, "/api/admin/setting/reset_token?reveal=true")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("ResetToken(reveal=true) must succeed, got %d: %s", code, w.Body.String())
	}
	var resp tokenActionResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	plainToken := resp.Data

	item, err := op.GetSettingItemByKey(conf.Token)
	if err != nil {
		t.Fatal(err)
	}
	if item.Value != plainToken {
		t.Fatalf("reveal=true ResetToken must persist the token in plaintext, got %q want %q", item.Value, plainToken)
	}

	plaintextItem, err := op.GetSettingItemByKey(conf.TokenPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if plaintextItem.Value != "true" {
		t.Fatalf("conf.TokenPlaintext must be true after a reveal=true reset, got %q", plaintextItem.Value)
	}

	wGet := getJSON(handlerAdmin(), GetToken, "/api/admin/setting/token")
	var getResp getTokenResp
	if err := json.Unmarshal(wGet.Body.Bytes(), &getResp); err != nil {
		t.Fatal(err)
	}
	if !getResp.Data.Plaintext || getResp.Data.Token != plainToken {
		t.Fatalf("GetToken must return the plaintext token after a reveal=true reset, got %+v", getResp.Data)
	}
}

// TestSetTokenDefaultsToHashedAndReveal: SetToken shares ResetToken's
// storage helper, so it must exhibit the same hashed-by-default /
// reveal-opts-into-plaintext behavior for a caller-supplied token.
func TestSetTokenDefaultsToHashedAndReveal(t *testing.T) {
	w := postJSON(handlerAdmin(), SetToken, "/api/admin/setting/set_token", `{"token":"set-token-hashed-case"}`)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("SetToken must succeed, got %d: %s", code, w.Body.String())
	}
	item, err := op.GetSettingItemByKey(conf.Token)
	if err != nil {
		t.Fatal(err)
	}
	if item.Value == "set-token-hashed-case" {
		t.Fatalf("default (hashed) SetToken must not persist the token in plaintext")
	}
	if !utils.VerifyToken(item.Value, "set-token-hashed-case") {
		t.Fatalf("stored hash must verify against the set token")
	}
	plaintextItem, err := op.GetSettingItemByKey(conf.TokenPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if plaintextItem.Value != "false" {
		t.Fatalf("conf.TokenPlaintext must be false after a default (hashed) set, got %q", plaintextItem.Value)
	}

	w2 := postJSON(handlerAdmin(), SetToken, "/api/admin/setting/set_token", `{"token":"set-token-reveal-case","reveal":true}`)
	if code := respCode(t, w2); code != 200 {
		t.Fatalf("SetToken(reveal=true) must succeed, got %d: %s", code, w2.Body.String())
	}
	item2, err := op.GetSettingItemByKey(conf.Token)
	if err != nil {
		t.Fatal(err)
	}
	if item2.Value != "set-token-reveal-case" {
		t.Fatalf("reveal=true SetToken must persist the token in plaintext, got %q", item2.Value)
	}
	plaintextItem2, err := op.GetSettingItemByKey(conf.TokenPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if plaintextItem2.Value != "true" {
		t.Fatalf("conf.TokenPlaintext must be true after a reveal=true set, got %q", plaintextItem2.Value)
	}
}

// TestSetTokenRejectsMissingToken: the token field stays required regardless
// of the new reveal flag.
func TestSetTokenRejectsMissingToken(t *testing.T) {
	w := postJSON(handlerAdmin(), SetToken, "/api/admin/setting/set_token", `{"reveal":true}`)
	if code := respCode(t, w); code == 200 {
		t.Fatalf("SetToken without a token must be rejected, got 200: %s", w.Body.String())
	}
}

// registerGRPCOfflineDownloadTool wires a Kind=OfflineDownloadTool ExecGRPC
// entry reporting name via Config(), through
// extension.SetOfflineDownloadToolFactory — the shape ring 9's
// getOfflineDownloadToolOptions must surface in the offline_download_tool
// setting's Options alongside the 4 built-ins. b6FakeTool is defined in
// offline_download_b6_test.go (same package).
func registerGRPCOfflineDownloadTool(t *testing.T, pluginID, name string) {
	t.Helper()
	if err := extension.DefaultRegistry.RegisterPlugin(ext.Manifest{
		ID:    pluginID,
		Name:  map[string]string{"en": name},
		Kinds: []ext.Kind{ext.KindOfflineDownloadTool},
	}, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	extension.SetOfflineDownloadToolFactory(func(e *extension.Entry) (ext.OfflineDownloadTool, func(), error) {
		return &b6FakeTool{name: name}, func() {}, nil
	})
	t.Cleanup(func() {
		extension.DefaultRegistry.Deregister(pluginID)
		extension.SetOfflineDownloadToolFactory(nil)
	})
}

type settingItemResp struct {
	Code int               `json:"code"`
	Data model.SettingItem `json:"data"`
}

type settingItemsResp struct {
	Code int                 `json:"code"`
	Data []model.SettingItem `json:"data"`
}

// seedOfflineDownloadToolSetting upserts the offline_download_tool setting row
// with the static seed Options from internal/bootstrap/data/setting.go, so
// GetSetting/ListSettings have a real row to enrich (mirrors production: the
// row always exists post-bootstrap; tests seed it directly instead of running
// the full bootstrap).
func seedOfflineDownloadToolSetting(t *testing.T) {
	t.Helper()
	item := model.SettingItem{
		Key: conf.OfflineDownloadTool, Value: "SimpleHttp", Type: conf.TypeSelect,
		Options: "SimpleHttp,aria2,qBittorrent,Transmission", Group: model.OFFLINE_DOWNLOAD,
	}
	if err := op.SaveSettingItem(&item); err != nil {
		t.Fatalf("seed offline_download_tool setting: %v", err)
	}
}

// TestGetSettingEnrichesOfflineDownloadToolOptions pins ring 9's single-key
// GetSetting path: the offline_download_tool item's Options must list the 4
// built-ins plus a currently-enabled gRPC tool plugin's self-reported name.
func TestGetSettingEnrichesOfflineDownloadToolOptions(t *testing.T) {
	seedOfflineDownloadToolSetting(t)
	registerGRPCOfflineDownloadTool(t, "com.example.settingstest.single", "singlegrpctool")

	w := getJSON(handlerAdmin(), GetSetting, "/api/admin/setting/get?key=offline_download_tool")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("GetSetting must succeed, got %d: %s", code, w.Body.String())
	}
	var resp settingItemResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := "SimpleHttp,aria2,qBittorrent,Transmission,singlegrpctool"
	if resp.Data.Options != want {
		t.Fatalf("GetSetting(offline_download_tool).Options = %q, want %q", resp.Data.Options, want)
	}
}

// TestGetSettingBatchEnrichesOfflineDownloadToolOptions pins the batch (keys=)
// GetSetting path, requested alongside default_role in the same call — the
// switch-based enrichment must not stop after handling default_role first
// (the old code's `break` would have skipped offline_download_tool if it came
// after default_role in the keys list).
func TestGetSettingBatchEnrichesOfflineDownloadToolOptions(t *testing.T) {
	seedOfflineDownloadToolSetting(t)
	registerGRPCOfflineDownloadTool(t, "com.example.settingstest.batch", "batchgrpctool")
	// default_role must also exist for the batch lookup (op.GetSettingItemInKeys
	// 404s if ANY requested key is missing). An empty value passes the
	// conf.DefaultRole save-hook without needing a real role row (internal/op/
	// hook.go: empty value short-circuits before GetRole), and the handler's own
	// strconv.Atoi("") failure just leaves Value untouched.
	if err := op.SaveSettingItem(&model.SettingItem{
		Key: conf.DefaultRole, Value: "", Type: conf.TypeSelect, Group: model.SITE,
	}); err != nil {
		t.Fatalf("seed default_role setting: %v", err)
	}

	w := getJSON(handlerAdmin(), GetSetting, "/api/admin/setting/get?keys=default_role,offline_download_tool")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("GetSetting batch must succeed, got %d: %s", code, w.Body.String())
	}
	var resp settingItemsResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range resp.Data {
		if item.Key != conf.OfflineDownloadTool {
			continue
		}
		found = true
		if !strings.Contains(item.Options, "batchgrpctool") {
			t.Fatalf("batch GetSetting(offline_download_tool).Options = %q, want it to contain batchgrpctool", item.Options)
		}
	}
	if !found {
		t.Fatal("batch GetSetting must return the offline_download_tool item")
	}
}

// TestListSettingsEnrichesOfflineDownloadToolOptions pins ring 9's
// ListSettings path.
func TestListSettingsEnrichesOfflineDownloadToolOptions(t *testing.T) {
	seedOfflineDownloadToolSetting(t)
	registerGRPCOfflineDownloadTool(t, "com.example.settingstest.list", "listgrpctool")

	w := getJSON(handlerAdmin(), ListSettings, "/api/admin/setting/list")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("ListSettings must succeed, got %d: %s", code, w.Body.String())
	}
	var resp settingItemsResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range resp.Data {
		if item.Key != conf.OfflineDownloadTool {
			continue
		}
		found = true
		if !strings.Contains(item.Options, "listgrpctool") {
			t.Fatalf("ListSettings offline_download_tool Options = %q, want it to contain listgrpctool", item.Options)
		}
	}
	if !found {
		t.Fatal("ListSettings must return the offline_download_tool item")
	}
}
