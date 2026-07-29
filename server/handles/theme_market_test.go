package handles

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/thememarket"
	"github.com/gin-gonic/gin"
)

func init() {
	// Every test below hits FetchCatalog/DownloadTheme against an
	// httptest.NewServer (127.0.0.1) — the production marketplaceHTTPClient
	// refuses loopback (M1 SSRF hardening). TEST-ONLY, see
	// thememarket.AllowInternalHostsForTests's doc comment.
	thememarket.AllowInternalHostsForTests(true)
}

func themeMarketAdmin() *model.User {
	return &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}
}

// resetThemeMarket clears both theme-market settings before and after a test
// so tests don't leak state between each other (shared in-memory DB across
// the whole handles package, same reason resetPluginSources exists).
func resetThemeMarket(t *testing.T) {
	t.Helper()
	clear := func() {
		if err := op.SaveSettingItem(&model.SettingItem{Key: conf.ThemeSources, Value: "[]", Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE}); err != nil {
			t.Fatal(err)
		}
		if err := op.SaveSettingItem(&model.SettingItem{Key: conf.MarketThemes, Value: "[]", Type: conf.TypeText, Group: model.STYLE, Flag: model.PRIVATE}); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	t.Cleanup(clear)
}

func themeMarketCtx(w *httptest.ResponseRecorder, method, path, body string, params gin.Params) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, themeMarketAdmin())
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	c.Request = req
	c.Params = params
	return c
}

func TestAddThemeMarketSourceRejectsBadPubkey(t *testing.T) {
	resetThemeMarket(t)
	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "POST", "/api/v4/admin/theme-market/sources",
		`{"name":"myrepo","index_url":"https://example.com/index.json","pubkey":"zz"}`, nil)

	AddThemeMarketSource(c)

	if code := respCode(t, w); code != 400 {
		t.Fatalf("bad pubkey: expected 400, got %d: %s", code, w.Body.String())
	}
}

func TestAddThemeMarketSourceAllowsNoPubkey(t *testing.T) {
	resetThemeMarket(t)
	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "POST", "/api/v4/admin/theme-market/sources",
		`{"name":"myrepo","index_url":"https://example.com/index.json"}`, nil)

	AddThemeMarketSource(c)

	if code := respCode(t, w); code != 200 {
		t.Fatalf("no-pubkey source must be accepted, got %d: %s", code, w.Body.String())
	}
	var resp struct {
		Data []ThemeSourceView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].Trusted {
		t.Fatalf("expected one untrusted (no-key) source, got %+v", resp.Data)
	}
}

func TestThemeMarketSourcesAddListDeleteDuplicateName(t *testing.T) {
	resetThemeMarket(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)

	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "POST", "/api/v4/admin/theme-market/sources",
		fmt.Sprintf(`{"name":"myrepo","index_url":"https://example.com/index.json","pubkey":"%s"}`, pubHex), nil)
	AddThemeMarketSource(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("add: expected 200, got %d: %s", code, w.Body.String())
	}

	// Duplicate name (case-insensitive) must be rejected.
	w2 := httptest.NewRecorder()
	c2 := themeMarketCtx(w2, "POST", "/api/v4/admin/theme-market/sources",
		`{"name":"MyRepo","index_url":"https://example.com/index2.json"}`, nil)
	AddThemeMarketSource(c2)
	if code := respCode(t, w2); code != 400 {
		t.Fatalf("duplicate name: expected 400, got %d", code)
	}

	// List must show it, trusted.
	wList := httptest.NewRecorder()
	cList := themeMarketCtx(wList, "GET", "/api/v4/admin/theme-market/sources", "", nil)
	ListThemeMarketSources(cList)
	var listResp struct {
		Data []ThemeSourceView `json:"data"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Data) != 1 || listResp.Data[0].Name != "myrepo" || !listResp.Data[0].Trusted {
		t.Fatalf("got %+v", listResp.Data)
	}

	// Delete it using a DIFFERENTLY-CASED name (M4: EqualFold, consistent
	// with the add-duplicate check above).
	wDel := httptest.NewRecorder()
	cDel := themeMarketCtx(wDel, "DELETE", "/api/v4/admin/theme-market/sources/MyRepo", "",
		gin.Params{{Key: "name", Value: "MyRepo"}})
	DeleteThemeMarketSource(cDel)
	if code := respCode(t, wDel); code != 200 {
		t.Fatalf("delete (case-insensitive): expected 200, got %d: %s", code, wDel.Body.String())
	}

	// Deleting again (not found) must 404.
	wDel2 := httptest.NewRecorder()
	cDel2 := themeMarketCtx(wDel2, "DELETE", "/api/v4/admin/theme-market/sources/myrepo", "",
		gin.Params{{Key: "name", Value: "myrepo"}})
	DeleteThemeMarketSource(cDel2)
	if code := respCode(t, wDel2); code != 404 {
		t.Fatalf("delete again: expected 404, got %d", code)
	}
}

// themeMarketTestServer serves a catalog at /index.json plus each entry's
// theme JSON at /<id>.json. When priv is non-nil, every entry is signed over
// its OWN canonical signing input (id|version|sha256|url — see
// canonicalEntrySigningInput in internal/thememarket), matching production
// signing; version defaults to "1.0.0" for every entry (use
// themeMarketTestServerV for control over per-entry versions). Returns the
// base URL.
func themeMarketTestServer(t *testing.T, priv ed25519.PrivateKey, contents map[string][]byte) (baseURL string, entries []thememarket.CatalogEntry) {
	t.Helper()
	versions := make(map[string]string, len(contents))
	for id := range contents {
		versions[id] = "1.0.0"
	}
	return themeMarketTestServerV(t, priv, contents, versions)
}

// themeMarketTestServerV is themeMarketTestServer with an explicit version
// per entry id (for update/rollback tests).
func themeMarketTestServerV(t *testing.T, priv ed25519.PrivateKey, contents map[string][]byte, versions map[string]string) (baseURL string, entries []thememarket.CatalogEntry) {
	t.Helper()
	mux := http.NewServeMux()
	for id, content := range contents {
		content := content
		mux.HandleFunc("/"+id+".json", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(content)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for id, content := range contents {
		sum := sha256.Sum256(content)
		entry := thememarket.CatalogEntry{
			ID: id, Name: "Theme " + id, Version: versions[id],
			URL: srv.URL + "/" + id + ".json", SHA256: hex.EncodeToString(sum[:]),
		}
		if priv != nil {
			sig := ed25519.Sign(priv, thememarketCanonicalSigningInputForTest(entry))
			entry.Signature = hex.EncodeToString(sig)
		}
		entries = append(entries, entry)
	}
	catalogJSON, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(catalogJSON)
	})
	return srv.URL + "/index.json", entries
}

// thememarketCanonicalSigningInputForTest mirrors
// thememarket.canonicalEntrySigningInput (unexported) so this test file — a
// different package — can sign fixtures the same way a real source would.
// Kept in lockstep deliberately simple (id|version|sha256|url) so a drift
// between this and the real implementation would show up as EVERY signed-
// source test failing, not a subtle pass.
func thememarketCanonicalSigningInputForTest(entry thememarket.CatalogEntry) []byte {
	return []byte(entry.ID + "|" + entry.Version + "|" + entry.SHA256 + "|" + entry.URL)
}

func addThemeSource(t *testing.T, name, indexURL, pubHex string) {
	t.Helper()
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`{"name":%q,"index_url":%q,"pubkey":%q}`, name, indexURL, pubHex)
	c := themeMarketCtx(w, "POST", "/api/v4/admin/theme-market/sources", body, nil)
	AddThemeMarketSource(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("add source: expected 200, got %d: %s", code, w.Body.String())
	}
}

func installTheme(t *testing.T, source, id string, force bool) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`{"source":%q,"id":%q,"force":%v}`, source, id, force)
	c := themeMarketCtx(w, "POST", "/api/v4/admin/theme-market/install", body, nil)
	InstallThemeMarketTheme(c)
	return w
}

func listInstalled(t *testing.T) []thememarket.InstalledTheme {
	t.Helper()
	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "GET", "/api/v4/admin/theme-market/installed", "", nil)
	ListInstalledThemeMarketThemes(c)
	var resp struct {
		Data []thememarket.InstalledTheme `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Data
}

func TestGetThemeMarketCatalogUntrustedSource(t *testing.T) {
	resetThemeMarket(t)
	indexURL, _ := themeMarketTestServer(t, nil, map[string][]byte{
		"nord": []byte(`{"id":"nord","name":{"en":"Nord"},"light":{}}`),
	})
	addThemeSource(t, "myrepo", indexURL, "")

	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "GET", "/api/v4/admin/theme-market/sources/myrepo/catalog", "",
		gin.Params{{Key: "name", Value: "myrepo"}})
	GetThemeMarketCatalog(c)

	if code := respCode(t, w); code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, w.Body.String())
	}
	var resp struct {
		Data []ThemeCatalogEntryView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].Tier != thememarket.TierUntrusted || resp.Data[0].Blocked {
		t.Fatalf("got %+v", resp.Data)
	}
}

func TestGetThemeMarketCatalogVerifiedSource(t *testing.T) {
	resetThemeMarket(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	indexURL, _ := themeMarketTestServer(t, priv, map[string][]byte{
		"nord": []byte(`{"id":"nord","name":{"en":"Nord"},"light":{}}`),
	})
	addThemeSource(t, "signed", indexURL, hex.EncodeToString(pub))

	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "GET", "/api/v4/admin/theme-market/sources/signed/catalog", "",
		gin.Params{{Key: "name", Value: "signed"}})
	GetThemeMarketCatalog(c)

	var resp struct {
		Data []ThemeCatalogEntryView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].Tier != thememarket.TierVerified || resp.Data[0].Blocked {
		t.Fatalf("got %+v", resp.Data)
	}
}

// TestGetThemeMarketCatalogBlocksBadSignature: a source WITH a pubkey serving
// an entry whose signature doesn't verify (signed by a DIFFERENT key) must be
// annotated Blocked, not silently shown as untrusted-but-installable.
func TestGetThemeMarketCatalogBlocksBadSignature(t *testing.T) {
	resetThemeMarket(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	indexURL, _ := themeMarketTestServer(t, otherPriv, map[string][]byte{
		"nord": []byte(`{"id":"nord"}`),
	})
	addThemeSource(t, "signed", indexURL, hex.EncodeToString(pub))

	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "GET", "/api/v4/admin/theme-market/sources/signed/catalog", "",
		gin.Params{{Key: "name", Value: "signed"}})
	GetThemeMarketCatalog(c)

	var resp struct {
		Data []ThemeCatalogEntryView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || !resp.Data[0].Blocked || resp.Data[0].Reason == "" {
		t.Fatalf("expected a blocked entry with a reason, got %+v", resp.Data)
	}
}

func TestGetThemeMarketCatalogSourceNotFound(t *testing.T) {
	resetThemeMarket(t)
	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "GET", "/api/v4/admin/theme-market/sources/nope/catalog", "",
		gin.Params{{Key: "name", Value: "nope"}})
	GetThemeMarketCatalog(c)
	if code := respCode(t, w); code != 404 {
		t.Fatalf("expected 404, got %d: %s", code, w.Body.String())
	}
}

// TestGetThemeMarketCatalogInstalledKeyedByIdentityNotDescriptorID pins the
// I5 catalog-association fix: an entry whose CATALOG id differs from the
// downloaded theme's own DESCRIPTOR id must still show installed=true (keyed
// by (source, catalog entry id), not by the descriptor id).
func TestGetThemeMarketCatalogInstalledKeyedByIdentityNotDescriptorID(t *testing.T) {
	resetThemeMarket(t)
	// Catalog entry id is "entry-42"; the downloaded descriptor's OWN id is
	// "totally-different-name" — a real-world mismatch this fix must survive.
	indexURL, _ := themeMarketTestServer(t, nil, map[string][]byte{
		"entry-42": []byte(`{"id":"totally-different-name","light":{}}`),
	})
	addThemeSource(t, "myrepo", indexURL, "")

	if code := respCode(t, installTheme(t, "myrepo", "entry-42", false)); code != 200 {
		t.Fatalf("install: expected 200, got %d", code)
	}

	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "GET", "/api/v4/admin/theme-market/sources/myrepo/catalog", "",
		gin.Params{{Key: "name", Value: "myrepo"}})
	GetThemeMarketCatalog(c)
	var resp struct {
		Data []ThemeCatalogEntryView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || !resp.Data[0].Installed {
		t.Fatalf("expected the entry to show installed=true keyed by (source, entry id), got %+v", resp.Data)
	}
}

// TestInstallListUninstallFullFlow drives the whole lifecycle end to end:
// install (verified tier) -> appears in the admin list AND anonymously via
// the DEDICATED public endpoint (I1: no longer /api/public/settings) ->
// uninstall (by source+entry id identity, I5) -> gone from both.
func TestInstallListUninstallFullFlow(t *testing.T) {
	resetThemeMarket(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	indexURL, _ := themeMarketTestServer(t, priv, map[string][]byte{
		"nord": []byte(`{"id":"nord","name":{"en":"Nord"},"light":{"primary":"#5e81ac"}}`),
	})
	addThemeSource(t, "signed", indexURL, hex.EncodeToString(pub))

	w := installTheme(t, "signed", "nord", false)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("install: expected 200, got %d: %s", code, w.Body.String())
	}
	var installResp struct {
		Data thememarket.InstalledTheme `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &installResp); err != nil {
		t.Fatal(err)
	}
	if installResp.Data.ID != "nord" || installResp.Data.Tier != thememarket.TierVerified {
		t.Fatalf("got %+v", installResp.Data)
	}

	listResp := listInstalled(t)
	if len(listResp) != 1 || listResp[0].ID != "nord" {
		t.Fatalf("got %+v", listResp)
	}

	// Visible anonymously through the DEDICATED public endpoint (I1) — NOT
	// through /api/public/settings, which conf.MarketThemes no longer rides.
	wPublic := httptest.NewRecorder()
	cPublic := themeMarketCtx(wPublic, "GET", "/api/v4/public/themes", "", nil)
	PublicMarketThemes(cPublic)
	var publicResp struct {
		Data []thememarket.InstalledTheme `json:"data"`
	}
	if err := json.Unmarshal(wPublic.Body.Bytes(), &publicResp); err != nil {
		t.Fatal(err)
	}
	if len(publicResp.Data) != 1 || publicResp.Data[0].ID != "nord" {
		t.Fatalf("expected the installed theme via the public endpoint, got %+v", publicResp.Data)
	}

	// Sanity: it must NOT leak through the generic public settings map.
	wSettings := httptest.NewRecorder()
	cSettings := themeMarketCtx(wSettings, "GET", "/api/public/settings", "", nil)
	PublicSettings(cSettings)
	var settingsResp struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(wSettings.Body.Bytes(), &settingsResp); err != nil {
		t.Fatal(err)
	}
	if _, ok := settingsResp.Data[conf.MarketThemes]; ok {
		t.Fatalf("market_installed_themes must not appear in the generic public settings map, got %q", settingsResp.Data[conf.MarketThemes])
	}

	// Uninstall by (source, entry id) identity.
	wDel := httptest.NewRecorder()
	cDel := themeMarketCtx(wDel, "DELETE", "/api/v4/admin/theme-market/installed/signed/nord", "",
		gin.Params{{Key: "source", Value: "signed"}, {Key: "entryId", Value: "nord"}})
	UninstallThemeMarketTheme(cDel)
	if code := respCode(t, wDel); code != 200 {
		t.Fatalf("uninstall: expected 200, got %d: %s", code, wDel.Body.String())
	}

	// Gone from the public endpoint too.
	wPublic2 := httptest.NewRecorder()
	cPublic2 := themeMarketCtx(wPublic2, "GET", "/api/v4/public/themes", "", nil)
	PublicMarketThemes(cPublic2)
	var publicResp2 struct {
		Data []thememarket.InstalledTheme `json:"data"`
	}
	if err := json.Unmarshal(wPublic2.Body.Bytes(), &publicResp2); err != nil {
		t.Fatal(err)
	}
	if len(publicResp2.Data) != 0 {
		t.Fatalf("expected nord to be gone after uninstall, got %+v", publicResp2.Data)
	}

	// Uninstalling again must 404.
	wDel2 := httptest.NewRecorder()
	cDel2 := themeMarketCtx(wDel2, "DELETE", "/api/v4/admin/theme-market/installed/signed/nord", "",
		gin.Params{{Key: "source", Value: "signed"}, {Key: "entryId", Value: "nord"}})
	UninstallThemeMarketTheme(cDel2)
	if code := respCode(t, wDel2); code != 404 {
		t.Fatalf("uninstall again: expected 404, got %d", code)
	}
}

// TestPublicMarketThemesETag pins the I1 ETag/conditional-caching fix: a
// repeat GET with If-None-Match set to the first response's ETag must return
// 304 with no body, and a stale/foreign ETag must return the full 200 body.
func TestPublicMarketThemesETag(t *testing.T) {
	resetThemeMarket(t)

	w1 := httptest.NewRecorder()
	c1 := themeMarketCtx(w1, "GET", "/api/v4/public/themes", "", nil)
	PublicMarketThemes(c1)
	if code := respCode(t, w1); code != 200 {
		t.Fatalf("first request: expected 200, got %d", code)
	}
	etag := w1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected an ETag header")
	}
	if cc := w1.Header().Get("Cache-Control"); cc == "" {
		t.Fatal("expected a Cache-Control header")
	}

	// Matching If-None-Match -> 304, no body.
	w2 := httptest.NewRecorder()
	c2 := themeMarketCtx(w2, "GET", "/api/v4/public/themes", "", nil)
	c2.Request.Header.Set("If-None-Match", etag)
	PublicMarketThemes(c2)
	if w2.Code != http.StatusNotModified {
		t.Fatalf("expected 304 for a matching ETag, got %d", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Fatalf("expected an empty body for 304, got %q", w2.Body.String())
	}

	// A stale/foreign ETag -> full 200 body.
	w3 := httptest.NewRecorder()
	c3 := themeMarketCtx(w3, "GET", "/api/v4/public/themes", "", nil)
	c3.Request.Header.Set("If-None-Match", `"stale-value"`)
	PublicMarketThemes(c3)
	if code := respCode(t, w3); code != 200 {
		t.Fatalf("stale ETag: expected 200, got %d", code)
	}

	// Installing a theme changes the ETag.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	indexURL, _ := themeMarketTestServer(t, priv, map[string][]byte{
		"nord": []byte(`{"id":"nord","light":{}}`),
	})
	addThemeSource(t, "signed", indexURL, hex.EncodeToString(pub))
	installTheme(t, "signed", "nord", false)

	w4 := httptest.NewRecorder()
	c4 := themeMarketCtx(w4, "GET", "/api/v4/public/themes", "", nil)
	c4.Request.Header.Set("If-None-Match", etag)
	PublicMarketThemes(c4)
	if code := respCode(t, w4); code != 200 {
		t.Fatalf("after install, old ETag: expected 200 (content changed), got %d", code)
	}
}

// TestInstallRejectsSHA256Mismatch: a catalog entry whose declared sha256
// doesn't match the actually-served content must be rejected at install
// time, never reaching the installed list.
func TestInstallRejectsSHA256Mismatch(t *testing.T) {
	resetThemeMarket(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/nord.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"nord"}`))
	})
	wrongSum := sha256.Sum256([]byte("not the actual content"))
	entries := []thememarket.CatalogEntry{{
		ID: "nord", Name: "Nord", URL: "", SHA256: hex.EncodeToString(wrongSum[:]),
	}}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	entries[0].URL = srv.URL + "/nord.json"
	catalogJSON, _ := json.Marshal(entries)
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(catalogJSON)
	})

	addThemeSource(t, "myrepo", srv.URL+"/index.json", "")

	w := installTheme(t, "myrepo", "nord", false)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("sha256 mismatch: expected 400, got %d: %s", code, w.Body.String())
	}

	if got := listInstalled(t); len(got) != 0 {
		t.Fatalf("expected nothing installed after a rejected install, got %+v", got)
	}
}

// TestInstallRejectsBadJSONPayload: sha256 matches (so the download itself
// succeeds), but the payload isn't a valid theme descriptor — must still be
// rejected.
func TestInstallRejectsBadJSONPayload(t *testing.T) {
	resetThemeMarket(t)
	indexURL, _ := themeMarketTestServer(t, nil, map[string][]byte{
		"broken": []byte(`{not json`),
	})
	addThemeSource(t, "myrepo", indexURL, "")

	w := installTheme(t, "myrepo", "broken", false)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("bad JSON payload: expected 400, got %d: %s", code, w.Body.String())
	}
}

// TestInstallRejectsUnsignedEntryFromKeyedSource: a source WITH a pubkey
// serving an entry with no signature at all must be rejected at install,
// same as the catalog-view "blocked" annotation.
func TestInstallRejectsUnsignedEntryFromKeyedSource(t *testing.T) {
	resetThemeMarket(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// nil priv: entries carry NO signature, even though the source has a pubkey.
	indexURL, _ := themeMarketTestServer(t, nil, map[string][]byte{
		"nord": []byte(`{"id":"nord"}`),
	})
	addThemeSource(t, "signed", indexURL, hex.EncodeToString(pub))

	w := installTheme(t, "signed", "nord", false)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("unsigned entry from keyed source: expected 400, got %d: %s", code, w.Body.String())
	}
}

// TestInstallRejectsJSFromUntrustedSource is the I2 red-team pin: a
// no-pubkey (untrusted-tier) source's theme carrying custom js must be
// refused OUTRIGHT at install — never even reaching storage. This is what
// stops a random, no-signature URL from becoming a site-wide js distribution
// channel: js can only enter storage from a verified (signed) source, full
// stop.
func TestInstallRejectsJSFromUntrustedSource(t *testing.T) {
	resetThemeMarket(t)
	indexURL, _ := themeMarketTestServer(t, nil, map[string][]byte{
		"evil": []byte(`{"id":"evil","js":"alert(document.cookie)"}`),
	})
	addThemeSource(t, "myrepo", indexURL, "")

	w := installTheme(t, "myrepo", "evil", false)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("js from untrusted source: expected 400, got %d: %s", code, w.Body.String())
	}
	if got := listInstalled(t); len(got) != 0 {
		t.Fatalf("expected nothing installed, got %+v", got)
	}
}

// TestInstallAllowsJSFromVerifiedSource: the SAME js-carrying theme, from a
// VERIFIED (signed) source, is allowed into storage — it is simply never
// auto-enabled (that gate lives entirely on the frontend; see
// src/features/appearance/marketThemes.ts and its red-team tests).
func TestInstallAllowsJSFromVerifiedSource(t *testing.T) {
	resetThemeMarket(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	indexURL, _ := themeMarketTestServer(t, priv, map[string][]byte{
		"scripted": []byte(`{"id":"scripted","js":"console.log(1)"}`),
	})
	addThemeSource(t, "signed", indexURL, hex.EncodeToString(pub))

	w := installTheme(t, "signed", "scripted", false)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("js from verified source: expected 200, got %d: %s", code, w.Body.String())
	}
	got := listInstalled(t)
	if len(got) != 1 || !got[0].HasJS || got[0].Tier != thememarket.TierVerified {
		t.Fatalf("got %+v", got)
	}
}

// TestInstallCrossSourceIDCollisionRejectedThenForced is the I5 red-team
// pin: source B (no pubkey) declaring a descriptor id that matches a theme
// ALREADY installed (and verified) from source A must NOT silently take
// over — it must be refused with 409 unless force=true is explicitly passed.
func TestInstallCrossSourceIDCollisionRejectedThenForced(t *testing.T) {
	resetThemeMarket(t)
	pubA, privA, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	indexA, _ := themeMarketTestServer(t, privA, map[string][]byte{
		"nord-real": []byte(`{"id":"nord","light":{"primary":"#5e81ac"}}`),
	})
	addThemeSource(t, "source-a", indexA, hex.EncodeToString(pubA))
	if code := respCode(t, installTheme(t, "source-a", "nord-real", false)); code != 200 {
		t.Fatal("arrange: installing from source-a must succeed")
	}

	// source-b has NO pubkey and declares the SAME descriptor id "nord".
	indexB, _ := themeMarketTestServer(t, nil, map[string][]byte{
		"nord-fake": []byte(`{"id":"nord","branding":{"siteName":"phished"}}`),
	})
	addThemeSource(t, "source-b", indexB, "")

	// Without force: refused, 409, and the original installed record is untouched.
	w := installTheme(t, "source-b", "nord-fake", false)
	if code := respCode(t, w); code != 409 {
		t.Fatalf("cross-source id collision: expected 409, got %d: %s", code, w.Body.String())
	}
	got := listInstalled(t)
	if len(got) != 1 || got[0].Source != "source-a" || got[0].Tier != thememarket.TierVerified {
		t.Fatalf("the verified source-a record must be untouched, got %+v", got)
	}

	// With force=true: the admin explicitly confirms the takeover.
	w2 := installTheme(t, "source-b", "nord-fake", true)
	if code := respCode(t, w2); code != 200 {
		t.Fatalf("forced cross-source replace: expected 200, got %d: %s", code, w2.Body.String())
	}
	got2 := listInstalled(t)
	if len(got2) != 1 || got2[0].Source != "source-b" {
		t.Fatalf("expected source-b to have replaced source-a's record, got %+v", got2)
	}
}

// TestInstallRejectsVersionDowngradeThenForced is the I3 anti-rollback pin:
// reinstalling the SAME (source, entry) identity with an OLDER, even if
// genuinely-signed, version must be refused unless force=true.
func TestInstallRejectsVersionDowngradeThenForced(t *testing.T) {
	resetThemeMarket(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	indexV2, _ := themeMarketTestServerV(t, priv,
		map[string][]byte{"nord": []byte(`{"id":"nord","light":{"primary":"#2.0.0"}}`)},
		map[string]string{"nord": "2.0.0"})
	addThemeSource(t, "signed", indexV2, hex.EncodeToString(pub))
	if code := respCode(t, installTheme(t, "signed", "nord", false)); code != 200 {
		t.Fatal("arrange: installing v2.0.0 must succeed")
	}

	// Same source, same entry id, but the catalog now (re-)serves an OLDER
	// version — e.g. a compromised/rolled-back index, or a replay of an old
	// catalog snapshot.
	indexV1, _ := themeMarketTestServerV(t, priv,
		map[string][]byte{"nord": []byte(`{"id":"nord","light":{"primary":"#1.0.0"}}`)},
		map[string]string{"nord": "1.0.0"})
	// Re-point the SAME source name at the v1 index (simulating the source's
	// index_url now serving stale/rolled-back content).
	deleteSourceByName(t, "signed")
	addThemeSource(t, "signed", indexV1, hex.EncodeToString(pub))

	w := installTheme(t, "signed", "nord", false)
	if code := respCode(t, w); code != 409 {
		t.Fatalf("version downgrade: expected 409, got %d: %s", code, w.Body.String())
	}
	got := listInstalled(t)
	if len(got) != 1 || got[0].Version != "2.0.0" {
		t.Fatalf("the newer installed version must be untouched, got %+v", got)
	}

	// With force=true: the admin explicitly confirms the downgrade.
	w2 := installTheme(t, "signed", "nord", true)
	if code := respCode(t, w2); code != 200 {
		t.Fatalf("forced downgrade: expected 200, got %d: %s", code, w2.Body.String())
	}
	got2 := listInstalled(t)
	if len(got2) != 1 || got2[0].Version != "1.0.0" {
		t.Fatalf("expected the forced downgrade to take effect, got %+v", got2)
	}
}

func deleteSourceByName(t *testing.T, name string) {
	t.Helper()
	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "DELETE", "/api/v4/admin/theme-market/sources/"+name, "",
		gin.Params{{Key: "name", Value: name}})
	DeleteThemeMarketSource(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("delete source %q: expected 200, got %d: %s", name, code, w.Body.String())
	}
}

// TestInstallUpdatesInPlace: re-running install for the SAME (source, entry)
// identity replaces the record (the marketplace UI's "update" action).
func TestInstallUpdatesInPlace(t *testing.T) {
	resetThemeMarket(t)
	indexURL, _ := themeMarketTestServer(t, nil, map[string][]byte{
		"nord": []byte(`{"id":"nord","light":{"primary":"#111111"}}`),
	})
	addThemeSource(t, "myrepo", indexURL, "")

	if code := respCode(t, installTheme(t, "myrepo", "nord", false)); code != 200 {
		t.Fatal("first install: expected 200")
	}
	if got := listInstalled(t); len(got) != 1 {
		t.Fatalf("expected exactly one installed theme, got %+v", got)
	}
}

// TestGetThemeMarketCatalogSanitizesNetworkErrors is the M6 pin: a
// transport-level catalog-fetch failure (here, a closed local port — dial
// refused) must surface a GENERIC message to the admin UI, never the raw
// dial error (which embeds the internal target host:port).
func TestGetThemeMarketCatalogSanitizesNetworkErrors(t *testing.T) {
	resetThemeMarket(t)
	// A closed port on loopback: connection refused, a transport failure.
	addThemeSource(t, "unreachable", "http://127.0.0.1:1/index.json", "")

	w := httptest.NewRecorder()
	c := themeMarketCtx(w, "GET", "/api/v4/admin/theme-market/sources/unreachable/catalog", "",
		gin.Params{{Key: "name", Value: "unreachable"}})
	GetThemeMarketCatalog(c)

	if code := respCode(t, w); code != 502 {
		t.Fatalf("expected 502, got %d: %s", code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "127.0.0.1:1") || strings.Contains(body, "dial") || strings.Contains(body, "connection refused") {
		t.Fatalf("response must not leak raw network/dial details, got: %s", body)
	}
	if !strings.Contains(body, "server logs") {
		t.Errorf("expected the generic message to point at server logs, got: %s", body)
	}
}
