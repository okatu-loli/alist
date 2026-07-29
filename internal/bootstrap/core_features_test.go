package bootstrap

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/authn/passkey"
	"github.com/alist-org/alist/v3/internal/authn/totp"
	"github.com/alist-org/alist/v3/internal/extension"
)

// TestInitCoreFeatureExtensionsRegistersWebdav pins that internal WebDAV
// (design W5) joins the other natively-wired core features as a manageable,
// enabled-by-default builtin.* extension -- the same Project A-2 treatment
// offline download/search/share/SSO already get, so it lists in the
// extension manager and backs middlewares.RequireExtensionEnabled.
func TestInitCoreFeatureExtensionsRegistersWebdav(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
	})

	InitCoreFeatureExtensions()

	e, ok := extension.DefaultRegistry.Get("builtin.webdav")
	if !ok {
		t.Fatal("builtin.webdav must be registered by InitCoreFeatureExtensions")
	}
	if !e.Enabled() {
		t.Fatal("builtin.webdav must be enabled by default")
	}
	if !e.HasKind(ext.KindFeature) {
		t.Fatal("builtin.webdav must carry ext.KindFeature")
	}

	// The other four core features must still be present too -- this test
	// registering builtin.webdav must not come at the expense of the rest of
	// the batch (a partial registration is fatal in InitCoreFeatureExtensions
	// itself, but pin the successful-batch shape here too).
	for _, id := range ids {
		if _, ok := extension.DefaultRegistry.Get(id); !ok {
			t.Fatalf("InitCoreFeatureExtensions must register %q", id)
		}
	}
}

// ssoSettingsPage returns builtin.sso's contributed native settings page from
// the aggregate contributions document, or false if the extension (or the page)
// is absent.
func ssoSettingsPage(doc extension.ContributionsDoc) (ext.SettingsPage, bool) {
	for _, e := range doc.Extensions {
		if e.ID != "builtin.sso" {
			continue
		}
		for _, p := range e.Contributions.SettingsPages {
			if p.ID == "sso" {
				return p, true
			}
		}
	}
	return ext.SettingsPage{}, false
}

// TestSSOContributesNativeSettingsPage pins the W1 contract: the built-in SSO
// feature declares a settings_pages entry pointing at its native host management
// page (/@manage/...), so the merged contributions document surfaces it while
// the feature is ENABLED and drops the whole extension when it is DISABLED —
// letting the frontend generate/hide the nav entry from data alone.
func TestSSOContributesNativeSettingsPage(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
	})

	InitCoreFeatureExtensions()

	// Manifest carries the contribution directly.
	e, ok := extension.DefaultRegistry.Get("builtin.sso")
	if !ok {
		t.Fatal("builtin.sso must be registered")
	}
	if len(e.Manifest.Contributions.SettingsPages) != 1 {
		t.Fatalf("builtin.sso must declare exactly one settings page, got %+v", e.Manifest.Contributions.SettingsPages)
	}

	// Enabled → the aggregate document surfaces the native page.
	p, ok := ssoSettingsPage(extension.AggregateContributions(extension.DefaultRegistry))
	if !ok {
		t.Fatal("enabled builtin.sso must surface its settings page in AggregateContributions")
	}
	if p.Route != "/@manage/extensions/sso" {
		t.Fatalf("sso settings page route = %q, want /@manage/extensions/sso", p.Route)
	}
	if p.Title["en"] != "SSO Login" || p.Title["zh"] != "单点登录" {
		t.Fatalf("sso settings page title = %+v, want bilingual SSO Login/单点登录", p.Title)
	}

	// Disabled → the extension (and thus its settings page) drops out entirely.
	if err := extension.DefaultRegistry.Disable("builtin.sso"); err != nil {
		t.Fatalf("Disable builtin.sso: %v", err)
	}
	if _, ok := ssoSettingsPage(extension.AggregateContributions(extension.DefaultRegistry)); ok {
		t.Fatal("disabled builtin.sso must not surface its settings page")
	}
}

// settingsPageOf returns extension extID's contributed settings page with the
// given page id from the aggregate contributions document, or false if absent.
func settingsPageOf(doc extension.ContributionsDoc, extID, pageID string) (ext.SettingsPage, bool) {
	for _, e := range doc.Extensions {
		if e.ID != extID {
			continue
		}
		for _, p := range e.Contributions.SettingsPages {
			if p.ID == pageID {
				return p, true
			}
		}
	}
	return ext.SettingsPage{}, false
}

// TestCoreFeatureNativeSettingsPages pins the W6 contract: the built-in search
// and offline_download features each declare a native (/@manage/) settings page
// so the merged contributions document links the extension manager to their real,
// code-read config groups (INDEX / OFFLINE_DOWNLOAD). Disabling a feature drops
// its page from the document.
func TestCoreFeatureNativeSettingsPages(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
	})

	InitCoreFeatureExtensions()

	cases := []struct {
		extID, pageID, route string
		titleEN, titleZH     string
	}{
		{"builtin.search", "search", "/@manage/feature/builtin.search", "File Search", "文件搜索"},
		{"builtin.offline_download", "offline_download", "/@manage/feature/builtin.offline_download", "Offline Download", "离线下载"},
	}
	doc := extension.AggregateContributions(extension.DefaultRegistry)
	for _, tc := range cases {
		p, ok := settingsPageOf(doc, tc.extID, tc.pageID)
		if !ok {
			t.Fatalf("enabled %s must surface settings page %q in AggregateContributions", tc.extID, tc.pageID)
		}
		if p.Route != tc.route {
			t.Errorf("%s settings page route = %q, want %q", tc.extID, p.Route, tc.route)
		}
		if p.Title["en"] != tc.titleEN || p.Title["zh"] != tc.titleZH {
			t.Errorf("%s settings page title = %+v, want %s/%s", tc.extID, p.Title, tc.titleEN, tc.titleZH)
		}
	}

	// Disabled → the search page drops out of the merged document.
	if err := extension.DefaultRegistry.Disable("builtin.search"); err != nil {
		t.Fatalf("Disable builtin.search: %v", err)
	}
	if _, ok := settingsPageOf(extension.AggregateContributions(extension.DefaultRegistry), "builtin.search", "search"); ok {
		t.Fatal("disabled builtin.search must not surface its settings page")
	}
}

// TestCoreFeatureDisableWarnings pins the declarative disable-consequence copy:
// the two features whose disable has real user-visible fallout (public share
// links stop working; SSO login stops working) carry a bilingual DisableWarning,
// while the low-consequence features (offline download/search/webdav) declare
// none -- this manifest field replaces the frontend's hardcoded
// CONFIRM_ON_DISABLE_IDS set.
func TestCoreFeatureDisableWarnings(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
	})

	InitCoreFeatureExtensions()

	// Features that genuinely warrant a confirm carry en+zh copy.
	for _, id := range []string{"builtin.share", "builtin.sso"} {
		e, ok := extension.DefaultRegistry.Get(id)
		if !ok {
			t.Fatalf("%q must be registered", id)
		}
		w := e.Manifest.DisableWarning
		if w["en"] == "" || w["zh"] == "" {
			t.Fatalf("%q must declare a bilingual DisableWarning, got %+v", id, w)
		}
	}

	// Low-consequence features must not declare a warning.
	for _, id := range []string{"builtin.offline_download", "builtin.search", "builtin.webdav"} {
		e, ok := extension.DefaultRegistry.Get(id)
		if !ok {
			t.Fatalf("%q must be registered", id)
		}
		if len(e.Manifest.DisableWarning) != 0 {
			t.Fatalf("%q must not declare a DisableWarning, got %+v", id, e.Manifest.DisableWarning)
		}
	}
}

// passkeyInDoc reports whether builtin.passkey appears in the aggregate
// contributions document (membership == "enabled" for the frontend's gate).
func passkeyInDoc(doc extension.ContributionsDoc) bool {
	for _, e := range doc.Extensions {
		if e.ID == "builtin.passkey" {
			return true
		}
	}
	return false
}

// TestPasskeyManageableBuiltinNoCollision is the W5 CRITICAL check: registering a
// manageable Kind=Feature entry under "builtin.passkey" (so an admin can disable
// passkey) must coexist with the SEPARATE trusted AuthNMethod instance registered
// under the same id. They live in two independent maps — the Feature entry in the
// registry's byID (read by Get), the login method in authNInstances (read by
// GetAuthNMethod) — so neither registration overwrites the other. This test pins
// that BOTH resolve after both registrations, and that disabling the Feature
// entry flips Get(...).Enabled() to false WITHOUT unregistering the method (the
// method still resolves; the login/self-manage gates are what block it).
func TestPasskeyManageableBuiltinNoCollision(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
		extension.RegisterTrustedAuthNMethodInstance(passkey.BuiltinMethodID, nil)
	})

	// Order mirrors boot: the trusted AuthNMethod instance (InitPasskey) then the
	// manageable Feature entry (InitCoreFeatureExtensions).
	passkey.RegisterBuiltin()
	InitCoreFeatureExtensions()

	// The Feature entry resolves as an enabled, KindFeature registry entry.
	e, ok := extension.DefaultRegistry.Get("builtin.passkey")
	if !ok {
		t.Fatal("builtin.passkey must register a manageable Feature entry")
	}
	if !e.Enabled() {
		t.Fatal("builtin.passkey Feature entry must be enabled by default")
	}
	if !e.HasKind(ext.KindFeature) {
		t.Fatal("builtin.passkey Feature entry must carry ext.KindFeature")
	}
	if w := e.Manifest.DisableWarning; w["en"] == "" || w["zh"] == "" {
		t.Fatalf("builtin.passkey must declare a bilingual DisableWarning, got %+v", w)
	}

	// The trusted AuthNMethod instance STILL resolves (independent map, no
	// collision) — registering the Feature entry did not clobber it.
	if m, _, err := extension.DefaultRegistry.GetAuthNMethod("builtin.passkey"); err != nil || m == nil {
		t.Fatalf("trusted passkey AuthNMethod must still resolve after Feature registration: m=%v err=%v", m, err)
	}
	if !extension.DefaultRegistry.AuthNMethodTrusted("builtin.passkey") {
		t.Fatal("passkey AuthNMethod must remain host-trusted (admin-login carve-out) after Feature registration")
	}

	// Enabled → surfaces in the merged contributions document (frontend membership
	// gate for the account PasskeyCard).
	if !passkeyInDoc(extension.AggregateContributions(extension.DefaultRegistry)) {
		t.Fatal("enabled builtin.passkey must appear in AggregateContributions")
	}

	// Disabled → Feature entry flips to disabled and drops from the document, but
	// the trusted method instance is untouched (existence is fine; the gate blocks).
	if err := extension.DefaultRegistry.Disable("builtin.passkey"); err != nil {
		t.Fatalf("Disable builtin.passkey: %v", err)
	}
	e2, _ := extension.DefaultRegistry.Get("builtin.passkey")
	if e2.Enabled() {
		t.Fatal("disabled builtin.passkey Feature entry must report Enabled()==false")
	}
	if passkeyInDoc(extension.AggregateContributions(extension.DefaultRegistry)) {
		t.Fatal("disabled builtin.passkey must drop out of AggregateContributions")
	}
	if m, _, err := extension.DefaultRegistry.GetAuthNMethod("builtin.passkey"); err != nil || m == nil {
		t.Fatalf("disabling the Feature entry must NOT unregister the trusted method: m=%v err=%v", m, err)
	}
}

// totpInDoc reports whether builtin.totp appears in the aggregate
// contributions document.
func totpInDoc(doc extension.ContributionsDoc) bool {
	for _, e := range doc.Extensions {
		if e.ID == "builtin.totp" {
			return true
		}
	}
	return false
}

// loginMethodOf returns extID's login_methods[] entry with the given id from
// the aggregate contributions document, or false if absent.
func loginMethodOf(doc extension.ContributionsDoc, extID, methodID string) (ext.LoginMethod, bool) {
	for _, e := range doc.Extensions {
		if e.ID != extID {
			continue
		}
		for _, lm := range e.Contributions.LoginMethods {
			if lm.ID == methodID {
				return lm, true
			}
		}
	}
	return ext.LoginMethod{}, false
}

// TestPasskeyDeclaresLoginMethod pins the Phase F i18n/login_methods contract:
// builtin.passkey's Feature manifest (the one AggregateContributions/
// PublicLoginMethods actually read Contributions from — see the manifest's own
// doc comment in core_features.go) declares itself in login_methods so the
// login page's aggregate discovery includes passkey, satisfying the
// single-id addressing contract (login_methods[].id == manifest.ID, and the
// manifest also declares Kind=AuthNMethod alongside Kind=Feature —
// internal/extension/manifest.go's ValidateManifest would otherwise reject
// this shape, even though RegisterBuiltinExtension itself does not invoke
// ValidateManifest for built-ins).
func TestPasskeyDeclaresLoginMethod(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
	})

	InitCoreFeatureExtensions()

	e, ok := extension.DefaultRegistry.Get("builtin.passkey")
	if !ok {
		t.Fatal("builtin.passkey must be registered")
	}
	if !e.DeclaresKind(ext.KindAuthNMethod) {
		t.Fatal("builtin.passkey must declare Kind=AuthNMethod alongside Kind=Feature (single-id addressing contract)")
	}

	lm, ok := loginMethodOf(extension.AggregateContributions(extension.DefaultRegistry), "builtin.passkey", "builtin.passkey")
	if !ok {
		t.Fatal("builtin.passkey must surface a login_methods entry in AggregateContributions")
	}
	if lm.Kind != "primary" {
		t.Fatalf("builtin.passkey login_methods[].kind = %q, want \"primary\"", lm.Kind)
	}
	if lm.Label["en"] == "" || lm.Label["zh"] == "" {
		t.Fatalf("builtin.passkey login_methods[].label must be bilingual, got %+v", lm.Label)
	}

	// Disabled → drops out of the aggregate document entirely, alongside the
	// rest of its Contributions (mirrors TestPasskeyManageableBuiltinNoCollision).
	if err := extension.DefaultRegistry.Disable("builtin.passkey"); err != nil {
		t.Fatalf("Disable builtin.passkey: %v", err)
	}
	if _, ok := loginMethodOf(extension.AggregateContributions(extension.DefaultRegistry), "builtin.passkey", "builtin.passkey"); ok {
		t.Fatal("disabled builtin.passkey must not surface a login_methods entry")
	}
}

// TestTOTPManageableBuiltinNoCollision is builtin.passkey's sibling check for
// builtin.totp (Phase C: internal TOTP second factor): registering a
// manageable Kind=Feature entry under "builtin.totp" must coexist with the
// SEPARATE trusted AuthNMethod instance registered under the same id, and
// disabling the Feature entry must flip Get(...).Enabled() to false WITHOUT
// unregistering the method — the login-orchestrator step-up gate
// (server/handles/auth.go) is what actually blocks it.
func TestTOTPManageableBuiltinNoCollision(t *testing.T) {
	ids := []string{
		"builtin.offline_download", "builtin.search", "builtin.share",
		"builtin.sso", "builtin.webdav", "builtin.passkey", "builtin.totp",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			extension.DefaultRegistry.Deregister(id)
		}
		extension.RegisterTrustedAuthNMethodInstance(totp.BuiltinMethodID, nil)
	})

	// Order mirrors boot: the trusted AuthNMethod instance (InitTOTP) then the
	// manageable Feature entry (InitCoreFeatureExtensions).
	totp.RegisterBuiltin()
	InitCoreFeatureExtensions()

	e, ok := extension.DefaultRegistry.Get("builtin.totp")
	if !ok {
		t.Fatal("builtin.totp must register a manageable Feature entry")
	}
	if !e.Enabled() {
		t.Fatal("builtin.totp Feature entry must be enabled by default")
	}
	if !e.HasKind(ext.KindFeature) {
		t.Fatal("builtin.totp Feature entry must carry ext.KindFeature")
	}
	if w := e.Manifest.DisableWarning; w["en"] == "" || w["zh"] == "" {
		t.Fatalf("builtin.totp must declare a bilingual DisableWarning, got %+v", w)
	}

	if m, _, err := extension.DefaultRegistry.GetAuthNMethod("builtin.totp"); err != nil || m == nil {
		t.Fatalf("trusted totp AuthNMethod must still resolve after Feature registration: m=%v err=%v", m, err)
	}
	if !extension.DefaultRegistry.AuthNMethodTrusted("builtin.totp") {
		t.Fatal("totp AuthNMethod must remain host-trusted after Feature registration")
	}

	if !totpInDoc(extension.AggregateContributions(extension.DefaultRegistry)) {
		t.Fatal("enabled builtin.totp must appear in AggregateContributions")
	}

	if err := extension.DefaultRegistry.Disable("builtin.totp"); err != nil {
		t.Fatalf("Disable builtin.totp: %v", err)
	}
	e2, _ := extension.DefaultRegistry.Get("builtin.totp")
	if e2.Enabled() {
		t.Fatal("disabled builtin.totp Feature entry must report Enabled()==false")
	}
	if totpInDoc(extension.AggregateContributions(extension.DefaultRegistry)) {
		t.Fatal("disabled builtin.totp must drop out of AggregateContributions")
	}
	if m, _, err := extension.DefaultRegistry.GetAuthNMethod("builtin.totp"); err != nil || m == nil {
		t.Fatalf("disabling the Feature entry must NOT unregister the trusted method: m=%v err=%v", m, err)
	}
}
