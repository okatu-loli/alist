package db

import (
	log "github.com/sirupsen/logrus"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/gorm"
)

var db *gorm.DB

// onHandleChanged fires, in registration order, every time Init repoints db at
// a (possibly different) handle. internal/op registers itself here to empty
// its process-wide read caches (roleCache, userCache, settingCache, ...): those
// caches are only ever valid for whichever handle db currently holds, and
// db is the single choke point where that handle changes — including
// `alist migrate v4`'s initTarget, which calls bootstrap.InitDB -> Init a
// second time in-process when it opens the freshly created target. Without
// this, a process that reopens db against a different database (migrate v4
// followed by an in-process boot chief among them) would keep serving
// roles/users/settings cached from whatever database was open before.
//
// db can't import internal/op directly (op already imports db), hence the
// registration hook instead of a direct call.
var onHandleChanged []func()

// RegisterOnHandleChanged registers f to run every time Init repoints db at a
// new handle. See onHandleChanged.
func RegisterOnHandleChanged(f func()) {
	onHandleChanged = append(onHandleChanged, f)
}

// Init stores the gorm handle. Schema management happens in bootstrap.InitDB
// according to --migrate-mode; Init itself never touches the schema.
func Init(d *gorm.DB) {
	db = d
	for _, f := range onHandleChanged {
		f()
	}
}

// LegacyAutoMigrate is the pre-gormigrate safety valve
// (--migrate-mode=legacy). It must stay behind migration.GuardLegacyAutoMigrate.
func LegacyAutoMigrate() error {
	if err := AutoMigrate(new(model.Storage), new(model.User), new(model.Meta), new(model.SettingItem), new(model.TaskItem), new(model.Role), new(model.Session), new(model.Permission), new(model.RolePermission), new(model.UserRole), new(model.SigningKey), new(model.Token), new(model.AuditLog), new(model.Node), new(model.Label), new(model.LabelBinding), new(model.SearchNode)); err != nil {
		return err
	}
	// AutoMigrate only creates the table; unlike the gormigrate 0003
	// migration it doesn't seed it, so without this every download/signed
	// link request would 500 forever with no active signing key to sign with.
	return EnsureDownloadSigningKey()
}

func AutoMigrate(dst ...interface{}) error {
	var err error
	if conf.Conf.Database.Type == "mysql" {
		err = db.Set("gorm:table_options", "ENGINE=InnoDB CHARSET=utf8mb4").AutoMigrate(dst...)
	} else {
		err = db.AutoMigrate(dst...)
	}
	return err
}

func GetDb() *gorm.DB {
	return db
}

func Close() {
	log.Info("closing db")
	sqlDB, err := db.DB()
	if err != nil {
		log.Errorf("failed to get db: %s", err.Error())
		return
	}
	err = sqlDB.Close()
	if err != nil {
		log.Errorf("failed to close db: %s", err.Error())
		return
	}
}
