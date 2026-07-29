package migration

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shapes of the three tables introduced by this migration.
// No explicit TableName(): implementing the Tabler interface would make
// gorm use that name verbatim, bypassing NamingStrategy's TablePrefix
// (needed for the "x_"-prefixed migration tests, precedent: 0001's Meta).
// Instead the identifiers themselves are named so default pluralization
// lands on "permissions" / "role_permissions" / "user_roles".
type permission struct {
	ID         uint `gorm:"primaryKey"`
	Name       string
	Path       string `gorm:"index"`
	StorageID  uint   `gorm:"index:idx_permission_anchor"`
	Anchor     string `gorm:"size:255;index:idx_permission_anchor"`
	AnchorKind uint8
	Actions    string `gorm:"type:text"`
	Effect     string `gorm:"size:8;default:allow"`
	Recursive  bool
	Origin     string `gorm:"size:16;default:manual"`
}

type rolePermission struct {
	RoleID       uint `gorm:"primaryKey;autoIncrement:false"`
	PermissionID uint `gorm:"primaryKey;autoIncrement:false"`
}

type userRole struct {
	UserID uint `gorm:"primaryKey;autoIncrement:false"`
	RoleID uint `gorm:"primaryKey;autoIncrement:false"`
}

func init() {
	register(&gormigrate.Migration{
		ID: "0002_rbac",
		Migrate: func(tx *gorm.DB) error {
			if err := tx.AutoMigrate(&permission{}, &rolePermission{}, &userRole{}); err != nil {
				return err
			}
			return rbacBackfill(tx)
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			for _, tbl := range []interface{}{&userRole{}, &rolePermission{}, &permission{}} {
				if m.HasTable(tbl) {
					if err := m.DropTable(tbl); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
}

// rbacBackfill converts legacy role scopes and user role lists into the
// normalized tables. Idempotent: roles that already have bindings and users
// that already have rows are skipped.
func rbacBackfill(tx *gorm.DB) error {
	// mounts for anchor backfill (same approach as 0001)
	var mounts []struct {
		ID        uint
		MountPath string
	}
	if err := tx.Model(&Storage{}).Select("id", "mount_path").Find(&mounts).Error; err != nil {
		return err
	}
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].MountPath) > len(mounts[j].MountPath) })

	var roles []struct {
		ID            uint
		Name          string
		RawPermission string
	}
	if err := tx.Table(tx.NamingStrategy.TableName("roles")).Find(&roles).Error; err != nil {
		return err
	}
	for _, r := range roles {
		var cnt int64
		if err := tx.Model(&rolePermission{}).Where("role_id = ?", r.ID).Count(&cnt).Error; err != nil {
			return err
		}
		if cnt > 0 || r.RawPermission == "" {
			continue // idempotent re-run / empty role
		}
		var entries []struct {
			Path       string `json:"path"`
			Permission int32  `json:"permission"`
		}
		if err := json.Unmarshal([]byte(r.RawPermission), &entries); err != nil {
			return fmt.Errorf("role %d raw_permission: %w", r.ID, err)
		}
		for i, e := range entries {
			// Conversion rule duplicated in op.scopePermissionRows (internal/op/policy_cache.go) — change both together.
			actions := append(model.ActionsFromLegacyBits(e.Permission), model.ActRead)
			bs, err := json.Marshal(actions)
			if err != nil {
				return err
			}
			p := permission{
				Name: fmt.Sprintf("role:%s:scope:%d", r.Name, i), Path: e.Path,
				Actions: string(bs), Effect: "allow", Recursive: true, Origin: "scope",
			}
			for _, mt := range mounts {
				if rel, ok := relUnder(mt.MountPath, e.Path); ok {
					p.StorageID, p.Anchor, p.AnchorKind = mt.ID, rel, 0
					break
				}
			}
			if err := tx.Create(&p).Error; err != nil {
				return err
			}
			if err := tx.Create(&rolePermission{RoleID: r.ID, PermissionID: p.ID}).Error; err != nil {
				return err
			}
		}
	}

	var users []struct {
		ID   uint
		Role string
	}
	if err := tx.Table(tx.NamingStrategy.TableName("users")).Select("id", "role").Find(&users).Error; err != nil {
		return err
	}
	for _, u := range users {
		var cnt int64
		if err := tx.Model(&userRole{}).Where("user_id = ?", u.ID).Count(&cnt).Error; err != nil {
			return err
		}
		if cnt > 0 || u.Role == "" {
			continue
		}
		var rids []int
		if err := json.Unmarshal([]byte(u.Role), &rids); err != nil {
			return fmt.Errorf("user %d role json: %w", u.ID, err)
		}
		for _, rid := range rids {
			if err := tx.Create(&userRole{UserID: u.ID, RoleID: uint(rid)}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
