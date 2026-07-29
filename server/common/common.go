package common

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func hidePrivacy(msg string) string {
	for _, r := range conf.PrivacyReg {
		msg = r.ReplaceAllStringFunc(msg, func(s string) string {
			return strings.Repeat("*", len(s))
		})
	}
	return msg
}

// ErrorResp is used to return error response
// param l: if true, log error
func ErrorResp(c *gin.Context, err error, code int, l ...bool) {
	ErrorWithDataResp(c, err, code, nil, l...)
	//if len(l) > 0 && l[0] {
	//	if flags.Debug || flags.Dev {
	//		log.Errorf("%+v", err)
	//	} else {
	//		log.Errorf("%v", err)
	//	}
	//}
	//c.JSON(200, Resp[interface{}]{
	//	Code:    code,
	//	Message: hidePrivacy(err.Error()),
	//	Data:    nil,
	//})
	//c.Abort()
}

func ErrorWithDataResp(c *gin.Context, err error, code int, data interface{}, l ...bool) {
	if len(l) > 0 && l[0] {
		if flags.Debug || flags.Dev {
			log.Errorf("%+v", err)
		} else {
			log.Errorf("%v", err)
		}
	}
	c.JSON(200, Resp[interface{}]{
		Version: RespVersion(c),
		Code:    code,
		Message: hidePrivacy(i18n.Render(i18n.Lang(c), err)),
		Data:    data,
	})
	c.Abort()
}

func ErrorStrResp(c *gin.Context, str string, code int, l ...bool) {
	if len(l) != 0 && l[0] {
		log.Error(str)
	}
	c.JSON(200, Resp[interface{}]{
		Version: RespVersion(c),
		Code:    code,
		Message: hidePrivacy(str),
		Data:    nil,
	})
	c.Abort()
}

// ErrorI18n returns a keyed error response rendered in the request's
// negotiated locale. key is a catalog key (e.g. "user.role_assign_admin_only")
// and args are optional "{name}"-placeholder name/value pairs. It is the
// call-site helper for user-facing handler messages: the literal string is
// replaced by a key that the envelope renders per Accept-Language / User.Language.
func ErrorI18n(c *gin.Context, key string, code int, args ...any) {
	ErrorResp(c, i18n.Error(i18n.Lang(c), key, nil, args...), code)
}

// AuthzErrorResp maps an op.Authorize error to HTTP: policy/password denials
// stay 403 with the caller's message (frontend matches on it), anything else
// is an engine-internal failure and must surface as 500, not masquerade as
// a permission problem.
func AuthzErrorResp(c *gin.Context, err error, msg403 string) {
	if errors.Is(err, errs.PermissionDenied) || errors.Is(err, errs.WrongPassword) {
		ErrorStrResp(c, msg403, 403)
		return
	}
	ErrorResp(c, err, 500, true)
}

func SuccessResp(c *gin.Context, data ...interface{}) {
	SuccessWithMsgResp(c, "success", data...)
}

func SuccessWithMsgResp(c *gin.Context, msg string, data ...interface{}) {
	var respData interface{}
	if len(data) > 0 {
		respData = data[0]
	}

	c.JSON(200, Resp[interface{}]{
		Version: RespVersion(c),
		Code:    200,
		Message: msg,
		Data:    respData,
	})
}

func Pluralize(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func GetHttpReq(ctx context.Context) *http.Request {
	if c, ok := ctx.(*gin.Context); ok {
		return c.Request
	}
	return nil
}
