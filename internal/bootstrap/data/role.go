package data

import (
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

func initRoles() {
	fullPerm, err := op.GetPermissionByName("full-access")
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fullPerm = &model.Permission{
				Name:        "full-access",
				Method:      "mkdir,move,copy,rename,remove,remove_empty_directory",
				Path:        "*",
				Description: "Allow all file operations",
			}
			if err := op.CreatePermission(fullPerm); err != nil {
				utils.Log.Fatalf("[init role] failed create permission: %v", err)
			}
		} else {
			utils.Log.Fatalf("[init role] failed get permission: %v", err)
		}
	}

	ensureRole := func(id uint, name, desc string, permIDs []uint) {
		_, err := op.GetRoleById(id)
		if err == nil {
			return
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			utils.Log.Fatalf("[init role] failed get role %s: %v", name, err)
			return
		}
		r := &model.Role{ID: id, Name: name, Description: desc, PermissionInfo: permIDs}
		_ = r.SavePermissions()
		if err := db.CreateRole(r); err != nil {
			utils.Log.Fatalf("[init role] failed create role %s: %v", name, err)
		}
	}

	ensureRole(uint(model.ADMIN), "Admin", "Built-in administrator role", []uint{fullPerm.ID})
	ensureRole(uint(model.GUEST), "Guest", "Built-in guest role", []uint{})
}
