package handles

import (
	"fmt"
	"strconv"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// ListRoles returns a paginated list of roles.
// @Summary List roles
// @Description Returns a paginated list of roles.
// @Tags admin-role
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param page query int false "page number"
// @Param per_page query int false "page size"
// @Success 200 {object} common.Resp[common.PageResp] "data.content is []model.Role"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/role/list [get]
// @Router /v4/admin/roles [get]
func ListRoles(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	log.Debugf("%+v", req)
	roles, total, err := op.GetRoles(req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{Content: roles, Total: total})
}

// GetRole returns a single role by ID.
// @Summary Get role
// @Description Returns a single role by ID.
// @Tags admin-role
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "role id"
// @Success 200 {object} common.Resp[model.Role]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/role/get [get]
// @Param id path string true "role id"
// @Router /v4/admin/roles/{id} [get]
func GetRole(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	role, err := op.GetRole(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, role)
}

// CreateRole creates a new role.
// @Summary Create role
// @Description Creates a new role with the given name, description and permission scopes.
// @Tags admin-role
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.Role true "role to create"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/role/create [post]
// @Router /v4/admin/roles [post]
func CreateRole(c *gin.Context) {
	var req model.Role
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.CreateRole(&req); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		auditControlPlane(c, "role.create", fmt.Sprintf("role:%d", req.ID))
		common.SuccessResp(c)
	}
}

// UpdateRole updates an existing role's name, description, permission scopes and default flag.
// @Summary Update role
// @Description Updates an existing role (matched by ID in the body): name, description, permission_scopes and default flag. The built-in "admin" role cannot be renamed/modified; the built-in "guest" role's name cannot be changed.
// @Tags admin-role
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body object true "role update payload: {id, name, description, permission_scopes: []model.PermissionEntry, default}"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "cannot change the built-in admin role"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/role/update [post]
// @Param id path string true "role id"
// @Router /v4/admin/roles/{id} [put]
func UpdateRole(c *gin.Context) {
	var req struct {
		ID               uint                    `json:"id"`
		Name             string                  `json:"name" binding:"required"`
		Description      string                  `json:"description"`
		PermissionScopes []model.PermissionEntry `json:"permission_scopes"`
		Default          *bool                   `json:"default"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	role, err := op.GetRole(req.ID)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	// Protected roles (admin/guest and future presets) cannot be renamed
	// (spec §4.1); other fields may still be edited.
	if role.Protected && req.Name != role.Name {
		common.ErrorResp(c, errs.ErrChangeDefaultRole, 403)
		return
	}
	role.Name = req.Name
	role.Description = req.Description
	role.PermissionScopes = req.PermissionScopes
	if req.Default != nil {
		role.Default = *req.Default
	}
	if err := op.UpdateRole(role); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		auditControlPlane(c, "role.update", fmt.Sprintf("role:%d", role.ID))
		common.SuccessResp(c)
	}
}

// DeleteRole deletes a role by ID.
// @Summary Delete role
// @Description Deletes a role by ID. The built-in "admin" and "guest" roles cannot be deleted.
// @Tags admin-role
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "role id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "cannot delete built-in admin/guest role"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/role/delete [post]
// @Param id path string true "role id"
// @Router /v4/admin/roles/{id} [delete]
func DeleteRole(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	role, err := op.GetRole(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if role.Protected {
		common.ErrorResp(c, errs.ErrChangeDefaultRole, 403)
		return
	}
	if err := op.DeleteRole(uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	auditControlPlane(c, "role.delete", fmt.Sprintf("role:%d", id))
	common.SuccessResp(c)
}
