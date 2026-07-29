package op

import (
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

func TestValidateMarketThemesAcceptsEmpty(t *testing.T) {
	for _, v := range []string{"", "   ", "[]"} {
		if err := validateMarketThemes(v); err != nil {
			t.Errorf("value %q must be valid, got: %v", v, err)
		}
	}
}

func TestValidateMarketThemesAcceptsWellFormedList(t *testing.T) {
	list := `[{"id":"nord","source":"myrepo","entry_id":"nord","tier":"verified","theme":{"id":"nord","light":{}}}]`
	if err := validateMarketThemes(list); err != nil {
		t.Fatalf("well-formed list must be valid, got: %v", err)
	}
}

// TestValidateMarketThemesAllowsJS is the key policy DIFFERENCE from
// validateSiteTheme: a marketplace theme's Theme payload carrying a "js"
// field must be ACCEPTED for storage (it is simply never auto-enabled).
func TestValidateMarketThemesAllowsJS(t *testing.T) {
	list := `[{"id":"evil","source":"myrepo","entry_id":"evil","tier":"untrusted","theme":{"id":"evil","js":"alert(1)"}}]`
	if err := validateMarketThemes(list); err != nil {
		t.Fatalf("a theme payload with js must be accepted for storage, got: %v", err)
	}
}

func TestValidateMarketThemesRejectsBadJSON(t *testing.T) {
	if err := validateMarketThemes(`{not json`); err == nil {
		t.Fatal("bad JSON must be rejected")
	}
}

func TestValidateMarketThemesRejectsMissingID(t *testing.T) {
	list := `[{"source":"myrepo","entry_id":"nord","theme":{"id":"nord"}}]`
	if err := validateMarketThemes(list); err == nil {
		t.Fatal("missing id must be rejected")
	}
}

func TestValidateMarketThemesRejectsMissingThemePayload(t *testing.T) {
	list := `[{"id":"nord","source":"myrepo","entry_id":"nord"}]`
	if err := validateMarketThemes(list); err == nil {
		t.Fatal("missing theme payload must be rejected")
	}
}

func TestValidateMarketThemesRejectsBadThemePayloadJSON(t *testing.T) {
	list := `[{"id":"nord","source":"myrepo","entry_id":"nord","theme":"not an object"}]`
	// "not an object" is valid JSON (a string), but json.Unmarshal into
	// map[string]json.RawMessage must fail for it — assert it is rejected.
	if err := validateMarketThemes(list); err == nil {
		t.Fatal("a non-object theme payload must be rejected")
	}
}

func TestValidateMarketThemesRejectsDuplicateID(t *testing.T) {
	list := `[
		{"id":"nord","source":"a","entry_id":"nord","theme":{"id":"nord"}},
		{"id":"nord","source":"b","entry_id":"nord","theme":{"id":"nord"}}
	]`
	if err := validateMarketThemes(list); err == nil {
		t.Fatal("duplicate id must be rejected")
	}
}

func TestValidateMarketThemesRejectsOversized(t *testing.T) {
	huge := `[{"id":"nord","source":"a","entry_id":"nord","theme":{"id":"nord","css":"` +
		strings.Repeat("a", MarketThemesMaxBytes) + `"}}]`
	if err := validateMarketThemes(huge); err == nil {
		t.Fatal("oversized value must be rejected")
	}
}

func marketThemesItem(value string) *model.SettingItem {
	return &model.SettingItem{Key: conf.MarketThemes, Value: value, Type: conf.TypeText, Group: model.STYLE}
}

func TestSaveSettingItemRejectsBadMarketThemes(t *testing.T) {
	t.Cleanup(func() { _ = SaveSettingItem(marketThemesItem("[]")) })

	const baseline = `[{"id":"baseline","source":"a","entry_id":"baseline","theme":{"id":"baseline"}}]`
	if err := SaveSettingItem(marketThemesItem(baseline)); err != nil {
		t.Fatalf("arrange: saving the baseline value must succeed, got: %v", err)
	}

	if err := SaveSettingItem(marketThemesItem(`{not json`)); err == nil {
		t.Fatal("SaveSettingItem must reject invalid market_installed_themes JSON")
	}

	stored, err := GetSettingItemByKey(conf.MarketThemes)
	if err != nil {
		t.Fatalf("get market_installed_themes after rejected save: %v", err)
	}
	if stored.Value != baseline {
		t.Fatalf("rejected save must not persist: stored = %q, want unchanged baseline %q", stored.Value, baseline)
	}
}

func TestSaveSettingItemAcceptsGoodMarketThemes(t *testing.T) {
	t.Cleanup(func() { _ = SaveSettingItem(marketThemesItem("[]")) })

	good := `[{"id":"nord","source":"myrepo","entry_id":"nord","tier":"verified","theme":{"id":"nord","light":{}}}]`
	if err := SaveSettingItem(marketThemesItem(good)); err != nil {
		t.Fatalf("SaveSettingItem must accept a well-formed list, got: %v", err)
	}
	stored, err := GetSettingItemByKey(conf.MarketThemes)
	if err != nil {
		t.Fatalf("get market_installed_themes after save: %v", err)
	}
	if stored.Value != good {
		t.Fatalf("stored value = %q, want %q", stored.Value, good)
	}
}
