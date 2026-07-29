package handles

import (
	"errors"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// refreshThrottle is kept separate from the password/AuthNMethod login
// throttles: a refresh flood shouldn't lock out logins from the same IP and
// vice versa. It uses the shared loginThrottle policy (see login_throttle.go),
// so all three share one implementation and the same admin-tunable thresholds.
var refreshThrottle = newLoginThrottle()

type RefreshReq struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// Refresh exchanges a refresh token for a new access+refresh pair
// (rotation): the presented refresh token is verified, then immediately
// revoked before the new pair is issued, so it cannot be used twice under
// any circumstance — not just in the replay case below.
//
// If the presented jti comes back ErrTokenRevoked, that means it was
// already consumed by an earlier rotation (or an earlier replay) — someone
// is presenting a dead token, which is the signature of a stolen refresh
// token being replayed after the legitimate client already rotated past it.
// The response to that is not just rejecting this request but revoking the
// whole session (RevokeBySession), since we can no longer tell the
// legitimate holder of this session apart from the attacker.
// @Summary Refresh access token
// @Description Exchanges a refresh token for a new access+refresh token pair. The presented refresh token is rotated (revoked then replaced); reuse of an already-rotated token revokes the whole session.
// @Tags auth
// @Accept json
// @Produce json
// @Param body body RefreshReq true "refresh token"
// @Success 200 {object} common.Resp[gin.H] "data: {token, refresh_token}"
// @Failure 400 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any] "invalid/expired refresh token, or session revoked"
// @Failure 429 {object} common.Resp[any] "too many refresh attempts"
// @Failure 500 {object} common.Resp[any]
// @Router /auth/refresh [post]
// @Router /v4/auth/refresh [post]
func Refresh(c *gin.Context) {
	// Refresh has no Auth middleware and does DB writes, so it needs the
	// same per-IP rate limiting as login: check count first (before parsing
	// anything), and accumulate on every non-success path below so both
	// junk floods and valid-signature replay/brute attempts are bounded.
	ip := c.ClientIP()
	if refreshThrottle.Blocked(ip) {
		common.ErrorI18n(c, "auth.too_many_refresh_attempts", 429)
		return
	}

	var req RefreshReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		refreshThrottle.Fail(ip)
		return
	}

	claims, err := common.ParseTokenClaims(req.RefreshToken)
	if err != nil {
		common.ErrorI18n(c, "auth.invalid_refresh_token", 401)
		refreshThrottle.Fail(ip)
		return
	}

	store := tokenstore.Get()
	if store == nil {
		common.ErrorStrResp(c, "token store is not initialized", 500)
		refreshThrottle.Fail(ip)
		return
	}

	rec, err := store.Get(claims.JTI)
	if err != nil {
		if errors.Is(err, tokenstore.ErrTokenRevoked) {
			if err := store.RevokeBySession(claims.SessionID); err != nil {
				log.Errorf("replay detected but session revoke failed for %s: %+v", claims.SessionID, err)
			}
		}
		common.ErrorI18n(c, "auth.invalid_refresh_token", 401)
		refreshThrottle.Fail(ip)
		return
	}
	if rec.Kind != model.TokenKindRefresh {
		common.ErrorI18n(c, "auth.not_a_refresh_token", 401)
		refreshThrottle.Fail(ip)
		return
	}

	user, err := op.GetUserByName(claims.Username)
	if err != nil {
		common.ErrorResp(c, err, 401)
		refreshThrottle.Fail(ip)
		return
	}
	if user.Disabled {
		common.ErrorI18n(c, "auth.current_user_disabled", 401)
		refreshThrottle.Fail(ip)
		return
	}
	// The refresh token's claims carry the PwdTS at the time it was issued.
	// If the live user's PwdTS has since moved on (password changed), this
	// refresh token predates that change: a stolen refresh token must not
	// be able to keep minting valid access forever after the victim resets
	// their password. Treat it the same as replay — reject and kill the
	// whole session, since we can't tell the legitimate holder apart from
	// an attacker who grabbed the token before the password change.
	if claims.PwdTS != user.PwdTS {
		if err := store.RevokeBySession(claims.SessionID); err != nil {
			log.Errorf("password changed but session revoke failed for %s: %+v", claims.SessionID, err)
		}
		common.ErrorI18n(c, "auth.invalid_refresh_token", 401)
		refreshThrottle.Fail(ip)
		return
	}

	// Revoke the old refresh token before issuing the new pair: if anything
	// below fails, the old token stays dead rather than remaining usable.
	//
	// This must be a compare-and-swap, not a plain Revoke: the up-front
	// store.Get above only catches *sequential* reuse (present the dead
	// token again later). Two concurrent presentations of the same live
	// refresh token both pass that Get before either reaches this point, so
	// a plain Revoke would let both mint a fresh pair. RevokeIfActive closes
	// that window — only the caller that wins the CAS may proceed; the
	// loser is treated as a replay.
	won, err := store.RevokeIfActive(claims.JTI)
	if err != nil {
		common.ErrorResp(c, err, 500)
		refreshThrottle.Fail(ip)
		return
	}
	if !won {
		if err := store.RevokeBySession(claims.SessionID); err != nil {
			log.Errorf("concurrent replay detected but session revoke failed for %s: %+v", claims.SessionID, err)
		}
		common.ErrorI18n(c, "auth.invalid_refresh_token", 401)
		refreshThrottle.Fail(ip)
		return
	}

	newAccess, err := common.GenerateToken(user, claims.SessionID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		refreshThrottle.Fail(ip)
		return
	}
	// Rotation always issues a default-length refresh token: "remember me"
	// is only honored on the initial login, not re-applied on every
	// rotation. See s7-t3-report.md for why this is an intentional
	// simplification rather than an oversight.
	newRefresh, err := common.GenerateRefreshToken(user, claims.SessionID, false)
	if err != nil {
		common.ErrorResp(c, err, 500)
		refreshThrottle.Fail(ip)
		return
	}
	common.SuccessResp(c, gin.H{"token": newAccess, "refresh_token": newRefresh})
	refreshThrottle.Reset(ip)
}
