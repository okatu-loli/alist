package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// CreateRoleWithScopePermissions persists the role row and its scope-derived
// permission rows atomically: a failure at any point (including the
// default-role exclusivity update) leaves nothing behind.
func CreateRoleWithScopePermissions(r *model.Role, perms []model.Permission) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(r).Error; err != nil {
			return err
		}
		if r.Default {
			if err := tx.Model(&model.Role{}).Where("id <> ?", r.ID).Update("default", false).Error; err != nil {
				return err
			}
		}
		return replaceScopePermissionsTx(tx, r.ID, perms)
	}))
}

// UpdateRoleWithScopePermissions persists the role row and re-derives its
// scope permission rows atomically, mirroring CreateRoleWithScopePermissions.
func UpdateRoleWithScopePermissions(r *model.Role, perms []model.Permission) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(r).Error; err != nil {
			return err
		}
		if r.Default {
			if err := tx.Model(&model.Role{}).Where("id <> ?", r.ID).Update("default", false).Error; err != nil {
				return err
			}
		}
		return replaceScopePermissionsTx(tx, r.ID, perms)
	}))
}

// replaceScopePermissionsTx is the tx-scoped equivalent of
// DeleteScopePermissionsByRole (permission.go) followed by a create+bind
// loop: it drops the role's existing scope-origin permission rows and
// replaces them with perms, all within the caller's transaction. Manual
// permissions bound to the role are untouched.
func replaceScopePermissionsTx(tx *gorm.DB, roleID uint, perms []model.Permission) error {
	var ids []uint
	if err := tx.Model(&model.Permission{}).
		Joins(rolePermissionsJoin()).
		Where("rp.role_id = ? AND origin = ?", roleID, model.OriginScope).
		Pluck(permissionIDColumn(), &ids).Error; err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := tx.Where("permission_id IN ?", ids).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&model.Permission{}, ids).Error; err != nil {
			return err
		}
	}
	for i := range perms {
		if err := tx.Create(&perms[i]).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.RolePermission{RoleID: roleID, PermissionID: perms[i].ID}).Error; err != nil {
			return err
		}
	}
	return nil
}

// DeleteOrphanScopePermissions removes scope-origin permission rows that
// belong to roleName by naming convention ("role:<name>:scope:<i>", see
// op.scopePermissionRows) but have lost their role binding. Such rows are
// unreachable — scope rows are only ever addressed through their binding — and
// they shadow the re-derived rows the boot self-heal is about to write.
func DeleteOrphanScopePermissions(roleName string) error {
	return errors.WithStack(db.
		Where("origin = ? AND name LIKE ? AND id NOT IN (?)",
			model.OriginScope, "role:"+roleName+":scope:%",
			db.Model(&model.RolePermission{}).Select("permission_id")).
		Delete(&model.Permission{}).Error)
}

// CountPermissionsByNamePrefix counts permission rows whose name starts with
// prefix — test/diagnostic helper for the scope-heal invariant (exactly one
// row per scope index after a resync).
func CountPermissionsByNamePrefix(prefix string, out *int64) error {
	return errors.WithStack(db.Model(&model.Permission{}).
		Where("name LIKE ?", prefix+"%").Count(out).Error)
}
