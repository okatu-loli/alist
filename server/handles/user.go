package handles

import (
	"strconv"

	"github.com/alist-org/alist/v3/pkg/utils"

	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// ListUsers returns a paginated list of users.
// @Summary List users
// @Description Returns a paginated list of users.
// @Tags admin-user
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param page query int false "page number"
// @Param per_page query int false "page size"
// @Success 200 {object} common.Resp[common.PageResp] "data.content is []model.User"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/user/list [get]
// @Router /v4/admin/users [get]
func ListUsers(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	log.Debugf("%+v", req)
	users, total, err := op.GetUsers(req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{
		Content: users,
		Total:   total,
	})
}

// CreateUser creates a new user.
// @Summary Create user
// @Description Creates a new user. Cannot be used to create an admin or guest user; if no role is given, the default role is assigned.
// @Tags admin-user
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.User true "user to create"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid body, or attempted to create admin/guest user"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/user/create [post]
// @Router /v4/admin/users [post]
func CreateUser(c *gin.Context) {
	var req model.User
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	defaultRoleID := op.GetDefaultRoleID()
	if len(req.Role) == 0 {
		req.Role = model.Roles{defaultRoleID}
	}
	if req.IsAdmin() || req.IsGuest() {
		common.ErrorResp(c, i18n.Error(i18n.Lang(c), "user.admin_or_guest_cannot_be_created", nil), 400, true)
		return
	}
	// Control-plane isolation (spec §5.3, decision A): assigning a role binding
	// is IsAdmin-only. A user-manager operator may run user lifecycle but must
	// only ever bind the default role — letting a non-admin pick an arbitrary
	// privileged role (e.g. editor / storage-manager) is the self-escalation
	// vector UpdateUser's role-binding gate already closes; the CREATE twin
	// closes it here. Empty role was defaulted above, so it passes freely.
	if !callerIsAdmin(c) && !utils.SliceEqual(req.Role, model.Roles{defaultRoleID}) {
		common.ErrorI18n(c, "user.role_assign_admin_only", 403)
		return
	}
	req.SetPassword(req.Password)
	req.Password = ""
	req.Authn = "[]"
	if err := op.CreateUser(&req); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		common.SuccessResp(c)
	}
}

// UpdateUser updates an existing user.
// @Summary Update user
// @Description Updates an existing user (matched by ID in the body). The admin user's role cannot be changed, and demoting the last enabled admin is rejected.
// @Tags admin-user
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.User true "user to update, including id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid body, or attempted to assign admin/guest role"
// @Failure 403 {object} common.Resp[any] "cannot change role of admin user, or would leave zero enabled admins"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/user/update [post]
// @Param id path string true "user id"
// @Router /v4/admin/users/{id} [put]
func UpdateUser(c *gin.Context) {
	var req model.User
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, err := op.GetUserById(req.ID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	// Control-plane isolation (spec §5.3, decision A): assigning/removing a
	// user's role binding is IsAdmin-only. A user-manager operator does user
	// lifecycle (create/disable/profile/password/delete non-admin) but must
	// never rewrite role bindings — that would let them self-escalate. The
	// route gates user/update at system.user.manage; this handler gate keeps
	// the role-binding change itself admin-only regardless.
	if !utils.SliceEqual(user.Role, req.Role) && !callerIsAdmin(c) {
		common.ErrorI18n(c, "user.role_change_admin_only", 403)
		return
	}

	if user.Username == "admin" {
		if !utils.SliceEqual(user.Role, req.Role) {
			common.ErrorI18n(c, "user.cannot_change_admin_role", 403)
			return
		}
		//if user.Username != req.Username {
		//	common.ErrorStrResp(c, "cannot change username of admin user", 403)
		//	return
		//}
	}

	if req.Password == "" {
		req.PwdHash = user.PwdHash
		req.Salt = user.Salt
	} else {
		req.SetPassword(req.Password)
		req.Password = ""
	}
	if req.OtpSecret == "" {
		req.OtpSecret = user.OtpSecret
	}
	// Last-admin lockout guard (spec §7): if the target is currently an enabled
	// admin, refuse any change that would strip its adminness — disabling it or
	// removing the ADMIN role — when it is the last enabled admin left.
	if user.IsAdmin() && !user.Disabled && (req.Disabled || !req.IsAdmin()) {
		count, err := op.CountEnabledAdminsExcluding(user.ID)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		if count == 0 {
			common.ErrorI18n(c, "user.last_admin_required", 400)
			return
		}
	}

	if !utils.SliceEqual(user.Role, req.Role) {
		if req.IsAdmin() || req.IsGuest() {
			common.ErrorResp(c, i18n.Error(i18n.Lang(c), "user.cannot_assign_admin_or_guest_role", nil), 400, true)
			return
		}
	}

	if err := op.UpdateUser(&req); err != nil {
		common.ErrorResp(c, err, 500)
	} else {
		common.SuccessResp(c)
	}
}

// DeleteUser deletes a user by ID.
// @Summary Delete user
// @Description Deletes a user by ID.
// @Tags admin-user
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "user id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/user/delete [post]
// @Param id path string true "user id"
// @Router /v4/admin/users/{id} [delete]
func DeleteUser(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, err := op.GetUserById(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// The built-in "admin" account is structurally undeletable, and the last
	// enabled admin must never be removed (spec §7). op.DeleteUserById enforces
	// the same invariant as an unbypassable backstop; these give clean errors.
	if user.Username == "admin" {
		common.ErrorI18n(c, "user.cannot_delete_admin", 403)
		return
	}
	if user.IsAdmin() && !user.Disabled {
		count, err := op.CountEnabledAdminsExcluding(user.ID)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		if count == 0 {
			common.ErrorI18n(c, "user.last_admin_required", 400)
			return
		}
	}
	if err := op.DeleteUserById(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// GetUser returns a single user by ID.
// @Summary Get user
// @Description Returns a single user by ID.
// @Tags admin-user
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "user id"
// @Success 200 {object} common.Resp[model.User]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/user/get [get]
// @Param id path string true "user id"
// @Router /v4/admin/users/{id} [get]
func GetUser(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, err := op.GetUserById(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, user)
}

// DelUserCache invalidates the cached user entry for the given username.
// @Summary Delete user cache
// @Description Invalidates the cached user entry for the given username, forcing it to be reloaded from storage on next use.
// @Tags admin-user
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param username query string true "username"
// @Success 200 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/user/del_cache [post]
// @Param username path string true "username"
// @Router /v4/admin/user_cache/{username} [delete]
func DelUserCache(c *gin.Context) {
	username := pathOrQuery(c, "username")
	err := op.DelUserCache(username)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}
