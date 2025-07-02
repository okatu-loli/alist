package op

import "github.com/alist-org/alist/v3/internal/db"
import "github.com/alist-org/alist/v3/internal/model"

func CreateRole(r *model.Role) error                 { return db.CreateRole(r) }
func UpdateRole(r *model.Role) error                 { return db.UpdateRole(r) }
func GetRoleById(id uint) (*model.Role, error)       { return db.GetRoleById(id) }
func GetRoleByName(name string) (*model.Role, error) { return db.GetRoleByName(name) }
func GetRoles(pageIndex, pageSize int) ([]model.Role, int64, error) {
	return db.GetRoles(pageIndex, pageSize)
}
func DeleteRoleById(id uint) error { return db.DeleteRoleById(id) }
