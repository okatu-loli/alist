package handles

import (
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestFrpTokenNotInPublicSettings is the red-team guard for the frps token's
// PRIVATE flag: the unauthenticated /public/settings endpoint must never echo
// frp_token's key or value. PublicSettings reads GetPublicSettingsMap, which is
// backed by db.GetPublicSettingItems filtered to PUBLIC/READONLY flags only —
// so a PRIVATE frp_token is excluded.
//
// LOAD-BEARING: flip conf.FrpToken's registration to model.PUBLIC (or drop the
// flag) in internal/bootstrap/data/setting.go and this test turns red.
func TestFrpTokenNotInPublicSettings(t *testing.T) {
	const secret = "frps-super-secret-token-xyz"
	// Persist frp_token exactly as bootstrap does: FRP group, PRIVATE flag.
	if err := op.SaveSettingItem(&model.SettingItem{
		Key:   conf.FrpToken,
		Value: secret,
		Type:  conf.TypeString,
		Group: model.FRP,
		Flag:  model.PRIVATE,
	}); err != nil {
		t.Fatalf("save frp_token: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteSettingItemByKey(conf.FrpToken) })

	w := getJSON(nil, PublicSettings, "/api/public/settings")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("PublicSettings must succeed, got %d: %s", code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("frp_token value leaked into public settings: %s", body)
	}
	if strings.Contains(body, conf.FrpToken) {
		t.Fatalf("frp_token key exposed by public settings: %s", body)
	}

	// Sanity: the value really is persisted (so the absence above is filtering,
	// not a missing row) and readable through the admin-only settings map.
	if got := op.GetSettingsMap()[conf.FrpToken]; got != secret {
		t.Fatalf("frp_token must be stored (admin-readable), got %q", got)
	}
}
