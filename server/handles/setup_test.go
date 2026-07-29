package handles

import (
	"encoding/json"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

type setupStatusResp struct {
	Code int `json:"code"`
	Data struct {
		Completed bool `json:"completed"`
	} `json:"data"`
}

// TestGetSetupStatusDefaultsCompletedWhenRowMissing: this package's test DB
// (permission_test.go's init) never runs bootstrap's InitData, so
// conf.SetupCompleted has no seeded row here. GetSetupStatus must default to
// completed:true in that case — never force an admin into the wizard just
// because the flag itself couldn't be read (mirrors production: the row is
// always seeded before the server accepts requests, so this only guards the
// theoretical case where it's somehow missing).
func TestGetSetupStatusDefaultsCompletedWhenRowMissing(t *testing.T) {
	w := getJSON(handlerAdmin(), GetSetupStatus, "/api/admin/settings/setup")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("GetSetupStatus must succeed, got %d: %s", code, w.Body.String())
	}
	var resp setupStatusResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Data.Completed {
		t.Fatalf("GetSetupStatus must default completed:true when the row is missing, got %+v", resp.Data)
	}
}

// TestCompleteSetupThenGetSetupStatusReportsCompleted: the wizard's final
// step calls CompleteSetup; a subsequent GetSetupStatus must then report
// completed:true, and the underlying setting row must actually be "true".
func TestCompleteSetupThenGetSetupStatusReportsCompleted(t *testing.T) {
	// Start from an explicit false row, as a fresh install would have.
	item := model.SettingItem{Key: conf.SetupCompleted, Value: "false", Type: conf.TypeBool, Group: model.SITE, Flag: model.PRIVATE}
	if err := op.SaveSettingItem(&item); err != nil {
		t.Fatalf("seed setup_completed=false: %v", err)
	}

	wStatus := getJSON(handlerAdmin(), GetSetupStatus, "/api/admin/settings/setup")
	var before setupStatusResp
	if err := json.Unmarshal(wStatus.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Data.Completed {
		t.Fatalf("GetSetupStatus must report completed:false before CompleteSetup, got %+v", before.Data)
	}

	wComplete := postNoBody(handlerAdmin(), CompleteSetup, "/api/admin/settings/setup/complete")
	if code := respCode(t, wComplete); code != 200 {
		t.Fatalf("CompleteSetup must succeed, got %d: %s", code, wComplete.Body.String())
	}

	got, err := op.GetSettingItemByKey(conf.SetupCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "true" {
		t.Fatalf("CompleteSetup must persist setup_completed=true, got %q", got.Value)
	}

	wAfter := getJSON(handlerAdmin(), GetSetupStatus, "/api/admin/settings/setup")
	var after setupStatusResp
	if err := json.Unmarshal(wAfter.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if !after.Data.Completed {
		t.Fatalf("GetSetupStatus must report completed:true after CompleteSetup, got %+v", after.Data)
	}

	// Idempotent: calling CompleteSetup again once already completed stays a
	// no-op success.
	wComplete2 := postNoBody(handlerAdmin(), CompleteSetup, "/api/admin/settings/setup/complete")
	if code := respCode(t, wComplete2); code != 200 {
		t.Fatalf("CompleteSetup must stay idempotent once already completed, got %d: %s", code, wComplete2.Body.String())
	}
}
