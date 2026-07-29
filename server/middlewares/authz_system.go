package middlewares

import (
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// RequireSystem gates an admin control-plane route behind a (domain, verb)
// system permission, routed through the single op.Authorize decision entry
// (spec §2.5). It is layered on top of the /api/admin group's fail-closed
// AuthAdmin backstop, so it only ever refines an already-admin-gated surface:
// any route that forgets to add it still denies non-admins.
//
// Admins short-circuit (spec §5/§14): IsAdmin is the escape hatch that keeps
// the fail-closed backstop from locking real admins out, and it is the only
// bypass. Every non-admin must hold system.<domain>.<verb> (manage implies
// view, §6.1) in their effective policy or be denied 403.
func RequireSystem(domain, verb string) gin.HandlerFunc {
	act := model.SystemAction(domain, verb)
	return func(c *gin.Context) {
		user := c.MustGet(ctxkey.User).(*model.User)
		if user.IsAdmin() {
			// The short-circuit skips op.Authorize (and with it the audit hook
			// inside it), so record the admin's control-plane access here —
			// admin activity is exactly what an audit trail must not miss.
			audit.Decision(c, user, string(act), "system:"+domain, true)
			c.Next()
			return
		}
		if err := op.Authorize(c, user, act, op.AuthzRequest{
			ResourceType: model.ResourceSystem,
			Path:         domain,
		}); err != nil {
			common.ErrorResp(c, err, 403)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireAdmin gates control-plane routes that stay IsAdmin-only and are never
// delegable through the authz engine (spec §5): role/permission create/update/
// delete, role↔permission binding, and admin-token minting. It is the existing
// AuthAdmin behavior, named explicitly at those call sites so the IsAdmin-only
// intent reads directly in the router.
var RequireAdmin = AuthAdmin
