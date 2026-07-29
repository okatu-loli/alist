package op

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// Decision is the effective policy of one user at one path: per-action
// allow/deny sets (deny wins) plus a traverse flag for ancestor navigation.
type Decision struct {
	allowed  map[model.Action]struct{}
	denied   map[model.Action]struct{}
	Traverse bool
}

// Can reports whether action a is permitted, consulting the catalog's action
// hierarchy so a coarse grant covers finer actions (spec §6.2). It resolves over
// {a} ∪ ancestors(a): a deny anywhere in the chain wins (checked across the WHOLE
// chain first, so an ancestor deny beats a nearer allow — deny-first per level),
// then an allow anywhere in the chain permits, else default-deny. fs actions have
// no ancestors, so this reduces to the original flat allow/deny membership.
func (d *Decision) Can(a model.Action) bool {
	if d == nil {
		return false
	}
	chain := append([]model.Action{a}, ancestors(a)...)
	for _, x := range chain {
		if _, dn := d.denied[x]; dn {
			return false
		}
	}
	for _, x := range chain {
		if _, ok := d.allowed[x]; ok {
			return true
		}
	}
	return false
}

// Denied reports whether a is covered by an explicit deny row. Callers that
// have their own allow escape (meta.Write fallback, Traverse) must check
// this first: deny-first must beat any of those, or a deny row becomes
// silently overridable.
func (d *Decision) Denied(a model.Action) bool {
	if d == nil {
		return false
	}
	_, dn := d.denied[a]
	return dn
}

// LegacyBits projects the effective action set back onto the legacy 17-bit
// mask. Deprecated wrappers (server/common/role_perm.go) are the only caller;
// dies with them in Step 5.
func (d *Decision) LegacyBits() int32 {
	if d == nil {
		return 0
	}
	var out model.Actions
	for a := range d.allowed {
		if _, dn := d.denied[a]; !dn {
			out = append(out, a)
		}
	}
	return out.LegacyBits()
}

// UserPermissions returns the union of permission rows across the user's
// roles (cached per role). A scoped-token principal (Step 10) short-circuits to
// its injected ScopedPermissions: its policy is exactly the declared storage
// paths, never role-derived, so the deny-first engine confines it to scope.
func UserPermissions(u *model.User) ([]model.Permission, error) {
	if u == nil {
		return nil, nil
	}
	if u.Scoped {
		return u.ScopedPermissions, nil
	}
	var out []model.Permission
	for _, rid := range u.Role {
		ps, err := rolePermissions(uint(rid))
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

// ResolveEffectivePolicy is the fs entrypoint of the policy engine: permission
// p covers reqPath iff PathEqual(p.Path, reqPath) or (p.Recursive and reqPath
// under p.Path). Per action, a covering deny beats any allow; nothing covering
// means default-deny. It is a thin wrapper over the typed resolver so the ~20
// existing fs callers/tests keep compiling unchanged.
func ResolveEffectivePolicy(u *model.User, reqPath string) (*Decision, error) {
	return resolvePolicy(u, AuthzRequest{ResourceType: model.ResourceFS, Path: reqPath})
}

// resolvePolicy is the type-dispatched policy engine. A permission row is
// matched only within its own resource type — isFS(req) != isFS(p) skips the
// row outright; this cross-type gate is security-load-bearing (an fs row that
// carries a system action must never satisfy a system decision, and vice
// versa). Within a type: system rows match by exact domain (Path is a domain
// selector, not cleaned); fs rows keep the PathEqual/recursive-subpath match
// and the Traverse ancestor-navigation flag (fs-only).
func resolvePolicy(u *model.User, req AuthzRequest) (*Decision, error) {
	d := &Decision{allowed: map[model.Action]struct{}{}, denied: map[model.Action]struct{}{}}
	if u == nil || u.Disabled {
		return d, nil
	}
	fsReq := isFS(req.ResourceType)
	reqPath := req.Path
	if fsReq {
		reqPath = utils.FixAndCleanPath(req.Path)
	}
	perms, err := UserPermissions(u)
	if err != nil {
		return nil, err
	}
	for _, p := range perms {
		if fsReq != isFS(p.ResourceType) {
			continue // cross-type rows never cover each other (security gate)
		}
		var covers bool
		if fsReq {
			covers = utils.PathEqual(p.Path, reqPath) ||
				(p.Recursive && utils.IsSubPath(p.Path, reqPath))
		} else {
			covers = p.Path == req.Path // exact domain match
		}
		if covers {
			for _, a := range p.Actions {
				if p.Effect == model.EffectDeny {
					d.denied[a] = struct{}{}
				} else {
					d.allowed[a] = struct{}{}
				}
			}
		}
		if fsReq && p.Effect == model.EffectAllow && p.Actions.Contains(model.ActRead) &&
			utils.IsSubPath(reqPath, p.Path) {
			d.Traverse = true
		}
	}
	return d, nil
}

// InvalidateRolePolicyForTest is a test hook; production invalidation happens
// inside op CRUD (policy_cache.go / permission.go).
func InvalidateRolePolicyForTest(roleID uint) {
	invalidateRolePolicy(roleID)
}
