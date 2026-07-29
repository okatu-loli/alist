package handles

import (
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// pathOrQuery reads a request-scoped identifier from the route's path
// parameter first, falling back to the query string. It is what lets the
// /api/v4 RESTful shapes (`/users/:id`) and the legacy /api query shapes
// (`/user/get?id=`) share one handler: v4 routes name the path param exactly
// like the legacy query key.
//
// A query-rewriting middleware can't do this job: gin caches the parsed query
// on first access, and the group-level Auth middleware reads a query key
// before route-level middlewares run, so later RawQuery rewrites are
// invisible to c.Query.
func pathOrQuery(c *gin.Context, key string) string {
	if v := c.Param(key); v != "" {
		return v
	}
	return c.Query(key)
}

// auditControlPlane records a control-plane mutation (role/permission CRUD,
// binding changes) as an audit event with the authenticated actor. These
// routes are RequireAdmin-gated (no op.Authorize call), so without this hook
// the audit trail would miss exactly the most privileged writes.
func auditControlPlane(c *gin.Context, action, targetRef string) {
	if u, ok := c.Get(ctxkey.User); ok {
		user := u.(*model.User)
		audit.Event(c, user.ID, user.Username, action, targetRef, true)
	}
}
