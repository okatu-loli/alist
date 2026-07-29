package op

import (
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestValidateSiteThemeAcceptsEmpty: an empty value ("use the built-in
// default theme") is always valid — it's the seeded default.
func TestValidateSiteThemeAcceptsEmpty(t *testing.T) {
	if err := validateSiteTheme(""); err != nil {
		t.Fatalf("empty value must be valid, got: %v", err)
	}
	if err := validateSiteTheme("   "); err != nil {
		t.Fatalf("whitespace-only value must be valid, got: %v", err)
	}
}

// TestValidateSiteThemeAcceptsWellFormedTheme: an ordinary theme descriptor
// (no js) under the size cap must pass.
func TestValidateSiteThemeAcceptsWellFormedTheme(t *testing.T) {
	theme := `{"id":"nord","name":{"en":"Nord"},"swatch":["#000","#111","#fff"],"light":{"primary":"#5e81ac"}}`
	if err := validateSiteTheme(theme); err != nil {
		t.Fatalf("well-formed theme must be valid, got: %v", err)
	}
}

// TestValidateSiteThemeRejectsBadJSON: unparsable JSON must be rejected with a
// readable error.
func TestValidateSiteThemeRejectsBadJSON(t *testing.T) {
	err := validateSiteTheme(`{not json`)
	if err == nil {
		t.Fatal("bad JSON must be rejected")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("error should mention JSON, got: %v", err)
	}
}

// TestValidateSiteThemeRejectsOversized: a value over SiteThemeMaxBytes must
// be rejected, e.g. a base64-embedded font bloating the always-public
// settings response.
func TestValidateSiteThemeRejectsOversized(t *testing.T) {
	huge := `{"css":"` + strings.Repeat("a", SiteThemeMaxBytes+1) + `"}`
	err := validateSiteTheme(huge)
	if err == nil {
		t.Fatal("oversized value must be rejected")
	}
}

// TestValidateSiteThemeAcceptsAtExactLimit: a value exactly at the cap must
// still pass — only strictly-over is rejected.
func TestValidateSiteThemeAcceptsAtExactLimit(t *testing.T) {
	// Build a JSON value whose total byte length is exactly SiteThemeMaxBytes.
	prefix := `{"css":"`
	suffix := `"}`
	pad := SiteThemeMaxBytes - len(prefix) - len(suffix)
	value := prefix + strings.Repeat("a", pad) + suffix
	if len(value) != SiteThemeMaxBytes {
		t.Fatalf("test setup: len(value) = %d, want %d", len(value), SiteThemeMaxBytes)
	}
	if err := validateSiteTheme(value); err != nil {
		t.Fatalf("value at exactly the limit must be valid, got: %v", err)
	}
}

// TestValidateSiteThemeAcceptsJsField: PRODUCT DECISION (reversed 2026-07,
// see validateSiteTheme's doc comment) — a theme carrying a top-level "js"
// field is now accepted. The admin-confirmation gate lives client-side
// (SiteThemeCard's blocking review dialog), not here.
func TestValidateSiteThemeAcceptsJsField(t *testing.T) {
	theme := `{"id":"evil","name":{"en":"Evil"},"swatch":["#000","#111","#fff"],"light":{"primary":"#000"},"js":"alert(1)"}`
	if err := validateSiteTheme(theme); err != nil {
		t.Fatalf("a theme with a js field must now be accepted, got: %v", err)
	}
}

// TestValidateSiteThemeAcceptsEmptyJsField: an empty-string "js" field is
// likewise accepted — no reason to special-case it now that non-empty js is
// allowed too.
func TestValidateSiteThemeAcceptsEmptyJsField(t *testing.T) {
	theme := `{"id":"x","name":{"en":"X"},"swatch":["#000","#111","#fff"],"light":{"primary":"#000"},"js":""}`
	if err := validateSiteTheme(theme); err != nil {
		t.Fatalf("a theme with an empty js field must be accepted, got: %v", err)
	}
}

// siteThemeItem builds a fresh SettingItem for conf.SiteTheme, matching the
// seeded shape (STYLE group, TypeText, PUBLIC/zero-value Flag).
func siteThemeItem(value string) *model.SettingItem {
	return &model.SettingItem{Key: conf.SiteTheme, Value: value, Type: conf.TypeText, Group: model.STYLE}
}

// TestSaveSettingItemRejectsBadSiteTheme: the hook must fire through the
// normal admin save path (op.SaveSettingItem), so an invalid value never
// reaches the DB. Does not assume a pre-existing row — establishes a known
// baseline itself, since op package tests share one in-memory DB across the
// whole test binary.
func TestSaveSettingItemRejectsBadSiteTheme(t *testing.T) {
	t.Cleanup(func() { _ = SaveSettingItem(siteThemeItem("")) })

	const baseline = `{"id":"baseline","name":{"en":"Baseline"},"swatch":["#000","#111","#fff"],"light":{"primary":"#000"}}`
	if err := SaveSettingItem(siteThemeItem(baseline)); err != nil {
		t.Fatalf("arrange: saving the baseline value must succeed, got: %v", err)
	}

	if err := SaveSettingItem(siteThemeItem(`{not json`)); err == nil {
		t.Fatal("SaveSettingItem must reject invalid site_theme JSON")
	}

	stored, err := GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after rejected save: %v", err)
	}
	if stored.Value != baseline {
		t.Fatalf("rejected save must not persist: stored = %q, want unchanged baseline %q", stored.Value, baseline)
	}
}

// TestSaveSettingItemAcceptsGoodSiteTheme: a well-formed theme (no js) saves
// successfully and round-trips.
func TestSaveSettingItemAcceptsGoodSiteTheme(t *testing.T) {
	t.Cleanup(func() { _ = SaveSettingItem(siteThemeItem("")) })

	good := `{"id":"nord","name":{"en":"Nord"},"swatch":["#000","#111","#fff"],"light":{"primary":"#5e81ac"}}`
	if err := SaveSettingItem(siteThemeItem(good)); err != nil {
		t.Fatalf("SaveSettingItem must accept a well-formed theme, got: %v", err)
	}
	stored, err := GetSettingItemByKey(conf.SiteTheme)
	if err != nil {
		t.Fatalf("get site_theme after save: %v", err)
	}
	if stored.Value != good {
		t.Fatalf("stored value = %q, want %q", stored.Value, good)
	}
}

// TestSiteThemeCarriesJS: the helper server/handles/setting.go's SaveSettings
// (generic bulk endpoint) and site_theme.go's SetSiteTheme (dedicated
// endpoint) both use to decide whether a value needs the js-confirmation
// gate — see conf.SiteTheme's doc comment for the HIGH-severity red-team fix
// this feeds.
func TestSiteThemeCarriesJS(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"empty value", "", false},
		{"whitespace-only value", "   ", false},
		{"no js field at all", `{"id":"x","light":{"primary":"#000"}}`, false},
		{"js field present but empty string", `{"id":"x","js":""}`, false},
		{"js field present but whitespace-only", `{"id":"x","js":"   "}`, false},
		{"js field present and non-blank", `{"id":"x","js":"alert(1)"}`, true},
		{"malformed JSON is tolerated (reported false, not an error)", `{not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SiteThemeCarriesJS(c.value); got != c.want {
				t.Fatalf("SiteThemeCarriesJS(%q) = %v, want %v", c.value, got, c.want)
			}
		})
	}
}
