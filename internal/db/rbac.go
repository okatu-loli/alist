package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func AddRolePermission(roleID, permID uint) error {
	return errors.WithStack(db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.RolePermission{RoleID: roleID, PermissionID: permID}).Error)
}

func SetRolePermissions(roleID uint, permIDs []uint) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", roleID).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		for _, pid := range permIDs {
			if err := tx.Create(&model.RolePermission{RoleID: roleID, PermissionID: pid}).Error; err != nil {
				return err
			}
		}
		return nil
	}))
}

func GetPermissionIDsByRole(roleID uint) ([]uint, error) {
	var ids []uint
	err := db.Model(&model.RolePermission{}).Where("role_id = ?", roleID).
		Pluck("permission_id", &ids).Error
	return ids, errors.WithStack(err)
}

func GetRoleIDsByPermission(permID uint) ([]uint, error) {
	var ids []uint
	err := db.Model(&model.RolePermission{}).Where("permission_id = ?", permID).
		Pluck("role_id", &ids).Error
	return ids, errors.WithStack(err)
}

func DeleteRolePermissionsByRole(roleID uint) error {
	return errors.WithStack(db.Where("role_id = ?", roleID).Delete(&model.RolePermission{}).Error)
}

func SetUserRoles(userID uint, roleIDs []uint) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Delete(&model.UserRole{}).Error; err != nil {
			return err
		}
		for _, rid := range roleIDs {
			if err := tx.Create(&model.UserRole{UserID: userID, RoleID: rid}).Error; err != nil {
				return err
			}
		}
		return nil
	}))
}

func GetUserRoleIDs(userID uint) ([]uint, error) {
	var ids []uint
	err := db.Model(&model.UserRole{}).Where("user_id = ?", userID).
		Pluck("role_id", &ids).Error
	return ids, errors.WithStack(err)
}

func DeleteUserRolesByUser(userID uint) error {
	return errors.WithStack(db.Where("user_id = ?", userID).Delete(&model.UserRole{}).Error)
}
