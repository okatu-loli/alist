package handles

import (
	"fmt"
	"strconv"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// This file is the ADMIN recovery path for a user's TOTP second factor
// (Phase C red-team C1): a locked-out user who lost both their authenticator
// AND every recovery code has no self-service way back in (TOTPDelete always
// requires proof of possession — see totp.go). An admin can force-clear the
// factor with NO proof required, so this is deliberately gated on the
// non-delegable RequireAdmin (not RequireSystem("user","manage"), which the
// sibling /admin/users/:id CRUD routes use) and always audited, success or
// failure.

// AdminCancel2FA force-clears a user's TOTP second factor: OtpSecret is
// cleared and every recovery code is deleted, admin-only, audited.
// @Summary Admin: reset a user's TOTP second factor
// @Description Clears the target user's OtpSecret and every recovery code with no proof of possession required (admin recovery path for a user who lost both their authenticator and their recovery codes). Admin-only, audited.
// @Tags admin-user
// @Produce json
// @Security ApiKeyAuth
// @Param id path int true "user id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/users/{id}/cancel_2fa [post]
func AdminCancel2FA(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	target, err := op.GetUserById(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	admin := c.MustGet(ctxkey.User).(*model.User)
	targetRef := fmt.Sprintf("user:%d", target.ID)

	if err := op.Cancel2FAById(target.ID); err != nil {
		audit.Event(c, admin.ID, admin.Username, "admin.user.cancel_2fa", targetRef, false)
		common.ErrorResp(c, err, 500)
		return
	}
	if err := op.DeleteRecoveryCodes(target.ID); err != nil {
		audit.Event(c, admin.ID, admin.Username, "admin.user.cancel_2fa", targetRef, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, admin.ID, admin.Username, "admin.user.cancel_2fa", targetRef, true)
	common.SuccessResp(c)
}
