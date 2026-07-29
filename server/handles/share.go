package handles

import (
	"errors"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Share API (design §2.3-A). The authed surface — create/list/delete — lets a
// logged-in user mint public share links for paths they can read, list their
// own links, and revoke them. Two authorization rules are load-bearing here:
//
//   - Create is gated on ActRead over the target path (op.Authorize). A share
//     delegates the creator's read right to anyone holding the token, so a
//     caller who cannot read a path must not be able to hand that read out.
//     No read, no share (403).
//   - List/Delete are confined to the creator (delete also allows an admin).
//     One user must never see or revoke another user's shares.
//
// The public, unauthenticated access surface (/s/:token info/list/download) is a
// separate file — this one never runs without a logged-in caller in context.

// CreateShareReq is the create-share body: the path to share plus an optional
// password and expiry (hours; <=0 means never).
type CreateShareReq struct {
	Path        string `json:"path" form:"path"`
	Password    string `json:"password" form:"password"`
	ExpireHours int    `json:"expire_hours" form:"expire_hours"`
}

// ShareDTO is the wire form of a share for its creator. It deliberately carries
// NO PasswordHash: needs_password exposes only the single bit of whether a
// password is set, never the salted hash itself. Token IS included — it is the
// bearer credential the creator needs to build/share the link (like a signed
// URL), and this surface is already gated to the creator's own shares.
type ShareDTO struct {
	ID            uint       `json:"id"`
	Token         string     `json:"token"`
	URL           string     `json:"url"`
	ExpiresAt     *time.Time `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
	NeedsPassword bool       `json:"needs_password"`
	Disabled      bool       `json:"disabled"`
}

// toShareDTO projects a stored Share onto the creator-facing DTO, dropping the
// PasswordHash and deriving needs_password + the public URL.
func toShareDTO(s *model.Share) ShareDTO {
	return ShareDTO{
		ID:            s.ID,
		Token:         s.Token,
		URL:           "/s/" + s.Token,
		ExpiresAt:     s.ExpiresAt,
		CreatedAt:     s.CreatedAt,
		NeedsPassword: s.PasswordHash != "",
		Disabled:      s.Disabled,
	}
}

// CreateShare mints a public share for a path the caller can read. The order is
// deliberate: resolve the caller-scoped path (JoinPath), then gate on ActRead
// (no read permission, no share — 403), and only then anchor + persist in the op
// layer. The response carries the token and its public /s/<token> URL.
// @Summary Create share
// @Description Mints a public share link for a path the caller can read. Requires ActRead on the target path — a share delegates the creator's read right, so no read permission means no share.
// @Tags shares
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body CreateShareReq true "path + optional password/expiry"
// @Success 200 {object} common.Resp[ShareDTO]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "no read permission on path"
// @Failure 500 {object} common.Resp[any]
// @Router /v4/shares [post]
func CreateShare(c *gin.Context) {
	var req CreateShareReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// Read permission is the whole gate: a share delegates the creator's read
	// right, so a caller who cannot read the path must not be able to share it.
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: reqPath}); err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	s, err := op.CreateShare(c, user.ID, reqPath, req.Password, req.ExpireHours)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, toShareDTO(s))
}

// ListShares returns every share the caller created (creator isolation: only
// your own), projected to the PasswordHash-free DTO.
// @Summary List shares
// @Description Returns every share the caller created (creator isolation: only your own).
// @Tags shares
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[[]ShareDTO]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/shares [get]
func ListShares(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	shares, err := db.ListSharesByCreator(user.ID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dtos := make([]ShareDTO, 0, len(shares))
	for i := range shares {
		dtos = append(dtos, toShareDTO(&shares[i]))
	}
	common.SuccessResp(c, dtos)
}

// DeleteShare revokes a share by id. Ownership is enforced here: a caller may
// only delete their own share unless they are an admin — deleting another user's
// share is a 403. The id comes from the :id path param (like LabelDelete).
// @Summary Delete share
// @Description Revokes a share by id. A caller may only delete their own share unless they are an admin.
// @Tags shares
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "share id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "not the share's creator"
// @Failure 404 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/shares/{id} [delete]
func DeleteShare(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	s, err := db.GetShareByID(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	if s.CreatorID != user.ID && !user.IsAdmin() {
		common.ErrorResp(c, errors.New("you may only delete your own shares"), 403)
		return
	}
	if err := db.DeleteShare(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}
