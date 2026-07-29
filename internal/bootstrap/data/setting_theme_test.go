package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestSiteThemeSettingSeeded pins the J4 site-wide theme setting: site_theme
// lands in the STYLE group as a TypeText item, empty by default (meaning "use
// the built-in default theme"), and — critically — is NOT flagged PRIVATE or
// DEPRECATED, since it must reach the anonymous /api/public/settings response
// (GetPublicSettingItems only serves PUBLIC/READONLY-flagged rows).
func TestSiteThemeSettingSeeded(t *testing.T) {
	item := findSeed(t, conf.SiteTheme)
	if item.Group != model.STYLE {
		t.Errorf("%s: Group = %d, want model.STYLE (%d)", conf.SiteTheme, item.Group, model.STYLE)
	}
	if item.Type != conf.TypeText {
		t.Errorf("%s: Type = %q, want %q", conf.SiteTheme, item.Type, conf.TypeText)
	}
	if item.Value != "" {
		t.Errorf("%s: default Value = %q, want empty (use built-in default)", conf.SiteTheme, item.Value)
	}
	if item.Flag == model.PRIVATE || item.Flag == model.DEPRECATED {
		t.Errorf("%s: Flag = %d, must be PUBLIC (or READONLY) so it reaches /api/public/settings", conf.SiteTheme, item.Flag)
	}
}
