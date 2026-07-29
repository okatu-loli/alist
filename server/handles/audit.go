package handles

import (
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

type ListAuditLogsReq struct {
	model.PageReq
	ActorUID uint   `form:"actor_uid"`
	Action   string `form:"action"` // dotted-prefix match: "fs." matches fs.read…
	Protocol string `form:"protocol"`
	Result   string `form:"result"` // allow|deny|success|failure
	From     string `form:"from"`   // RFC 3339
	To       string `form:"to"`     // RFC 3339
}

// ListAuditLogs pages through the audit trail, newest first.
// @Summary List audit logs
// @Description Query the audit trail (authorization decisions and security events), newest first. Admin only.
// @Tags admin_audit
// @Security ApiKeyAuth
// @Produce json
// @Param page query int false "page number"
// @Param per_page query int false "page size"
// @Param actor_uid query int false "filter by acting user id"
// @Param action query string false "dotted action prefix, e.g. fs. or system.user."
// @Param protocol query string false "entry protocol, e.g. http"
// @Param result query string false "allow / deny / success / failure"
// @Param from query string false "RFC 3339 lower bound"
// @Param to query string false "RFC 3339 upper bound"
// @Success 200 {object} common.Resp[common.PageResp]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/audit [get]
func ListAuditLogs(c *gin.Context) {
	var req ListAuditLogsReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	filter := db.AuditLogFilter{
		ActorUID:     req.ActorUID,
		ActionPrefix: req.Action,
		Protocol:     req.Protocol,
		Result:       req.Result,
	}
	if req.From != "" {
		ts, err := time.Parse(time.RFC3339, req.From)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		filter.From = ts
	}
	if req.To != "" {
		ts, err := time.Parse(time.RFC3339, req.To)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		filter.To = ts
	}
	logs, total, err := db.GetAuditLogs(filter, req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{Content: logs, Total: total})
}
