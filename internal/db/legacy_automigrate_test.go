package db

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestLegacyAutoMigrateSeedsDownloadKey guards the --migrate-mode=legacy
// path: LegacyAutoMigrate creates the signing_keys table via AutoMigrate but
// (unlike the gormigrate 0003 migration) never used to seed it, leaving
// every download/signed-link request permanently broken. It must now leave
// an active "download" key behind, same as the gormigrate path does.
func TestLegacyAutoMigrateSeedsDownloadKey(t *testing.T) {
	dB, err := gorm.Open(sqlite.Open("file:legacy_automigrate_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig()
	t.Cleanup(func() { conf.Conf = oldConf })
	old := db
	db = dB
	t.Cleanup(func() { db = old })

	if err := LegacyAutoMigrate(); err != nil {
		t.Fatal(err)
	}

	k, err := GetActiveSigningKey(model.SignPurposeDownload)
	if err != nil {
		t.Fatalf("expected an active download signing key after LegacyAutoMigrate, got err: %v", err)
	}
	if k.KID == "" || k.Secret == "" {
		t.Fatalf("seeded key missing kid/secret: %+v", k)
	}
}
