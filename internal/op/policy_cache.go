package op

import (
	"fmt"
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/singleflight"
)

// rolePermCache caches the permission rows of one role, keyed by role ID.
// Invalidated on any permission/binding/role mutation; the policy engine
// (policy.go) reads through it.
var rolePermCache = cache.NewMemCache[[]model.Permission](cache.WithShards[[]model.Permission](2))
var rolePermG singleflight.Group[[]model.Permission]

const rolePermCacheExpiration = time.Hour

func rolePermissions(roleID uint) ([]model.Permission, error) {
	key := fmt.Sprint(roleID)
	if ps, ok := rolePermCache.Get(key); ok {
		return ps, nil
	}
	ps, err, _ := rolePermG.Do(key, func() ([]model.Permission, error) {
		_ps, err := db.GetPermissionsByRoleIDs([]uint{roleID})
		if err != nil {
			return nil, err
		}
		derived := deriveNodePaths(_ps)
		rolePermCache.Set(key, derived, cache.WithEx[[]model.Permission](rolePermCacheExpiration))
		return derived, nil
	})
	return ps, err
}

// deriveNodePaths overwrites each node-anchored row's Path with its live derived
// virtual path (NodeVirtualPath), so the policy engine — which still matches on
// Path — sees where the object lives NOW, tracking moves/renames/mount renames
// for free. A row whose node no longer resolves (its storage was deleted) is
// DROPPED so it stops participating in decisions (spec §7); it remains visible
// in the admin permission API (GetPermissions does not filter). Unanchored rows
// (NodeID 0: scope declarations, system domains, above-mount paths, not-yet-
// adopted migration rows) keep their stored Path untouched. Operates on a copy;
// the DB row is never rewritten.
func deriveNodePaths(ps []model.Permission) []model.Permission {
	out := make([]model.Permission, 0, len(ps))
	for _, p := range ps {
		if p.NodeID != 0 {
			vp, err := NodeVirtualPath(p.NodeID)
			if err != nil {
				continue // storage gone: skip in decisions (spec §7)
			}
			p.Path = vp
		}
		out = append(out, p)
	}
	return out
}

func invalidateRolePolicy(roleID uint) {
	rolePermCache.Del(fmt.Sprint(roleID))
}

func invalidateAllPolicy() {
	rolePermCache.Clear()
}

// scopePermissionRows derives the role's scope-origin permission rows from
// PermissionScopes (dual-write; manual permissions are untouched by this
// conversion). The caller persists the result atomically alongside the role
// row via db.CreateRoleWithScopePermissions/UpdateRoleWithScopePermissions.
//
// Kept in lockstep with the conversion baked into the 0002_rbac migration
// (internal/db/migration/0002_rbac.go) — a change here needs a matching
// change there.
func scopePermissionRows(r *model.Role) []model.Permission {
	rows := make([]model.Permission, 0, len(r.PermissionScopes))
	for i, entry := range r.PermissionScopes {
		// Scope rows are never node-anchored: the role's PermissionScopes
		// declaration is their sole truth (regenerated wholesale on each resync),
		// so Path is what the engine matches on and stays the truth.
		rows = append(rows, model.Permission{
			Name:      fmt.Sprintf("role:%s:scope:%d", r.Name, i),
			Path:      entry.Path,
			Actions:   append(model.ActionsFromLegacyBits(entry.Permission), model.ActRead),
			Effect:    model.EffectAllow,
			Recursive: true,
			Origin:    model.OriginScope,
		})
	}
	return rows
}

func syncUserRoles(u *model.User) error {
	rids := make([]uint, 0, len(u.Role))
	for _, r := range u.Role {
		rids = append(rids, uint(r))
	}
	return db.SetUserRoles(u.ID, rids)
}
