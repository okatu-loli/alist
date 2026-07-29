// Package legacy holds frozen, read-only snapshots of the v3 database schema
// as it existed before the v4 clean break. The `alist migrate v4` tool reads a
// v3 database through these structs; they are deliberately NOT the live
// internal/model types (whose shapes have since diverged) so the reader stays
// pinned to the on-disk v3 layout regardless of later model changes.
//
// Nothing here is ever written. Fields cover the five carried domains plus the
// columns needed to convert them; columns a given v3 version lacks simply stay
// zero (gorm Find selects `*` and maps what exists).
package legacy

import (
	"fmt"
	"strconv"
	"time"
)

// RawRole tolerates both v3 role encodings: a bare integer column (pre-3.46,
// e.g. `2`) and a JSON `[]int` text column (3.46+, e.g. `[1,2]`). It captures
// the raw bytes; conversion to role ids happens in the migrator, mirroring the
// parsing in internal/bootstrap/patch/v3_46_0/convert_role.go.
type RawRole []byte

// Scan implements sql.Scanner so gorm can map either an INTEGER or a TEXT
// column into the raw bytes without a type-mismatch error.
func (r *RawRole) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*r = nil
	case []byte:
		b := make([]byte, len(t))
		copy(b, t)
		*r = b
	case string:
		*r = []byte(t)
	case int64:
		*r = []byte(strconv.FormatInt(t, 10))
	case float64:
		*r = []byte(strconv.FormatInt(int64(t), 10))
	default:
		*r = []byte(fmt.Sprint(t))
	}
	return nil
}

// User is the v3 users row. Password hash/salt/timestamp and base path are
// carried verbatim into v4 (no rehash). Permission is the retired 17-bit mask,
// kept only so a full-row read does not error on old databases.
type User struct {
	ID         uint `gorm:"primaryKey"`
	Username   string
	PwdHash    string
	PwdTS      int64
	Salt       string
	Password   string
	BasePath   string
	Role       RawRole `gorm:"column:role"`
	Disabled   bool
	Permission int32
	OtpSecret  string
	SsoID      string
	Authn      string
}

// Role is the v3 roles row (present from 3.46+). RawPermission is the JSON
// PermissionScopes blob converted by op.CreateRole on carry.
type Role struct {
	ID            uint `gorm:"primaryKey"`
	Name          string
	Description   string
	Default       bool
	RawPermission string `gorm:"column:raw_permission"`
}

// Storage is the v3 storages row, carried verbatim (driver + addition JSON).
type Storage struct {
	ID              uint `gorm:"primaryKey"`
	MountPath       string
	Order           int
	Driver          string
	CacheExpiration int
	Status          string
	Addition        string
	Remark          string
	Modified        time.Time
	Disabled        bool
	DisableIndex    bool
	EnableSign      bool
	OrderBy         string
	OrderDirection  string
	ExtractFolder   string
	WebProxy        bool
	WebdavPolicy    string
	ProxyRange      bool
	DownProxyUrl    string
	DownProxySign   bool
}

// Meta is the v3 metas row (directory password/hide/readme/header), carried
// verbatim. v4 anchor columns are not present in v3 and resolve on first load.
type Meta struct {
	ID        uint `gorm:"primaryKey"`
	Path      string
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

// SettingItem is the v3 setting_items row. Only Key/Value are carried (for keys
// still valid in v4); the rest is re-derived by v4's initSettings on first boot.
type SettingItem struct {
	Key   string `gorm:"column:key;primaryKey"`
	Value string
	Type  string
	Group int
	Flag  int
}
