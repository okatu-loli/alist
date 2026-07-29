package handles

import (
	"fmt"
	"strconv"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// callerIsAdmin reports whether the request's authenticated user is an admin.
// It fails closed: a missing/typeless user is treated as non-admin, so the
// handler-layer control-plane gates below (spec §5) never fall open if a route
// somehow reached a handler without the auth middleware having run.
func callerIsAdmin(c *gin.Context) bool {
	v, ok := c.Get(ctxkey.User)
	if !ok {
		return false
	}
	u, ok := v.(*model.User)
	return ok && u.IsAdmin()
}

// ListPermissions returns a paginated list of permission rows.
// @Summary List permissions
// @Description Returns a paginated list of permission rows.
// @Tags admin-permission
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param page query int false "page number"
// @Param per_page query int false "page size"
// @Success 200 {object} common.Resp[common.PageResp] "data.content is []model.Permission"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/permission/list [get]
// @Router /v4/admin/permissions [get]
func ListPermissions(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	ps, total, err := op.GetPermissions(req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{Content: ps, Total: total})
}

// GetPermission returns a single permission row by ID.
// @Summary Get permission
// @Description Returns a single permission row by ID.
// @Tags admin-permission
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "permission id"
// @Success 200 {object} common.Resp[model.Permission]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/permission/get [get]
// @Param id path string true "permission id"
// @Router /v4/admin/permissions/{id} [get]
func GetPermission(c *gin.Context) {
	id, err := strconv.Atoi(pathOrQuery(c, "id"))
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	p, err := op.GetPermission(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, p)
}

// CreatePermission creates a new manually-authored permission row.
// @Summary Create permission
// @Description Creates a new permission row. Origin is always forced to "manual" server-side, since "scope"-origin rows are exclusively produced by role sync.
// @Tags admin-permission
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.Permission true "permission to create"
// @Success 200 {object} common.Resp[model.Permission]
// @Failure 400 {object} common.Resp[any] "invalid body, or effect not allow/deny"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/permission/create [post]
// @Router /v4/admin/permissions [post]
func CreatePermission(c *gin.Context) {
	var p model.Permission
	if err := c.ShouldBind(&p); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if p.Effect != "" && p.Effect != model.EffectAllow && p.Effect != model.EffectDeny {
		common.ErrorI18n(c, "permission.effect_must_be_allow_or_deny", 400)
		return
	}
	// Control-plane isolation (spec §5.1): writing a system-type permission row
	// is authorization-graph mutation and is IsAdmin-only. The route already
	// gates this with RequireAdmin; this is defense-in-depth against a
	// mis-mapped route.
	if p.ResourceType == model.ResourceSystem && !callerIsAdmin(c) {
		common.ErrorI18n(c, "permission.system_admin_only", 403)
		return
	}
	// Origin is system-managed: scope-origin rows are only ever produced by
	// role sync, so a client-labeled "scope" row must be forced to manual or
	// the next role sync would silently wipe it as stale.
	p.Origin = model.OriginManual
	if err := op.CreatePermission(&p); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	auditControlPlane(c, "permission.create", fmt.Sprintf("permission:%d", p.ID))
	common.SuccessResp(c, p)
}

type UpdatePermissionReq struct {
	ID          uint          `json:"id" binding:"required"`
	Name        string        `json:"name"`
	DisplayName string        `json:"display_name"`
	Path        string        `json:"path"`
	Effect      string        `json:"effect"`
	Actions     model.Actions `json:"actions"`
	Recursive   *bool         `json:"recursive"`
}

// UpdatePermission updates fields of an existing permission row.
// @Summary Update permission
// @Description Updates an existing permission row (matched by ID in the body). Only non-empty/non-nil fields are applied. Origin is not client-editable.
// @Tags admin-permission
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body UpdatePermissionReq true "permission fields to update, including id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid body, or effect not allow/deny"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/permission/update [post]
// @Param id path string true "permission id"
// @Router /v4/admin/permissions/{id} [put]
func UpdatePermission(c *gin.Context) {
	var req UpdatePermissionReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.Effect != "" && req.Effect != model.EffectAllow && req.Effect != model.EffectDeny {
		common.ErrorI18n(c, "permission.effect_must_be_allow_or_deny", 400)
		return
	}
	p, err := op.GetPermission(req.ID)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	// Control-plane isolation (spec §5.1): mutating a system-type row is
	// IsAdmin-only (defense-in-depth behind the route's RequireAdmin).
	if p.ResourceType == model.ResourceSystem && !callerIsAdmin(c) {
		common.ErrorI18n(c, "permission.system_admin_only", 403)
		return
	}
	if req.Name != "" {
		p.Name = req.Name
	}
	if req.DisplayName != "" {
		p.DisplayName = req.DisplayName
	}
	if req.Path != "" {
		p.Path = req.Path
	}
	if req.Effect != "" {
		p.Effect = req.Effect
	}
	if req.Actions != nil {
		p.Actions = req.Actions
	}
	if req.Recursive != nil {
		p.Recursive = *req.Recursive
	}
	// Origin is intentionally not merged from the request: it marks whether a
	// row was hand-authored or generated from a role's PermissionScopes, and
	// is not client-editable via this API.
	if err := op.UpdatePermission(p); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	auditControlPlane(c, "permission.update", fmt.Sprintf("permission:%d", p.ID))
	common.SuccessResp(c)
}

// DeletePermission deletes a permission row by ID.
// @Summary Delete permission
// @Description Deletes a permission row by ID.
// @Tags admin-permission
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "permission id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/permission/delete [post]
// @Param id path string true "permission id"
// @Router /v4/admin/permissions/{id} [delete]
func DeletePermission(c *gin.Context) {
	id, err := strconv.Atoi(pathOrQuery(c, "id"))
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// Control-plane isolation (spec §5.1): deleting a system-type row is
	// IsAdmin-only (defense-in-depth behind the route's RequireAdmin). Load the
	// row first so a non-admin cannot destroy a system authorization grant.
	if p, err := op.GetPermission(uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	} else if p.ResourceType == model.ResourceSystem && !callerIsAdmin(c) {
		common.ErrorI18n(c, "permission.system_admin_only", 403)
		return
	}
	if err := op.DeletePermission(uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	auditControlPlane(c, "permission.delete", fmt.Sprintf("permission:%d", id))
	common.SuccessResp(c)
}

// PermissionRoles returns which roles reference the permission (reverse query).
// @Summary Permission roles
// @Description Returns the IDs of roles whose permission_scopes reference the given permission (reverse lookup).
// @Tags admin-permission
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "permission id"
// @Success 200 {object} common.Resp[[]uint]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/permission/roles [get]
// @Param id path string true "permission id"
// @Router /v4/admin/permissions/{id}/roles [get]
func PermissionRoles(c *gin.Context) {
	id, err := strconv.Atoi(pathOrQuery(c, "id"))
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	rids, err := op.GetRoleIDsByPermission(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, rids)
}

// RolePermissions returns every permission row bound to a role (scope +
// manual alike), the forward counterpart to PermissionRoles' reverse lookup.
// Each row carries its Origin so the frontend can tell a read-only
// scope-derived row from an editable manual one.
// @Summary Role permissions
// @Description Returns the permission rows bound to the given role (scope + manual).
// @Tags admin-role
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "role id"
// @Success 200 {object} common.Resp[[]model.Permission]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/role/permissions [get]
// @Param id path string true "role id"
// @Router /v4/admin/roles/{id}/permissions [get]
func RolePermissions(c *gin.Context) {
	id, err := strconv.Atoi(pathOrQuery(c, "id"))
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	ps, err := op.GetPermissionsByRole(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, ps)
}

type SetRolePermissionsReq struct {
	RoleID        uint   `json:"role_id" binding:"required"`
	PermissionIDs []uint `json:"permission_ids"`
}

// SetRolePermissions replaces the set of permission rows associated with a role.
// @Summary Set role permissions
// @Description Replaces the set of permission IDs associated with the given role.
// @Tags admin-role
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body SetRolePermissionsReq true "role id and permission ids to associate"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/role/set_permissions [post]
// @Param id path string true "role id"
// @Router /v4/admin/roles/{id}/permissions [put]
func SetRolePermissions(c *gin.Context) {
	var req SetRolePermissionsReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.SetRolePermissions(req.RoleID, req.PermissionIDs); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	auditControlPlane(c, "role.set_permissions", fmt.Sprintf("role:%d", req.RoleID))
	common.SuccessResp(c)
}
