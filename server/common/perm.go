package common

import (
	"path/filepath"
	"strings"

	"github.com/alist-org/alist/v3/internal/model"
	opPkg "github.com/alist-org/alist/v3/internal/op"
)

// AllowOp lists fs operations that can be checked via role permissions.
var AllowOp = map[string]struct{}{
	"mkdir":                  {},
	"move":                   {},
	"copy":                   {},
	"rename":                 {},
	"remove":                 {},
	"remove_empty_directory": {},
}

// requireRolePermission verifies that user has role permission for op on path.
// If user's bit 14 is not set, the check always succeeds.
func RequireRolePermission(user *model.User, op string, targetPath string) bool {
	if ((user.Permission >> 14) & 1) == 0 {
		return true
	}
	if _, ok := AllowOp[op]; !ok {
		return true
	}

	// collect permission ids from roles
	permIDs := make(map[uint]struct{})
	for _, rid := range user.RoleInfo {
		role, err := opPkg.GetRoleById(uint(rid))
		if err != nil {
			continue
		}
		for _, pid := range role.PermissionInfo {
			permIDs[pid] = struct{}{}
		}
	}

	// check permissions
	for id := range permIDs {
		perm, err := opPkg.GetPermissionById(id)
		if err != nil {
			continue
		}
		match, _ := filepath.Match(perm.Path, targetPath)
		if !match {
			continue
		}
		for _, allowed := range strings.Split(perm.Method, ",") {
			if strings.TrimSpace(allowed) == op {
				return true
			}
		}
	}
	return false
}
