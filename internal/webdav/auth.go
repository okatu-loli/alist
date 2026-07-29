package webdav

import (
	"net/http"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/authlimit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// authRealm is sent on every 401 so a WebDAV client's Basic Auth prompt
// identifies itself; it also doubles as the uniform failure signal (design
// W5 §①: "统一失败 401 + WWW-Authenticate ... 防枚举" -- every miss shape
// below produces this exact response, never a different one).
const authRealm = `Basic realm="AList WebDAV"`

// webdavAuthAction is the audit.Event action name for every WebDAV Basic
// Auth attempt (success and failure alike), distinct from the web login's
// "auth.login" and the protocol-plugin bridge's "plugin.auth.login" so an
// operator can tell them apart in the audit log.
const webdavAuthAction = "webdav.auth"

// authThrottle rate-limits WebDAV Basic Auth per client IP. Unlike the
// reverse-gRPC protocol login path (internal/extension/hostcap), an inbound
// WebDAV request carries a real client IP (design W5 §①: "这里有真实 client
// IP"), so this keys on it directly rather than pluginID+username.
var authThrottle = authlimit.NewThrottle()

// verifyCredentials resolves (username, password) to the owning, usable
// *model.User for WebDAV Basic Auth. Order (design W5 §①): an app password
// is tried first -- a hit is used immediately, regardless of whether the
// account has 2FA enabled, since an app password IS the second-factor-free
// credential by design (W3). On a miss, a raw password is accepted ONLY for
// a non-2FA account, mirroring hostcap.Login's ErrNeedAppPassword rule: a
// WebDAV client cannot enter a TOTP code, so a 2FA account must present an
// app password instead of ever being allowed to try its raw password here.
//
// Every failure shape -- unknown username, disabled account, guest account,
// wrong app password, a 2FA account presenting any raw password, wrong raw
// password -- collapses to the same errs.PermissionDenied so a client can
// never enumerate which case it hit (same anti-enumeration contract as
// op.VerifyAppPassword itself).
func verifyCredentials(username, password string) (*model.User, error) {
	if user, err := op.VerifyAppPassword(username, password); err == nil {
		return user, nil
	}

	user, err := db.GetUserByName(username)
	if err != nil {
		return nil, errs.PermissionDenied
	}
	if user.Disabled || user.IsGuest() {
		return nil, errs.PermissionDenied
	}
	if user.OtpSecret != "" {
		// 2FA account: a raw password is refused outright, not merely
		// throttled as a failed attempt -- the password itself was never
		// wrong, the protocol just cannot carry a second factor.
		return nil, errs.PermissionDenied
	}
	if err := user.ValidateRawPassword(password); err != nil {
		return nil, errs.PermissionDenied
	}
	return user, nil
}

func unauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", authRealm)
	c.AbortWithStatus(http.StatusUnauthorized)
}

// BasicAuth is the gin middleware every /dav request runs behind, after the
// builtin.webdav extension gate (server/router.go). It resolves a real
// *model.User from the request's HTTP Basic credentials and stores it via
// ctxkey.User on the gin context, which the webdav.FileSystem adapter
// (fs.go) and Handler (handler.go) read back from the request context
// (gin.Context itself implements context.Context, so a value set here via
// c.Set is visible through c.Value/ctx.Value all the way down).
//
// It also stamps audit.CtxIPKey/CtxProtocolKey so both this middleware's own
// audit.Event calls AND every downstream op.Authorize decision (which
// audits itself, internal/op/authz.go) carry the real client IP and a
// "webdav" protocol tag instead of the blank values they'd get without a
// server/middlewares.AuditContext-equivalent on this route.
func BasicAuth(c *gin.Context) {
	ip := c.ClientIP()
	c.Set(audit.CtxIPKey, ip)
	c.Set(audit.CtxProtocolKey, "webdav")
	target := "webdav:" + ip

	username, password, ok := c.Request.BasicAuth()
	if !ok {
		unauthorized(c)
		return
	}

	if authThrottle.Blocked(ip) {
		audit.Event(c, 0, username, webdavAuthAction, target, false)
		unauthorized(c)
		return
	}

	user, err := verifyCredentials(username, password)
	if err != nil {
		authThrottle.Fail(ip)
		audit.Event(c, 0, username, webdavAuthAction, target, false)
		unauthorized(c)
		return
	}

	authThrottle.Reset(ip)
	audit.Event(c, user.ID, user.Username, webdavAuthAction, target, true)
	c.Set(ctxkey.User, user)
	c.Next()
}
