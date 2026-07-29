package middlewares

import (
	"net/url"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

func FsUp(c *gin.Context) {
	path := c.GetHeader("File-Path")
	password := c.GetHeader("Password")
	path, err := url.PathUnescape(path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		c.Abort()
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	path, err = user.JoinPath(path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: path, Password: password}); err != nil {
		common.AuthzErrorResp(c, err, "password is incorrect or you have no permission")
		return
	}
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: path}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	c.Next()
}
