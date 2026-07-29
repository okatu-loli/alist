package middlewares

import (
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// I18n negotiates the response locale for the request and stores it on the
// context for the envelope renderer (common.ErrorResp). It reads the
// Accept-Language header and, when Auth has already run and a user is present,
// that user's explicit User.Language preference (which wins over the header).
// Anonymous requests fall back to the header alone.
func I18n(c *gin.Context) {
	var userPref string
	if v, ok := c.Get(ctxkey.User); ok {
		if u, ok := v.(*model.User); ok {
			userPref = u.Language
		}
	}
	c.Set(ctxkey.Lang, i18n.Negotiate(c.GetHeader("Accept-Language"), userPref))
	c.Next()
}
