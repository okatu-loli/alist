package middlewares

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/alist-org/alist/v3/internal/ctxkey"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/device"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Auth is a middleware that checks if the user is logged in.
// if token is empty, set user to guest
func Auth(c *gin.Context) {
	token := c.GetHeader("Authorization")
	if adminTokenMatches(token) {
		admin, err := op.GetAdmin()
		if err != nil {
			common.ErrorResp(c, err, 500)
			c.Abort()
			return
		}
		if !HandleSession(c, admin) {
			return
		}
		log.Debugf("use admin token: %+v", admin)
		c.Next()
		return
	}
	if token == "" {
		guest, err := op.GetGuest()
		if err != nil {
			common.ErrorResp(c, err, 500)
			c.Abort()
			return
		}
		if guest.Disabled {
			common.ErrorI18n(c, "auth.guest_disabled", 401)
			c.Abort()
			return
		}
		if len(guest.Role) > 0 {
			roles, err := op.GetRolesByUserID(guest.ID)
			if err != nil {
				common.ErrorStrResp(c, fmt.Sprintf("Fail to load guest roles: %v", err), 500)
				c.Abort()
				return
			}
			guest.RolesDetail = roles
		}
		if !HandleSession(c, guest) {
			return
		}
		log.Debugf("use empty token: %+v", guest)
		c.Next()
		return
	}
	// Extension scoped token (Step 10): a short-lived machine credential that
	// authenticates as a synthetic principal restricted to the declared storage
	// paths. Recognized before the normal bearer path and only for scoped-shaped
	// tokens; it carries no device session (it is not a login), so it skips
	// HandleSession and just becomes the request principal — op.Authorize then
	// confines it to scope.
	if extension.IsScopedToken(token) {
		user, err := extension.AuthenticateScopedToken(token)
		if err != nil {
			common.ErrorResp(c, err, 401)
			c.Abort()
			return
		}
		c.Set(ctxkey.User, user)
		log.Debugf("use extension scoped token: %+v", user)
		c.Next()
		return
	}
	userClaims, err := common.ParseToken(token)
	if err != nil {
		common.ErrorResp(c, err, 401)
		c.Abort()
		return
	}
	user, err := op.GetUserByName(userClaims.Username)
	if err != nil {
		common.ErrorResp(c, err, 401)
		c.Abort()
		return
	}
	// validate password timestamp
	if userClaims.PwdTS != user.PwdTS {
		common.ErrorI18n(c, "auth.password_changed", 401)
		c.Abort()
		return
	}
	if user.Disabled {
		common.ErrorI18n(c, "auth.current_user_disabled_relogin", 401)
		c.Abort()
		return
	}
	if len(user.Role) > 0 {
		roles, err := op.GetRolesByUserID(user.ID)
		if err != nil {
			common.ErrorStrResp(c, fmt.Sprintf("Fail to load roles: %v", err), 500)
			c.Abort()
			return
		}
		user.RolesDetail = roles
	}
	if !HandleSession(c, user) {
		return
	}
	log.Debugf("use login token: %+v", user)
	c.Next()
}

// adminTokenMatches reports whether the request's Authorization header is
// the current admin API token (conf.Token). Storage mode
// (conf.TokenPlaintext) decides how the comparison happens: plaintext
// installs still compare raw bytes in constant time (unchanged from before
// hashing existed); hashed installs verify against the salted digest via
// utils.VerifyToken. An empty header never matches either branch — the
// stored token is always non-empty (random.Token()/SetToken's required
// field), and utils.VerifyToken rejects the malformed/empty stored shape —
// so this preserves the existing "empty token never authenticates as admin"
// behavior and instead falls through to the guest/login paths below.
func adminTokenMatches(token string) bool {
	stored := setting.GetStr(conf.Token)
	if setting.GetBool(conf.TokenPlaintext) {
		return subtle.ConstantTimeCompare([]byte(token), []byte(stored)) == 1
	}
	return utils.VerifyToken(stored, token)
}

// HandleSession verifies device sessions and stores context values.
func HandleSession(c *gin.Context, user *model.User) bool {
	clientID := c.GetHeader("Client-Id")
	if clientID == "" {
		clientID = c.Query("client_id")
	}
	key := utils.GetMD5EncodeStr(fmt.Sprintf("%d-%s", user.ID, clientID))
	if err := device.Handle(user.ID, key, c.Request.UserAgent(), c.ClientIP()); err != nil {
		token := c.GetHeader("Authorization")
		if errors.Is(err, errs.SessionInactive) {
			_ = common.InvalidateToken(token)
			common.ErrorResp(c, err, 401)
		} else {
			common.ErrorResp(c, err, 403)
		}
		c.Abort()
		return false
	}
	c.Set(ctxkey.DeviceKey, key)
	c.Set(ctxkey.User, user)
	return true
}

func AuthNotGuest(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	if user.IsGuest() {
		common.ErrorI18n(c, "auth.you_are_guest", 403)
		c.Abort()
	} else {
		c.Next()
	}
}

func AuthAdmin(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	if !user.IsAdmin() {
		common.ErrorI18n(c, "auth.you_are_not_admin", 403)
		c.Abort()
	} else {
		c.Next()
	}
}
