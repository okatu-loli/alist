package hostcap

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/authlimit"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
)

// ProtocolLoginGuard is the brute-force throttle a protocol (plugin) Login is
// funneled through — the SAME policy the web login path uses
// (server/handles/auth.go), so a CapAuth plugin cannot brute-force a password
// at gRPC speed any faster than the HTTP endpoint would allow. There is no
// client IP over the reverse bridge, so callers key it on pluginID+":"+username.
type ProtocolLoginGuard interface {
	Blocked(key string) bool
	Fail(key string)
	Reset(key string)
}

// protocolLoginGuard is the always-on default: a real internal/authlimit
// throttle reading the same admin-tunable thresholds as the web login. It is a
// package singleton so every Host of the same plugin shares one counter (a
// relaunch cannot reset a brute-force in progress). SetProtocolLoginGuard is the
// cycle-safe DI seam (mirroring SetAuthNMethodFactory/SetDisabledStore) that
// lets internal/bootstrap or a test override it; audit is imported directly
// (internal/audit is a low-level package that imports neither hostcap nor op, so
// there is no cycle) rather than injected.
var protocolLoginGuard ProtocolLoginGuard = authlimit.NewThrottle()

// SetProtocolLoginGuard overrides the default protocol-login throttle. A nil
// argument is ignored so the always-on default can never be cleared.
func SetProtocolLoginGuard(g ProtocolLoginGuard) {
	if g != nil {
		protocolLoginGuard = g
	}
}

// hostAuth is the borrowed Auth capability: it turns an inbound credential into
// an opaque Principal a protocol plugin can then hand to host.FS. Two security
// facts hold here:
//   - Token/session ISSUANCE stays in the core (design §7 decision 3): Login
//     only PROVES a credential is valid and registers a Principal handle; the
//     plugin never receives a token it could replay elsewhere.
//   - Login is the single choke point where a 2FA account is refused a raw
//     password (ErrNeedAppPassword) so a protocol login cannot bypass the
//     second factor (design §2.1). It is also where login rate-limiting AND
//     audit are applied: every attempt is throttled through the SAME
//     internal/authlimit policy the web login uses (keyed on pluginID+username
//     since there is no client IP over the bridge) and recorded via audit.Event,
//     so a CapAuth plugin cannot silently brute-force a password at gRPC speed.
type hostAuth struct {
	grant Grant
	prin  *principals
}

// loginAudit is the action name recorded for protocol (plugin) credential
// attempts, distinct from the web login's "auth.login" so an operator can tell
// a plugin-driven attempt apart from a browser one.
const loginAudit = "plugin.auth.login"

func (a *hostAuth) Login(ctx context.Context, username, password string) (ext.Principal, error) {
	if !a.grant.HasAPI(CapAuth) {
		return "", ext.ErrPermissionDenied
	}
	// Throttle key: no client IP exists over the reverse bridge, so scope the
	// counter to this plugin + the attempted username. A locked-out key is
	// refused before the credential is ever checked (same order as the web path).
	throttleKey := a.grant.PluginID + ":" + username
	target := "plugin:" + a.grant.PluginID
	if protocolLoginGuard.Blocked(throttleKey) {
		audit.Event(ctx, 0, username, loginAudit, target, false)
		return "", ext.ErrPermissionDenied
	}
	user, err := db.GetUserByName(username)
	if err != nil {
		protocolLoginGuard.Fail(throttleKey)
		audit.Event(ctx, 0, username, loginAudit, target, false)
		return "", ext.ErrPermissionDenied
	}
	if user.Disabled {
		protocolLoginGuard.Fail(throttleKey)
		audit.Event(ctx, user.ID, user.Username, loginAudit, target, false)
		return "", ext.ErrPermissionDenied
	}
	// A 2FA-enabled account must not authenticate to a protocol via its raw
	// password — protocol clients cannot enter a TOTP code, so allowing it would
	// turn the protocol into a second-factor bypass. Force an app password. This
	// is NOT counted as a failed credential attempt (the password was never
	// checked): it is a redirect to VerifyAppPassword, not a brute-force step.
	if user.OtpSecret != "" {
		return "", ext.ErrNeedAppPassword
	}
	if err := user.ValidateRawPassword(password); err != nil {
		protocolLoginGuard.Fail(throttleKey)
		audit.Event(ctx, user.ID, user.Username, loginAudit, target, false)
		return "", ext.ErrPermissionDenied
	}
	protocolLoginGuard.Reset(throttleKey)
	audit.Event(ctx, user.ID, user.Username, loginAudit, target, true)
	return ext.Principal(a.prin.issue(user)), nil
}

// VerifyToken resolves a bearer/session token to a Principal via the same
// server/common.ParseToken the HTTP Auth middleware uses (server/middlewares.Auth):
// signature + standard claims + tokenstore liveness (rejects revoked/replayed
// jti and refresh-kind tokens), then the same two post-checks the middleware
// applies — PwdTS mismatch (password changed since the token was issued) and
// Disabled — before minting a Principal. A plugin therefore can never outlive a
// logout/kick/disable that the HTTP path would also honor.
func (a *hostAuth) VerifyToken(_ context.Context, token string) (ext.Principal, error) {
	if !a.grant.HasAPI(CapAuth) {
		return "", ext.ErrPermissionDenied
	}
	claims, err := common.ParseToken(token)
	if err != nil {
		return "", ext.ErrPermissionDenied
	}
	user, err := op.GetUserByName(claims.Username)
	if err != nil {
		return "", ext.ErrPermissionDenied
	}
	if claims.PwdTS != user.PwdTS {
		return "", ext.ErrPermissionDenied
	}
	if user.Disabled {
		return "", ext.ErrPermissionDenied
	}
	return a.issuePrincipal(user), nil
}

// VerifyAppPassword resolves a per-client app password (design W3) to a
// Principal via op.VerifyAppPassword — the same uniform-error, no-cache
// resolver the account self-service API is built on, so a just-revoked app
// password fails here on the very next call too. This is deliberately a
// SEPARATE credential family from Login: an app password never satisfies
// Login (op.VerifyAppPassword is never consulted there), and Login's
// ErrNeedAppPassword path for a 2FA account is unchanged by this wiring — it
// tells the protocol client to come back through THIS method instead.
func (a *hostAuth) VerifyAppPassword(_ context.Context, username, appPassword string) (ext.Principal, error) {
	if !a.grant.HasAPI(CapAuth) {
		return "", ext.ErrPermissionDenied
	}
	user, err := op.VerifyAppPassword(username, appPassword)
	if err != nil {
		return "", ext.ErrPermissionDenied
	}
	return a.issuePrincipal(user), nil
}

// issuePrincipal is a helper for tests / callers that already hold a *model.User
// (e.g. VerifyToken once wired) to mint a Principal handle for it.
func (a *hostAuth) issuePrincipal(u *model.User) ext.Principal {
	return ext.Principal(a.prin.issue(u))
}
