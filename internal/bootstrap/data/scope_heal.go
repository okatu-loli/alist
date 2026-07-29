package data

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// healRoleScopeBindings restores every role whose scope-derived permission
// rows have gone missing or unbound. A role's filesystem rights live on
// scope-origin permission rows addressed only through role_permissions; a
// historical binding wipe (op.SetRolePermissions used to trust callers to
// round-trip scope ids) orphaned those rows and silently stripped the role —
// observed in the wild as an admin locked out of the whole filesystem. The
// wipe path is closed server-side now; this self-heal repairs databases that
// were already damaged. Idempotent: healthy roles resync to identical rows.
func healRoleScopeBindings() {
	roles, _, err := op.GetRoles(1, 10000)
	if err != nil {
		utils.Log.Errorf("[scope heal] failed to list roles: %v", err)
		return
	}
	for i := range roles {
		r := &roles[i]
		if len(r.PermissionScopes) == 0 {
			continue
		}
		bound, err := op.GetPermissionsByRole(r.ID)
		if err != nil {
			utils.Log.Errorf("[scope heal] failed to load bindings for role %q: %v", r.Name, err)
			continue
		}
		scopeBound := 0
		for _, p := range bound {
			if p.Origin == model.OriginScope {
				scopeBound++
			}
		}
		if scopeBound == len(r.PermissionScopes) {
			continue
		}
		utils.Log.Warnf("[scope heal] role %q has %d/%d scope rows bound; resyncing",
			r.Name, scopeBound, len(r.PermissionScopes))
		if err := op.SyncRoleScopePermissions(r.ID); err != nil {
			utils.Log.Errorf("[scope heal] resync for role %q failed: %v", r.Name, err)
		}
	}
}
