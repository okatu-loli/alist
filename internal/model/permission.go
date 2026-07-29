package model

// Effect values for Permission.
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
)

// Origin marks how a Permission row was produced. Scope-origin rows are
// regenerated wholesale whenever their role's PermissionScopes change
// (dual-write, Step 4); manual rows are only touched via the permission API.
const (
	OriginManual = "manual"
	OriginScope  = "scope"
)

// PermPolicy values for the move/rename perm_policy request option (spec 0-C).
// PermPolicyFollow (default, and the empty value) keeps the historical
// behavior: the relocate hook makes manual permissions follow the object.
// PermPolicyDetach deletes the manual permission rows the relocation carried
// into the moved subtree, so the moved object falls back to inheriting the
// destination's effective policy.
const (
	PermPolicyFollow = "follow"
	PermPolicyDetach = "detach"
)

// Permission is the first-class RBAC5 entity: a resource bound to an action set
// with an allow/deny effect. A filesystem resource is identified by NodeID (the
// nodes registry, v4 permission stage 1) from which the virtual path is derived
// at decision time; Path is only the truth for the four nodeless cases (see
// NodeID below).
type Permission struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// ResourceType selects the matching domain: "fs" (or "") targets a
	// filesystem virtual Path; "system" targets an admin control-plane domain
	// (Path holds the domain selector). Type is a security-load-bearing gate:
	// a row of one type never satisfies a decision of the other.
	ResourceType string `json:"resource_type" gorm:"size:16;index;default:fs"`
	Name         string `json:"name"`
	// DisplayName is a purely cosmetic label for admin-authored permissions
	// (front-end display only; never consulted by the authorization engine).
	// Left empty for built-in/scope-origin rows, which are identified by Name.
	DisplayName string `json:"display_name" gorm:"default:''"`
	Path        string `json:"path" gorm:"index"`
	// NodeID references the nodes table (v4 permission stage 1): the object's
	// stable identity, from which the live virtual path is derived (the fs
	// engine matches on that derived path, never the stored Path). 0 means
	// unanchored, and only then is Path the truth: system-domain rows (Path is
	// the domain selector), scope declarations, above-mount virtual paths, and
	// migration rows not yet adopted. A node-anchored fs row stores no Path.
	NodeID    uint    `json:"node_id" gorm:"column:node_id;index"`
	Actions   Actions `json:"actions" gorm:"type:text"`
	Effect    string  `json:"effect" gorm:"size:8;default:allow"`
	Recursive bool    `json:"recursive"`
	Origin    string  `json:"origin" gorm:"size:16;default:manual"`
}

type RolePermission struct {
	RoleID       uint `json:"role_id" gorm:"primaryKey;autoIncrement:false"`
	PermissionID uint `json:"permission_id" gorm:"primaryKey;autoIncrement:false"`
}

type UserRole struct {
	UserID uint `json:"user_id" gorm:"primaryKey;autoIncrement:false"`
	RoleID uint `json:"role_id" gorm:"primaryKey;autoIncrement:false"`
}
