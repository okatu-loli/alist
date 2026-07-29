package device

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/pkg/errors"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:device_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

func mustDeviceTestUser(t *testing.T, username string) *model.User {
	t.Helper()
	u := &model.User{Username: username}
	u.SetPassword("pw-" + username)
	if err := op.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(u.ID) })
	return u
}

// enforceMaxDevices is a behavior-preserving extraction of what used to be
// three inlined copies of the same max-devices block (Handle and the two
// branches of EnsureActiveOnLogin). These two tests pin its deny and
// evict_oldest branches directly, standing in as regression anchors for
// that extraction.

// TestEnforceMaxDevicesDeny pins that with MaxDevices=1 and the default
// "deny" policy, a user already at the limit is rejected with
// errs.TooManyDevices instead of being allowed to exceed it.
func TestEnforceMaxDevicesDeny(t *testing.T) {
	user := mustDeviceTestUser(t, "enforce-deny-user")

	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.MaxDevices, Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.DeviceEvictPolicy, Value: "deny"}); err != nil {
		t.Fatal(err)
	}

	if err := db.CreateSession(&model.Session{
		UserID:     user.ID,
		DeviceKey:  "device-existing-" + user.Username,
		LastActive: 1,
		Status:     model.SessionActive,
	}); err != nil {
		t.Fatal(err)
	}

	err := enforceMaxDevices(user.ID)
	if !errors.Is(err, errs.TooManyDevices) {
		t.Fatalf("enforceMaxDevices() error = %v, want errs.TooManyDevices", err)
	}
}

// TestEnforceMaxDevicesEvictOldest pins that with MaxDevices=1 and the
// "evict_oldest" policy, a user already at the limit has their oldest
// active session marked inactive and the call succeeds (no error), instead
// of being denied.
func TestEnforceMaxDevicesEvictOldest(t *testing.T) {
	user := mustDeviceTestUser(t, "enforce-evict-user")

	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.MaxDevices, Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.DeviceEvictPolicy, Value: "evict_oldest"}); err != nil {
		t.Fatal(err)
	}

	oldestKey := "device-oldest-" + user.Username
	if err := db.CreateSession(&model.Session{
		UserID:     user.ID,
		DeviceKey:  oldestKey,
		LastActive: 1,
		Status:     model.SessionActive,
	}); err != nil {
		t.Fatal(err)
	}

	if err := enforceMaxDevices(user.ID); err != nil {
		t.Fatalf("enforceMaxDevices() error = %v, want nil", err)
	}

	got, err := db.GetSession(user.ID, oldestKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.SessionInactive {
		t.Errorf("oldest session Status = %d, want SessionInactive after evict_oldest", got.Status)
	}
}
