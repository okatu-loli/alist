package handles

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// App password self-service API (design W3). Protocol clients (WebDAV/FTP and
// friends) cannot do 2FA, so a logged-in user mints a per-client credential
// here instead of handing out their real password. The whole surface is
// self-service and self-scoped: every handler reads the caller off
// ctxkey.User and every op call is scoped to that caller's own id — there is
// no admin-manages-another-user's-app-passwords path (op.DeleteAppPassword has
// no admin bypass, matching the design doc).
//
// AppPasswordDTO deliberately carries neither Hash nor Salt (model.AppPassword
// already excludes them from JSON via `json:"-"`, this DTO is the belt to that
// suspenders) — the one and only place the plaintext credential is ever
// visible is CreateAppPasswordResp, and only once, at creation time.

// AppPasswordDTO is the wire form of a stored app password: enough to
// identify and manage it, never the credential material.
type AppPasswordDTO struct {
	ID        uint       `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

func toAppPasswordDTO(ap model.AppPassword) AppPasswordDTO {
	return AppPasswordDTO{
		ID:        ap.ID,
		Name:      ap.Name,
		CreatedAt: ap.CreatedAt,
		LastUsed:  ap.LastUsed,
	}
}

// ListAppPasswords returns every app password the caller owns.
// @Summary List my app passwords
// @Description Returns all protocol app passwords belonging to the authenticated user. Never includes credential material.
// @Tags account
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]AppPasswordDTO]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/me/app-passwords [get]
func ListAppPasswords(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	aps, err := op.ListAppPasswords(user.ID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dtos := make([]AppPasswordDTO, 0, len(aps))
	for _, ap := range aps {
		dtos = append(dtos, toAppPasswordDTO(ap))
	}
	common.SuccessResp(c, dtos)
}

// CreateAppPasswordReq is the create body: a caller-chosen label so the
// account page can later show which client each credential belongs to.
type CreateAppPasswordReq struct {
	Name string `json:"name" form:"name"`
}

// CreateAppPasswordResp carries the plaintext credential exactly once — the
// caller must copy it now, it is never retrievable again.
type CreateAppPasswordResp struct {
	AppPassword AppPasswordDTO `json:"app_password"`
	Plaintext   string         `json:"plaintext"`
}

// CreateAppPassword mints a new app password for the caller.
// @Summary Create an app password
// @Description Mints a new protocol app password for the authenticated user. The plaintext credential is returned once and never stored or retrievable again.
// @Tags account
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body CreateAppPasswordReq true "app password name"
// @Success 200 {object} common.Resp[CreateAppPasswordResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/me/app-passwords [post]
func CreateAppPassword(c *gin.Context) {
	var req CreateAppPasswordReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	plaintext, ap, err := op.CreateAppPassword(user.ID, req.Name)
	if err != nil {
		code := 500
		if errors.Is(err, errs.AppPasswordLimitExceeded) {
			code = 400
		}
		audit.Event(c, user.ID, user.Username, "account.app_password.create", "app_password:new", false)
		common.ErrorResp(c, err, code)
		return
	}
	audit.Event(c, user.ID, user.Username, "account.app_password.create", fmt.Sprintf("app_password:%d", ap.ID), true)
	common.SuccessResp(c, CreateAppPasswordResp{
		AppPassword: toAppPasswordDTO(ap),
		Plaintext:   plaintext,
	})
}

// DeleteAppPassword revokes one of the caller's own app passwords. An id that
// does not exist or belongs to a different user is a uniform 404 — the same
// AppPasswordNotFound the op layer returns for both cases (enumeration
// defense) — and the row is never touched in either case.
// @Summary Delete an app password
// @Description Revokes one of the authenticated user's own app passwords. An unknown id or another user's id is a 404 and never deletes anything.
// @Tags account
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path int true "app password id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/me/app-passwords/{id} [delete]
func DeleteAppPassword(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	if err := op.DeleteAppPassword(user.ID, uint(id)); err != nil {
		code := 500
		if errors.Is(err, errs.AppPasswordNotFound) {
			code = 404
		}
		audit.Event(c, user.ID, user.Username, "account.app_password.delete", fmt.Sprintf("app_password:%d", id), false)
		common.ErrorResp(c, err, code)
		return
	}
	audit.Event(c, user.ID, user.Username, "account.app_password.delete", fmt.Sprintf("app_password:%d", id), true)
	common.SuccessResp(c)
}
