package data

// Preset role seeding runs in the bootstrap sequence AFTER initRoles (so
// admin/guest already exist) — not in a migration: migrations run before
// bootstrap, when the admin role does not yet exist on a fresh install
// (spec §10). Seeding is row-level idempotent: the idempotency key is the
// permission row content (owning role + resource_type + canonical path +
// effect + recursive + action-set), NOT the mere existence of the role. So a
// crash that created a preset role but failed to seed its rows recovers on the
// next run instead of leaving the role permanently powerless (spec §4.2).

import (
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// presetPerm describes one manual permission row a preset (or the admin
// backfill) must own. All preset rows are manual origin: the system rows cannot
// be expressed via Role.PermissionScopes (which only encodes fs paths + legacy
// bits), and the fs rows are seeded as manual too so the seeding path stays
// uniform. manual rows coexist with machine-generated scope rows —
// replaceScopePermissionsTx only ever deletes origin='scope' rows (§4.2).
type presetPerm struct {
	name         string
	resourceType string
	path         string
	actions      []model.Action
	effect       string
	recursive    bool
}

// presetRoleDef is a built-in operator template: a protected role plus the
// manual permission rows it must carry (spec §4.1).
type presetRoleDef struct {
	name        string
	description string
	perms       []presetPerm
}

// presetRoleDefs returns the built-in operator presets. viewer/editor/uploader
// are fs templates; storage-manager/user-manager are system control-plane
// templates. user-manager deliberately carries only user lifecycle (no
// authorization-graph writes — those are IsAdmin-only, §5).
func presetRoleDefs() []presetRoleDef {
	return []presetRoleDef{
		{
			name:        "viewer",
			description: "Read-only access to the whole filesystem",
			perms: []presetPerm{{
				name: "viewer:fs:/", resourceType: model.ResourceFS, path: "/",
				actions: []model.Action{model.ActRead}, effect: model.EffectAllow, recursive: true,
			}},
		},
		{
			name:        "editor",
			description: "Read/write access to the whole filesystem",
			perms: []presetPerm{{
				name: "editor:fs:/", resourceType: model.ResourceFS, path: "/",
				actions: []model.Action{model.ActRead, model.ActWrite, model.ActRename,
					model.ActMove, model.ActCopy, model.ActRemove},
				effect: model.EffectAllow, recursive: true,
			}},
		},
		{
			name:        "uploader",
			description: "Upload-only access to the whole filesystem",
			perms: []presetPerm{{
				name: "uploader:fs:/", resourceType: model.ResourceFS, path: "/",
				actions: []model.Action{model.ActRead, model.ActWrite},
				effect:  model.EffectAllow, recursive: true,
			}},
		},
		{
			name:        "storage-manager",
			description: "Manage storages only",
			perms: []presetPerm{{
				name: "storage-manager:system:storage", resourceType: model.ResourceSystem, path: "storage",
				actions: []model.Action{model.SystemAction("storage", "view"), model.SystemAction("storage", "manage")},
				effect:  model.EffectAllow,
			}},
		},
		{
			name:        "user-manager",
			description: "Manage user lifecycle only",
			perms: []presetPerm{{
				name: "user-manager:system:user", resourceType: model.ResourceSystem, path: "user",
				actions: []model.Action{model.SystemAction("user", "view"), model.SystemAction("user", "manage")},
				effect:  model.EffectAllow,
			}},
		},
	}
}

// initPresetRoles creates each preset role (Protected) if its name is missing,
// then row-idempotently ensures its manual permission rows exist. Must run
// after initRoles.
func initPresetRoles() {
	for _, preset := range presetRoleDefs() {
		role, err := op.GetRoleByName(preset.name)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				utils.Log.Fatalf("[init preset role] failed to get role %q: %v", preset.name, err)
			}
			role = &model.Role{
				Name:        preset.name,
				Description: preset.description,
				Protected:   true,
			}
			if err := op.CreateRole(role); err != nil {
				utils.Log.Fatalf("[init preset role] failed to create role %q: %v", preset.name, err)
			}
		}
		if err := ensureRolePerms(role.ID, preset.perms); err != nil {
			utils.Log.Fatalf("[init preset role] failed to seed rows for %q: %v", preset.name, err)
		}
	}
}

// backfillAdminSystem gives the admin role an explicit manual system row per
// control-plane domain (all of that domain's registered system actions). This
// is belt-and-suspenders: admin already passes every RequireSystem check via
// the IsAdmin short-circuit, so admin behavior is unchanged with or without
// these rows. The domain/action set is derived from the live action catalog so
// it always matches what T2 registered (e.g. driver, which exposes view only).
// Must run after initRoles.
func backfillAdminSystem() {
	admin, err := op.GetRoleByName("admin")
	if err != nil {
		utils.Log.Fatalf("[backfill admin system] failed to get admin role: %v", err)
	}
	snap, err := op.ActionCatalog()
	if err != nil {
		utils.Log.Fatalf("[backfill admin system] failed to load action catalog: %v", err)
	}
	domainActions := map[string][]model.Action{}
	var domains []string
	for _, a := range snap.Actions {
		if a.Orphaned || a.ResourceType != model.ResourceSystem || a.Domain == "" {
			continue
		}
		if _, seen := domainActions[a.Domain]; !seen {
			domains = append(domains, a.Domain)
		}
		domainActions[a.Domain] = append(domainActions[a.Domain], a.Name)
	}
	perms := make([]presetPerm, 0, len(domains))
	for _, dom := range domains {
		perms = append(perms, presetPerm{
			name:         "admin:system:" + dom,
			resourceType: model.ResourceSystem,
			path:         dom,
			actions:      domainActions[dom],
			effect:       model.EffectAllow,
		})
	}
	if err := ensureRolePerms(admin.ID, perms); err != nil {
		utils.Log.Fatalf("[backfill admin system] failed to seed admin system rows: %v", err)
	}
}

// ensureRolePerms row-idempotently makes sure every want row exists as a manual
// permission bound to roleID. A row already present (matched by content) is
// left untouched; a missing one is created via op.CreatePermission (which
// validates actions against the catalog and canonicalizes the resource) and
// bound. op.CreatePermission de-anchors system rows and cleans fs paths, so the
// stored form matches what hasRolePerm canonicalizes want to.
func ensureRolePerms(roleID uint, perms []presetPerm) error {
	existing, err := db.GetPermissionsByRoleIDs([]uint{roleID})
	if err != nil {
		return err
	}
	for _, want := range perms {
		if hasRolePerm(existing, want) {
			continue
		}
		p := &model.Permission{
			Name:         want.name,
			ResourceType: want.resourceType,
			Path:         want.path,
			Actions:      want.actions,
			Effect:       want.effect,
			Recursive:    want.recursive,
			Origin:       model.OriginManual,
		}
		if err := op.CreatePermission(p); err != nil {
			return err
		}
		if err := db.AddRolePermission(roleID, p.ID); err != nil {
			return err
		}
		existing = append(existing, *p)
	}
	return nil
}

// hasRolePerm reports whether existing already contains a row matching want by
// content: same resource type, canonical path, effect, recursive flag, and
// action set (order-independent). This is the row-level idempotency key.
func hasRolePerm(existing []model.Permission, want presetPerm) bool {
	wantRT := normResourceType(want.resourceType)
	wantPath := canonicalResourcePath(want.resourceType, want.path)
	wantEffect := want.effect
	if wantEffect == "" {
		wantEffect = model.EffectAllow
	}
	// system rows never carry Recursive (op.CreatePermission forces it off).
	wantRecursive := want.recursive && wantRT != model.ResourceSystem
	for _, p := range existing {
		if normResourceType(p.ResourceType) != wantRT {
			continue
		}
		if p.Path != wantPath {
			continue
		}
		pEffect := p.Effect
		if pEffect == "" {
			pEffect = model.EffectAllow
		}
		if pEffect != wantEffect {
			continue
		}
		if p.Recursive != wantRecursive {
			continue
		}
		if sameActionSet(p.Actions, want.actions) {
			return true
		}
	}
	return false
}

func normResourceType(rt string) string {
	if rt == "" {
		return model.ResourceFS
	}
	return rt
}

// canonicalResourcePath mirrors op.normalizePermissionResource so the seeded
// row's stored Path matches what a subsequent existence check builds from want.
func canonicalResourcePath(rt, path string) string {
	if normResourceType(rt) == model.ResourceSystem {
		if dom, ok := op.CanonicalDomain(path); ok {
			return dom
		}
		return path
	}
	return utils.FixAndCleanPath(path)
}

// sameActionSet reports order-independent equality of two action collections.
func sameActionSet(a model.Actions, b []model.Action) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[model.Action]int, len(a))
	for _, x := range a {
		set[x]++
	}
	for _, y := range b {
		set[y]--
		if set[y] < 0 {
			return false
		}
	}
	return true
}
