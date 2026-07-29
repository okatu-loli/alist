package op

import (
	"fmt"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// validateEffect rejects anything but the two known Effect values. The
// policy engine (policy.go) treats any Effect != EffectDeny as an allow, so
// a typo like "Deny"/"DENY" would otherwise silently grant access instead of
// erroring out.
func validateEffect(effect string) error {
	if effect != model.EffectAllow && effect != model.EffectDeny {
		return fmt.Errorf("invalid effect %q: must be allow or deny", effect)
	}
	return nil
}

func CreatePermission(p *model.Permission) error {
	if p.Effect == "" {
		p.Effect = model.EffectAllow
	}
	if err := validateEffect(p.Effect); err != nil {
		return err
	}
	if p.Origin == "" {
		p.Origin = model.OriginManual
	}
	if err := ValidateActions(p.ResourceType, p.Actions); err != nil {
		return err
	}
	if err := normalizePermissionResource(p); err != nil {
		return err
	}
	if err := db.CreatePermission(p); err != nil {
		return err
	}
	invalidateAllPolicy()
	return nil
}

func UpdatePermission(p *model.Permission) error {
	if p.Effect == "" {
		p.Effect = model.EffectAllow
	}
	if err := validateEffect(p.Effect); err != nil {
		return err
	}
	if err := ValidateActions(p.ResourceType, p.Actions); err != nil {
		return err
	}
	if err := normalizePermissionResource(p); err != nil {
		return err
	}
	if err := db.UpdatePermission(p); err != nil {
		return err
	}
	invalidateAllPolicy()
	return nil
}

// normalizePermissionResource canonicalizes a permission's resource selector by
// type (spec §2.4). A system row stores a canonical domain selector in Path, is
// never path-cleaned, carries no node (NodeID zero so fs matching cannot leak
// into it), and ignores Recursive (system matching is always exact-domain). An
// fs row is anchored to a node (creating its chain on demand); the live path is
// derived from that node at decision time, so the stored Path is cleared — no
// second, drifting truth. Only an fs path above every mount point (which fs ops
// cannot move) stays path-anchored with NodeID zero.
func normalizePermissionResource(p *model.Permission) error {
	if p.ResourceType == model.ResourceSystem {
		dom, ok := CanonicalDomain(p.Path)
		if !ok {
			return fmt.Errorf("unknown system domain %q", p.Path)
		}
		p.Path = dom
		p.NodeID = 0
		p.Recursive = false
		return nil
	}
	p.Path = utils.FixAndCleanPath(p.Path)
	if ref, err := ResolvePathAnchor(p.Path); err == nil {
		nid, nerr := db.EnsureNodeChain(ref.StorageID, ref.Anchor)
		if nerr != nil {
			return nerr
		}
		// Node-anchored: the live path is derived from the node
		// (policy_cache.deriveNodePaths), so do not persist a stored Path.
		p.Path, p.NodeID = "", nid
	} else {
		// Above every mount point (errs.StorageNotFound): stays path-anchored.
		p.NodeID = 0
	}
	return nil
}

func DeletePermission(id uint) error {
	if err := db.DeletePermissionById(id); err != nil {
		return err
	}
	invalidateAllPolicy()
	return nil
}

// GetPermission (single-row read, also the fetch step of update/delete) derives
// a node-anchored row's live virtual path so the edit form shows where the
// object is NOW. On derivation failure (storage deleted) it KEEPS the stored
// Path rather than blanking it: this getter feeds mutation round-trips, and a
// blank Path would re-anchor the row to "/" on save.
func GetPermission(id uint) (*model.Permission, error) {
	p, err := db.GetPermissionById(id)
	if err != nil {
		return nil, err
	}
	if p.NodeID != 0 {
		if vp, verr := NodeVirtualPath(p.NodeID); verr == nil {
			p.Path = vp
		}
	}
	return p, nil
}

// GetPermissions is the admin LIST endpoint: it does NOT drop storage-deleted
// rows (unlike the policy cache — spec §7: such rows stay visible for manual
// cleanup), but it DOES derive each node-anchored row's live virtual path so the
// UI shows where the object is NOW, tracking moves/renames the stored Path
// column no longer follows (spec §9: path out, server-derived). A row whose
// storage was deleted derives nothing: its Path is blanked so the UI can flag it
// as unresolved ("storage deleted") — the row stays listed, the admin can then
// delete it.
func GetPermissions(pageIndex, pageSize int) ([]model.Permission, int64, error) {
	ps, count, err := db.GetPermissions(pageIndex, pageSize)
	if err != nil {
		return nil, 0, err
	}
	for i := range ps {
		if ps[i].NodeID == 0 {
			continue // unanchored: stored Path is its truth
		}
		if vp, verr := NodeVirtualPath(ps[i].NodeID); verr == nil {
			ps[i].Path = vp
		} else {
			ps[i].Path = ""
		}
	}
	return ps, count, nil
}

// SetRolePermissions wholesale-replaces the role's MANUAL bindings with
// permIDs. Scope-origin bindings are system-managed (derived from the role's
// PermissionScopes) and are preserved server-side regardless of the caller's
// set: trusting clients to round-trip scope ids let one bad caller orphan a
// role's scope rows — observed in the wild as an admin locked out of the
// whole filesystem (its fs grant lives on a scope row).
func SetRolePermissions(roleID uint, permIDs []uint) error {
	bound, err := db.GetPermissionsByRoleIDs([]uint{roleID})
	if err != nil {
		return err
	}
	keep := make([]uint, 0, len(permIDs)+2)
	seen := make(map[uint]struct{}, len(permIDs)+2)
	for _, p := range bound {
		if p.Origin == model.OriginScope {
			keep = append(keep, p.ID)
			seen[p.ID] = struct{}{}
		}
	}
	for _, id := range permIDs {
		if _, dup := seen[id]; !dup {
			keep = append(keep, id)
			seen[id] = struct{}{}
		}
	}
	if err := db.SetRolePermissions(roleID, keep); err != nil {
		return err
	}
	invalidateRolePolicy(roleID)
	return nil
}

// SyncRoleScopePermissions re-derives and re-binds the role's scope-origin
// permission rows from its PermissionScopes, first sweeping any orphaned
// scope rows left behind by historical binding wipes (they are unreachable:
// scope rows are only ever addressed through their role binding). Used by the
// boot self-heal; safe to call on a healthy role (idempotent by content).
func SyncRoleScopePermissions(roleID uint) error {
	r, err := db.GetRole(roleID)
	if err != nil {
		return err
	}
	if err := db.DeleteOrphanScopePermissions(r.Name); err != nil {
		return err
	}
	if err := db.UpdateRoleWithScopePermissions(r, scopePermissionRows(r)); err != nil {
		return err
	}
	invalidateRolePolicy(roleID)
	return nil
}

func GetRoleIDsByPermission(permID uint) ([]uint, error) {
	return db.GetRoleIDsByPermission(permID)
}

// GetPermissionsByRole returns every permission row bound to a single role
// (scope + manual alike, each still carrying its Origin) — the forward
// counterpart to GetRoleIDsByPermission's reverse lookup.
func GetPermissionsByRole(roleID uint) ([]model.Permission, error) {
	return db.GetPermissionsByRoleIDs([]uint{roleID})
}
