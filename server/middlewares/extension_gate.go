package middlewares

import (
	"errors"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// RequireExtensionEnabled is the real enable/disable boundary for a core feature
// turned built-in extension (Project A-2): offline download, file search,
// sharing, SSO login. It rejects a request with 403 when the feature's built-in
// entry is absent or disabled, so an admin turning the feature off in the
// extension manager actually stops its entry-point routes — a curl straight at
// the endpoint gets 403, not just a hidden UI button.
//
// Placement: on an authed route it sits AFTER the auth middleware and BEFORE the
// handler (the feature gate is orthogonal to identity); on a public route (the
// unauthenticated /s share surface) it is the first thing the request meets. It
// reads extension.DefaultRegistry.Get, which returns disabled entries too, so a
// disabled built-in resolves and is rejected rather than looking un-registered.
func RequireExtensionEnabled(id string) gin.HandlerFunc {
	return func(c *gin.Context) {
		e, ok := extension.DefaultRegistry.Get(id)
		if !ok || !e.Enabled() {
			common.ErrorResp(c, errors.New("feature disabled"), 403)
			c.Abort()
			return
		}
		c.Next()
	}
}
