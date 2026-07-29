package data

// initRoles creates the default admin and guest roles if missing.
// These roles are essential and must not be modified or removed.

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

func initRoles() {
	guestRole, err := op.GetRoleByName("guest")
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			guestRole = &model.Role{
				ID:          uint(model.GUEST),
				Name:        "guest",
				Description: "Guest",
				Protected:   true,
				PermissionScopes: []model.PermissionEntry{
					{Path: "/", Permission: 0},
				},
			}
			if err := op.CreateRole(guestRole); err != nil {
				utils.Log.Fatalf("[init role] Failed to create guest role: %v", err)
			}
		} else {
			utils.Log.Fatalf("[init role] Failed to get guest role: %v", err)
		}
	} else if !guestRole.Protected {
		// Self-heal: rows created before Protected existed (or created by a
		// buggy prior build) must be upgraded so the built-in guest role
		// cannot be deleted or renamed via the role API.
		guestRole.Protected = true
		if err := op.UpdateRole(guestRole); err != nil {
			utils.Log.Fatalf("[init role] Failed to mark guest role protected: %v", err)
		}
	}

	adminRole, err := op.GetRoleByName("admin")
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			adminRole = &model.Role{
				ID:          uint(model.ADMIN),
				Name:        "admin",
				Description: "Administrator",
				Protected:   true,
				PermissionScopes: []model.PermissionEntry{
					{Path: "/", Permission: 0xFFFF},
				},
			}
			if err := op.CreateRole(adminRole); err != nil {
				utils.Log.Fatalf("[init role] Failed to create admin role: %v", err)
			}
		} else {
			utils.Log.Fatalf("[init role] Failed to get admin role: %v", err)
		}
	} else if !adminRole.Protected {
		// Self-heal: same as guest above.
		adminRole.Protected = true
		if err := op.UpdateRole(adminRole); err != nil {
			utils.Log.Fatalf("[init role] Failed to mark admin role protected: %v", err)
		}
	}
}
