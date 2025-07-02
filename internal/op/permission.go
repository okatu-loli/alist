package op

import "github.com/alist-org/alist/v3/internal/db"
import "github.com/alist-org/alist/v3/internal/model"

func CreatePermission(p *model.Permission) error                 { return db.CreatePermission(p) }
func UpdatePermission(p *model.Permission) error                 { return db.UpdatePermission(p) }
func GetPermissionById(id uint) (*model.Permission, error)       { return db.GetPermissionById(id) }
func GetPermissionByName(name string) (*model.Permission, error) { return db.GetPermissionByName(name) }
func GetPermissions(pageIndex, pageSize int) ([]model.Permission, int64, error) {
	return db.GetPermissions(pageIndex, pageSize)
}
func DeletePermissionById(id uint) error { return db.DeletePermissionById(id) }
