package migration

import (
	"errors"
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// ErrBaselineIrreversible is returned when a rollback would cross the
// baseline. `migrate down` stops here by design (rollout Step 1).
var ErrBaselineIrreversible = errors.New("0000_baseline is irreversible: rolling back the baseline would drop the database; restore from backup instead")

// ---- frozen snapshot of the 7 core tables (skeleton @ 2026-07-05) ----
// Do NOT sync these with internal/model. Model changes require a NEW
// migration; the schema-drift guard test enforces that.

type Sort struct {
	OrderBy        string
	OrderDirection string
	ExtractFolder  string
}

type Proxy struct {
	WebProxy      bool
	WebdavPolicy  string
	ProxyRange    bool
	DownProxyUrl  string
	DownProxySign bool `gorm:"default:true"`
}

type Storage struct {
	ID              uint   `gorm:"primaryKey"`
	MountPath       string `gorm:"unique"`
	Order           int
	Driver          string
	CacheExpiration int
	Status          string
	Addition        string `gorm:"type:text"`
	Remark          string
	Modified        time.Time
	Disabled        bool
	DisableIndex    bool
	EnableSign      bool
	Sort
	Proxy
}

type User struct {
	ID         uint   `gorm:"primaryKey"`
	Username   string `gorm:"unique"`
	PwdHash    string
	PwdTS      int64
	Salt       string
	Password   string
	BasePath   string
	Role       string `gorm:"type:text"` // JSON []int, serialized by model.Roles
	Disabled   bool
	Permission int32
	OtpSecret  string
	SsoID      string
	Authn      string `gorm:"type:text"`
}

type Meta struct {
	ID        uint   `gorm:"primaryKey"`
	Path      string `gorm:"unique"`
	Password  string
	PSub      bool
	Write     bool
	WSub      bool
	Hide      string
	HSub      bool
	Readme    string
	RSub      bool
	Header    string
	HeaderSub bool
}

type SettingItem struct {
	Key     string `gorm:"primaryKey"`
	Value   string
	Help    string
	Type    string
	Options string
	Group   int
	Flag    int
	Index   uint
}

type TaskItem struct {
	Key         string
	PersistData string `gorm:"type:text"`
}

type Role struct {
	ID            uint   `gorm:"primaryKey"`
	Name          string `gorm:"unique"`
	Description   string
	Default       bool   `gorm:"default:false"`
	RawPermission string `gorm:"type:text"` // JSON []PermissionEntry
}

type Session struct {
	UserID     uint   `gorm:"index"`
	DeviceKey  string `gorm:"primaryKey;size:64"`
	UserAgent  string `gorm:"size:255"`
	IP         string `gorm:"size:64"`
	LastActive int64
	Status     int
}

// baselineModels returns the frozen core-table set in a stable order.
func baselineModels() []interface{} {
	return []interface{}{
		&Storage{}, &User{}, &Meta{}, &SettingItem{},
		&TaskItem{}, &Role{}, &Session{},
	}
}

func init() {
	register(&gormigrate.Migration{
		ID: "0000_baseline",
		// AutoMigrate implements BOTH baseline scopes at once:
		//  - fresh DB: creates the 7 core tables
		//  - legacy v3 DB (any version): adds whatever columns/indexes are
		//    missing, leveling the schema up to the baseline BEFORE the ID
		//    is recorded — the "先拉平再标记" rule.
		// The 6 legacy v3 tables are intentionally absent from the model
		// set: recognized but unmanaged, left for the Step 14 data migration.
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(baselineModels()...)
		},
		Rollback: func(tx *gorm.DB) error {
			return ErrBaselineIrreversible
		},
	})
}
