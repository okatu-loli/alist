package middlewares

import (
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/gin-gonic/gin"
)

// AuditContext stamps the request metadata the audit trail records (client
// IP, entry protocol) onto the gin context. internal/audit reads them back
// through context.Value — it must not import gin (Step 8's decoupling rule),
// so the handoff is by context key.
func AuditContext(protocol string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(audit.CtxIPKey, c.ClientIP())
		c.Set(audit.CtxProtocolKey, protocol)
		c.Next()
	}
}
