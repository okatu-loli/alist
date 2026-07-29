package handles

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// siteThemeCleanup resets conf.SiteTheme back to empty (the seeded default)
// after a test mutates it, so other tests in this shared-DB package aren't
// affected by leftover state.
func siteThemeCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = op.SaveSettingItem(&model.SettingItem{Key: conf.SiteTheme, Value: "", Type: conf.TypeText, Group: model.STYLE})
	})
}

// TestPublicSettingsExposesSiteTheme is the positive counterpart of the
// frp_token red-team guard: site_theme is PUBLIC (theming spec §J4) precisely
// so an ANONYMOUS visitor's browser can see the admin-configured site-wide
// default theme, not just a logged-in one. Verifies the whole path: an admin
// save reaches PublicSettings with no auth required.
func TestPublicSettingsExposesSiteTheme(t *testing.T) {
	siteThemeCleanup(t)
	const theme = `{"id":"nord","name":{"en":"Nord"},"swatch":["#000","#111","#fff"],"light":{"primary":"#5e81ac"}}`
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.SiteTheme, Value: theme, Type: conf.TypeText, Group: model.STYLE}); err != nil {
		t.Fatalf("arrange: save site_theme: %v", err)
	}

	// nil user: PublicSettings must require no authentication at all.
	w := getJSON(nil, PublicSettings, "/api/public/settings")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("PublicSettings must succeed anonymously, got %d: %s", code, w.Body.String())
	}
	var resp struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data[conf.SiteTheme] != theme {
		t.Fatalf("public settings site_theme = %q, want %q", resp.Data[conf.SiteTheme], theme)
	}
}

// TestPublicSettingsHasCacheControl: the coordinator's B2 review flagged that
// site_theme (up to op.SiteThemeMaxBytes) noticeably raised this anonymous,
// no-auth, every-page-load endpoint's response weight with neither a
// Cache-Control header nor gzip in front of it. PublicSettings must send a
// short, revalidate-friendly Cache-Control so repeat anonymous loads within
// the window can skip the round trip entirely, without indefinitely delaying
// visibility of an admin's setting change.
func TestPublicSettingsHasCacheControl(t *testing.T) {
	w := getJSON(nil, PublicSettings, "/api/public/settings")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("PublicSettings must succeed, got %d: %s", code, w.Body.String())
	}
	cc := w.Header().Get("Cache-Control")
	if cc == "" {
		t.Fatal("PublicSettings response must carry a Cache-Control header")
	}
	if !strings.Contains(cc, "public") {
		t.Errorf("Cache-Control = %q, want it to include \"public\" (this endpoint requires no auth)", cc)
	}
	if !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want a bounded max-age so setting changes stay visible reasonably soon", cc)
	}
}

// TestSaveSettingsAdminSetsSiteTheme: an admin saving a well-formed theme
// (no js) through the ordinary bulk settings-save path must succeed and
// round-trip through GetSetting.
func TestSaveSettingsAdminSetsSiteTheme(t *testing.T) {
	siteThemeCleanup(t)
	const theme = `{"id":"forest","name":{"en":"Forest"},"swatch":["#0a0","#1a1","#fff"],"light":{"primary":"#0a0"}}`
	body := `[{"key":"site_theme","value":` + jsonString(theme) + `,"type":"text","group":2}]`
	w := postJSON(handlerAdmin(), SaveSettings, "/api/admin/settings", body)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("admin SaveSettings must accept a well-formed site_theme, got %d: %s", code, w.Body.String())
	}

	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after save: %v", err)
	}
	if item.Value != theme {
		t.Fatalf("stored site_theme = %q, want %q", item.Value, theme)
	}
}

// TestSaveSettingsRejectsBadSiteTheme: SaveSettings must surface a readable
// error, respond 400 (client input error — op.IsSettingValidationError, not a
// server fault), and must NOT persist an invalid site_theme (bad JSON here;
// the dedicated op-level tests cover the size cap).
func TestSaveSettingsRejectsBadSiteTheme(t *testing.T) {
	siteThemeCleanup(t)
	const baseline = `{"id":"baseline","name":{"en":"Baseline"},"swatch":["#000","#111","#fff"],"light":{"primary":"#000"}}`
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.SiteTheme, Value: baseline, Type: conf.TypeText, Group: model.STYLE}); err != nil {
		t.Fatalf("arrange: save baseline site_theme: %v", err)
	}

	body := `[{"key":"site_theme","value":` + jsonString(`{not json`) + `,"type":"text","group":2}]`
	w := postJSON(handlerAdmin(), SaveSettings, "/api/admin/settings", body)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("admin SaveSettings must reject invalid site_theme JSON with 400 (client input error), got %d: %s", code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "JSON") {
		t.Errorf("error response should mention JSON, got: %s", w.Body.String())
	}

	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after rejected save: %v", err)
	}
	if item.Value != baseline {
		t.Fatalf("rejected save must not persist: stored = %q, want unchanged baseline %q", item.Value, baseline)
	}
}

// jsonString JSON-encodes s as a quoted string literal, for embedding a raw
// theme-JSON payload as the `value` field of a hand-written SettingItem body.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

const siteThemeWithJS = `{"id":"evil","name":{"en":"Evil"},"swatch":["#000","#111","#fff"],"light":{"primary":"#000"},"js":"alert(document.cookie)"}`

// makeSettingManagerOperator builds a NON-admin operator whose role has
// EXACTLY the setting:view/setting:manage system permission — the exact
// shape this repo's RBAC allows an admin to delegate (RequireSystem("setting",
// "manage") on the generic /admin/settings endpoint), and exactly the role
// the HIGH-severity red-team finding is about: this role can reach the
// generic settings endpoint, but must NEVER be able to use it to run
// arbitrary JS for every visitor.
func makeSettingManagerOperator(t *testing.T) *model.User {
	t.Helper()
	r := createNonReservedRole(t, "h-setting-op-role")
	p := &model.Permission{
		Name:         "h-op-setting",
		ResourceType: model.ResourceSystem,
		Path:         "setting",
		Effect:       model.EffectAllow,
		Actions: model.Actions{
			model.SystemAction("setting", "view"),
			model.SystemAction("setting", "manage"),
		},
	}
	if err := db.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })
	if err := db.AddRolePermission(r.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	op.InvalidateRolePolicyForTest(r.ID)
	return &model.User{ID: 7777, Username: "h-setting-op", Role: model.Roles{int(r.ID)}}
}

// TestSaveSettingsRejectsSiteThemeWithJsEvenForFullAdmin: HIGH-severity
// red-team fix (see conf.SiteTheme's doc comment) — the generic bulk-settings
// endpoint must refuse a js-carrying site_theme value OUTRIGHT and
// UNCONDITIONALLY, regardless of who is calling (even a full admin), with no
// confirmation flag able to unlock it through this path. The confirmed save
// path is the dedicated SetSiteTheme endpoint only (tested below).
func TestSaveSettingsRejectsSiteThemeWithJsEvenForFullAdmin(t *testing.T) {
	siteThemeCleanup(t)
	body := `[{"key":"site_theme","value":` + jsonString(siteThemeWithJS) + `,"type":"text","group":2}]`
	w := postJSON(handlerAdmin(), SaveSettings, "/api/admin/settings", body)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("generic SaveSettings must refuse a js-carrying site_theme with 400, even for a full admin, got %d: %s", code, w.Body.String())
	}

	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after rejected save: %v", err)
	}
	if item.Value != "" {
		t.Fatalf("rejected save must not persist, got %q", item.Value)
	}
}

// TestSaveSettingsRejectsSiteThemeWithJsForSettingManagerRole: the exact
// scenario the red-team finding described — a role scoped to ONLY
// setting:manage (not full admin) hitting the generic endpoint with a
// js-carrying site_theme — must still be refused (the block in
// TestSaveSettingsRejectsSiteThemeWithJsEvenForFullAdmin above is
// value-based, not role-based, so it already covers this role too; this test
// pins that down explicitly against the precise role shape the finding named).
func TestSaveSettingsRejectsSiteThemeWithJsForSettingManagerRole(t *testing.T) {
	siteThemeCleanup(t)
	u := makeSettingManagerOperator(t)
	body := `[{"key":"site_theme","value":` + jsonString(siteThemeWithJS) + `,"type":"text","group":2}]`
	w := postJSON(u, SaveSettings, "/api/admin/settings", body)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("a setting:manage-only role must be refused 400 for a js-carrying site_theme via the generic endpoint, got %d: %s", code, w.Body.String())
	}
	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after rejected save: %v", err)
	}
	if item.Value != "" {
		t.Fatalf("rejected save must not persist, got %q", item.Value)
	}
}

// TestSetSiteThemeRefusesNonAdminEvenWithSettingManage: the dedicated
// endpoint's admin gate — a role scoped to ONLY setting:manage (which WOULD
// satisfy the generic endpoint's RequireSystem("setting","manage")) must
// still be refused 403 by SetSiteTheme's own handler-layer defense-in-depth
// check (callerIsAdmin), independent of the router's RequireAdmin. No save
// must happen.
func TestSetSiteThemeRefusesNonAdminEvenWithSettingManage(t *testing.T) {
	siteThemeCleanup(t)
	u := makeSettingManagerOperator(t)
	body := `{"value":` + jsonString(siteThemeWithJS) + `,"js_confirmed":true}`
	w := postJSON(u, SetSiteTheme, "/api/v4/admin/settings/site-theme", body)
	if code := respCode(t, w); code != 403 {
		t.Fatalf("a setting:manage-only role must be refused 403 by the dedicated endpoint, got %d: %s", code, w.Body.String())
	}
	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after refused call: %v", err)
	}
	if item.Value != "" {
		t.Fatalf("a refused call must not persist anything, got %q", item.Value)
	}
}

// TestSetSiteThemeRequiresJsConfirmedForAdmin: even a full admin must
// explicitly assert js_confirmed:true — a request that carries js but omits
// (or falses) that flag is refused 400, and nothing is persisted. This is
// what makes the confirmation impossible to bypass ACCIDENTALLY (a direct API
// call that just forgets the field), independent of the UI dialog.
func TestSetSiteThemeRequiresJsConfirmedForAdmin(t *testing.T) {
	siteThemeCleanup(t)
	body := `{"value":` + jsonString(siteThemeWithJS) + `}` // js_confirmed omitted
	w := postJSON(handlerAdmin(), SetSiteTheme, "/api/v4/admin/settings/site-theme", body)
	if code := respCode(t, w); code != 400 {
		t.Fatalf("admin without js_confirmed must be refused 400, got %d: %s", code, w.Body.String())
	}
	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after refused call: %v", err)
	}
	if item.Value != "" {
		t.Fatalf("a refused call must not persist anything, got %q", item.Value)
	}

	explicitFalse := `{"value":` + jsonString(siteThemeWithJS) + `,"js_confirmed":false}`
	w2 := postJSON(handlerAdmin(), SetSiteTheme, "/api/v4/admin/settings/site-theme", explicitFalse)
	if code := respCode(t, w2); code != 400 {
		t.Fatalf("admin with js_confirmed:false must still be refused 400, got %d: %s", code, w2.Body.String())
	}
}

// TestSetSiteThemeAdminConfirmedSucceedsAndAudits: the happy path — a full
// admin, js_confirmed:true — must succeed, persist the theme WITH its js
// field intact, and write a success audit event recording the actor.
func TestSetSiteThemeAdminConfirmedSucceedsAndAudits(t *testing.T) {
	siteThemeCleanup(t)
	admin := handlerAdmin()

	audit.Start()
	body := `{"value":` + jsonString(siteThemeWithJS) + `,"js_confirmed":true}`
	w := postJSON(admin, SetSiteTheme, "/api/v4/admin/settings/site-theme", body)
	audit.Stop()

	if code := respCode(t, w); code != 200 {
		t.Fatalf("admin + js_confirmed:true must succeed, got %d: %s", code, w.Body.String())
	}
	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after save: %v", err)
	}
	if item.Value != siteThemeWithJS {
		t.Fatalf("stored site_theme = %q, want %q (js field must round-trip)", item.Value, siteThemeWithJS)
	}

	logs, total, err := db.GetAuditLogs(db.AuditLogFilter{ActorUID: admin.ID, ActionPrefix: "settings.site_theme."}, 1, 10)
	if err != nil {
		t.Fatalf("query audit logs: %v", err)
	}
	if total == 0 {
		t.Fatal("a confirmed js-carrying site_theme save must be audited, found none")
	}
	found := false
	for _, l := range logs {
		if l.Actor == admin.Username && l.Result == "success" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a success audit entry for actor %q, got %+v", admin.Username, logs)
	}
}

// TestSetSiteThemeUnconfirmedAttemptIsAudited: even a REFUSED attempt (admin,
// js present, js_confirmed missing) must leave an audit trail — an operator
// reviewing the log should see the attempt was made and denied, not silence.
func TestSetSiteThemeUnconfirmedAttemptIsAudited(t *testing.T) {
	siteThemeCleanup(t)
	admin := handlerAdmin()

	audit.Start()
	body := `{"value":` + jsonString(siteThemeWithJS) + `}`
	postJSON(admin, SetSiteTheme, "/api/v4/admin/settings/site-theme", body)
	audit.Stop()

	logs, total, err := db.GetAuditLogs(db.AuditLogFilter{ActorUID: admin.ID, ActionPrefix: "settings.site_theme."}, 1, 10)
	if err != nil {
		t.Fatalf("query audit logs: %v", err)
	}
	if total == 0 {
		t.Fatal("a refused (unconfirmed) attempt must still be audited, found none")
	}
	found := false
	for _, l := range logs {
		if l.Result == "failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a failure audit entry, got %+v", logs)
	}
}

// TestSetSiteThemeAcceptsJsFreeThemeWithoutConfirmation: a theme with no js
// (or a blank js field) needs no confirmation at all through the dedicated
// endpoint — matching the generic endpoint's unaffected behavior for a
// js-free site_theme (coordinator's point 3/4: "不带 js 的主题两条路径都正常").
func TestSetSiteThemeAcceptsJsFreeThemeWithoutConfirmation(t *testing.T) {
	siteThemeCleanup(t)
	const noJS = `{"id":"nord","name":{"en":"Nord"},"swatch":["#000","#111","#fff"],"light":{"primary":"#5e81ac"}}`
	body := `{"value":` + jsonString(noJS) + `}`
	w := postJSON(handlerAdmin(), SetSiteTheme, "/api/v4/admin/settings/site-theme", body)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("a js-free theme must be accepted with no confirmation, got %d: %s", code, w.Body.String())
	}
	item, err := op.GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after save: %v", err)
	}
	if item.Value != noJS {
		t.Fatalf("stored site_theme = %q, want %q", item.Value, noJS)
	}
}
