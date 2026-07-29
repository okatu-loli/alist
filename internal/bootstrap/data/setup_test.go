package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestMarkFreshInstallFlipsSetupCompleted: markFreshInstall must flip an
// existing conf.SetupCompleted="true" row to "false" — the mechanism InitData
// uses to mark a genuinely new install after initSettings has already seeded
// the row with its "true" default. It must also be safe to call again on an
// already-"false" row (InitData never does this in practice, but the helper
// itself must not misbehave if it did).
func TestMarkFreshInstallFlipsSetupCompleted(t *testing.T) {
	item := model.SettingItem{Key: conf.SetupCompleted, Value: "true", Type: conf.TypeBool, Group: model.SITE, Flag: model.PRIVATE}
	if err := op.SaveSettingItem(&item); err != nil {
		t.Fatalf("seed setup_completed=true: %v", err)
	}

	markFreshInstall()

	got, err := op.GetSettingItemByKey(conf.SetupCompleted)
	if err != nil {
		t.Fatalf("get setup_completed after markFreshInstall: %v", err)
	}
	if got.Value != "false" {
		t.Fatalf("markFreshInstall must flip setup_completed to false, got %q", got.Value)
	}

	markFreshInstall()
	got2, err := op.GetSettingItemByKey(conf.SetupCompleted)
	if err != nil {
		t.Fatalf("get setup_completed after second markFreshInstall: %v", err)
	}
	if got2.Value != "false" {
		t.Fatalf("markFreshInstall must stay idempotent, got %q", got2.Value)
	}
}

// TestInitUserReturnsIsNewInstallOnlyOnCreation: initUser must report
// isNewInstall=true exactly on the run that actually creates the admin user,
// and false on every subsequent call once admin already exists. InitData
// uses this signal to decide whether to call markFreshInstall — getting it
// wrong in either direction is exactly the "lock users into/out of the
// wizard" failure mode this wave has to avoid.
func TestInitUserReturnsIsNewInstallOnlyOnCreation(t *testing.T) {
	initRoles() // idempotent; guarantees admin/guest roles exist regardless of test run order

	if first := initUser(); !first {
		t.Fatalf("initUser must report isNewInstall=true the run it creates the admin user")
	}

	if second := initUser(); second {
		t.Fatalf("initUser must report isNewInstall=false once the admin user already exists")
	}
}
