package handles

import (
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/session"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

type SessionResp struct {
	SessionID  string `json:"session_id"`
	UserID     uint   `json:"user_id,omitempty"`
	LastActive int64  `json:"last_active"`
	Status     int    `json:"status"`
	UA         string `json:"ua"`
	IP         string `json:"ip"`
}

// ListMySessions lists all device sessions belonging to the current user.
// @Summary List my sessions
// @Description Returns all device sessions (active and inactive) belonging to the authenticated user.
// @Tags auth
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]SessionResp]
// @Failure 500 {object} common.Resp[any]
// @Router /me/sessions [get]
// @Router /v4/me/sessions [get]
func ListMySessions(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	sessions, err := db.ListSessionsByUser(user.ID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	resp := make([]SessionResp, len(sessions))
	for i, s := range sessions {
		resp[i] = SessionResp{
			SessionID:  s.DeviceKey,
			LastActive: s.LastActive,
			Status:     s.Status,
			UA:         s.UserAgent,
			IP:         s.IP,
		}
	}
	common.SuccessResp(c, resp)
}

type EvictSessionReq struct {
	SessionID string `json:"session_id"`
}

// EvictMySession marks one of the current user's own device sessions inactive.
// @Summary Evict my session
// @Description Marks the given session (must belong to the authenticated user) inactive, forcing that device to re-authenticate.
// @Tags auth
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body EvictSessionReq true "session id to evict"
// @Param id path string true "session id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /me/sessions/evict [post]
// @Router /v4/me/sessions/{id} [delete]
func EvictMySession(c *gin.Context) {
	var req EvictSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	if _, err := db.GetSession(user.ID, req.SessionID); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := session.MarkInactive(req.SessionID); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "session.evict", "session:"+req.SessionID, true)
	common.SuccessResp(c)
}

// ListSessions lists all device sessions for all users (admin only).
// @Summary List all sessions (admin)
// @Description Returns all device sessions across all users.
// @Tags admin-session
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]SessionResp]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/session/list [get]
// @Router /v4/admin/sessions [get]
func ListSessions(c *gin.Context) {
	sessions, err := db.ListSessions()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	resp := make([]SessionResp, len(sessions))
	for i, s := range sessions {
		resp[i] = SessionResp{
			SessionID:  s.DeviceKey,
			UserID:     s.UserID,
			LastActive: s.LastActive,
			Status:     s.Status,
			UA:         s.UserAgent,
			IP:         s.IP,
		}
	}
	common.SuccessResp(c, resp)
}

// EvictSession marks any user's device session inactive (admin only).
// @Summary Evict session (admin)
// @Description Marks the given session inactive regardless of owner, forcing that device to re-authenticate.
// @Tags admin-session
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body EvictSessionReq true "session id to evict"
// @Param id path string true "session id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/session/evict [post]
// @Router /v4/admin/sessions/{id} [delete]
func EvictSession(c *gin.Context) {
	var req EvictSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := session.MarkInactive(req.SessionID); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	actor := c.MustGet(ctxkey.User).(*model.User)
	audit.Event(c, actor.ID, actor.Username, "session.evict", "session:"+req.SessionID, true)
	common.SuccessResp(c)
}
