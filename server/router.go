package server

import (
	"net/http"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/internal/webdav"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/alist-org/alist/v3/server/handles"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/alist-org/alist/v3/server/static"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func Init(e *gin.Engine) {
	// Wrong method on a known path answers 405, not 404 (Step 13 verb
	// normalization — applies to every mount).
	e.HandleMethodNotAllowed = true
	if !utils.SliceContains([]string{"", "/"}, conf.URL.Path) {
		e.GET("/", func(c *gin.Context) {
			c.Redirect(302, conf.URL.Path)
		})
	}
	Cors(e)
	e.Use(middlewares.SessionRefresh)
	g := e.Group(conf.URL.Path)
	if conf.Conf.Scheme.HttpPort != -1 && conf.Conf.Scheme.HttpsPort != -1 && conf.Conf.Scheme.ForceHttps {
		e.Use(middlewares.ForceHttps)
	}
	g.Any("/ping", handles.Ping)
	g.GET("/favicon.ico", handles.Favicon)
	g.GET("/robots.txt", handles.Robots)
	g.GET("/i/:link_name", handles.Plist)
	common.SecretKey = []byte(conf.Conf.JwtSecret)
	g.Use(middlewares.StoragesLoaded)
	if conf.Conf.MaxConnections > 0 {
		g.Use(middlewares.MaxAllowed(conf.Conf.MaxConnections))
	}

	downloadLimiter := middlewares.DownloadRateLimiter(stream.ClientDownloadLimit)
	perConnLimiter := middlewares.PerConnDownloadRateLimiter()
	signCheck := middlewares.Down()
	g.GET("/d/*path", signCheck, downloadLimiter, perConnLimiter, handles.Down)
	g.GET("/p/*path", signCheck, downloadLimiter, perConnLimiter, handles.Proxy)
	g.HEAD("/d/*path", signCheck, handles.Down)
	g.HEAD("/p/*path", signCheck, handles.Proxy)

	// Internal WebDAV (design W5): a top-level, clean-URL-root mount outside
	// /api/v4 -- WebDAV clients (nPlayer/RaiDrive/davfs2/...) expect to mount
	// exactly this path. Gated by the builtin.webdav extension (so an admin
	// can kill-switch the whole surface) and authenticated per-request via
	// HTTP Basic Auth (app password, or a raw password for a non-2FA
	// account); op.Authorize inside internal/webdav's FileSystem adapter is
	// the sole authorization boundary from there, same RBAC5 policy a
	// browser session gets.
	davMethods := []string{
		http.MethodOptions, "PROPFIND", http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, "MKCOL", http.MethodDelete, "MOVE", "COPY", "LOCK", "UNLOCK", "PROPPATCH",
	}
	davGate := middlewares.RequireExtensionEnabled("builtin.webdav")
	g.Match(davMethods, "/dav/*path", davGate, webdav.BasicAuth, webdav.Handler)

	// Register the configured SSO providers as AuthNMethods now that settings are
	// loaded, and track later config changes.
	handles.InitSSO()

	// The versioned v4 contract (Step 13) and the legacy /api mount share all
	// handlers; /api is deprecated — kept alive for exactly one major release,
	// advertised via Deprecation/Link headers so clients can detect it.
	apiV4(g)

	api := g.Group("/api", middlewares.Deprecated("/api/v4"), middlewares.AuditContext("http"))
	// I18n negotiates the response locale. It runs twice by design: once here
	// so anonymous endpoints (login/refresh/public) honor Accept-Language, and
	// again inside the authed group AFTER Auth so a logged-in user's explicit
	// User.Language preference (only known post-auth) can override the header.
	api.Use(middlewares.I18n)
	auth := api.Group("", middlewares.Auth, middlewares.I18n)

	api.POST("/auth/login", handles.Login)
	api.POST("/auth/login/hash", handles.LoginHash)
	// TOTP login step-up completion (Phase C red-team I6): this legacy /api
	// mount had no alias for the v4-only step-up route at all, so a client
	// still pointed at /api whose loginHash 402'd with a challenge (loginHash
	// itself is shared by both mounts) had no endpoint on ITS OWN prefix to
	// ever complete the login. Same handler as apiV4's /api/v4/auth/login/2fa,
	// anonymous for the same reason (the challenge id, minted only after a
	// correct password, is the access control). The self-service
	// /me/2fa/totp/* surface is intentionally NOT mirrored here — like
	// passkey/app-password/SSO self-service, it is a v4-only addition this
	// legacy mount never carried; a v3-only client has no way to bind TOTP in
	// the first place, so completing a step-up (for an account bound via the
	// v4 UI, or migrated from a v3 install that already had OtpSecret set) is
	// the one gap worth closing here.
	api.POST("/auth/login/2fa", handles.LoginTOTPStepUp)
	api.POST("/auth/refresh", handles.Refresh)
	auth.GET("/me", handles.CurrentUser)
	auth.POST("/me/update", handles.UpdateCurrent)
	auth.GET("/auth/logout", handles.LogOut)
	auth.GET("/me/sessions", handles.ListMySessions)
	auth.POST("/me/sessions/evict", handles.EvictMySession)

	// no need auth
	public := api.Group("/public")
	public.Any("/settings", handles.PublicSettings)

	_fs(auth.Group("/fs"))
	_task(auth.Group("/task", middlewares.AuthNotGuest))
	admin(auth.Group("/admin", middlewares.AuthAdmin))
	if flags.Debug || flags.Dev {
		debug(g.Group("/debug"))
	}

	// Plugin theme assets (theming spec §5.4): read-only, ANONYMOUS, same-origin
	// backing for a theme's rewritten url()/@font-face src. Anonymous because the
	// browser loads them as css subresources (no Authorization header); the
	// handler restricts to enabled plugins + an allowlisted file type + a
	// traversal guard. Registered before the static catch-all so it wins the match.
	g.GET("/plugin-assets/:id/*filepath", handles.PluginAsset)

	static.Static(g, func(handlers ...gin.HandlerFunc) {
		// ExtPublicPrefixDispatch runs FIRST: it resolves an unmatched
		// request's top-level path segment against a plugin's claimed
		// public_prefix (design §3 PublicRouter) and, on a hit, proxies to
		// that plugin's public route — aborting the chain so the handlers
		// below (the SPA fallback) never also run. On a miss it returns
		// without aborting, so gin's NoRoute chain falls through to those
		// handlers unchanged: the SPA fallback's behavior for everything it
		// does not recognize is preserved byte for byte (hard constraint —
		// the demo frontend depends on it).
		e.NoRoute(append([]gin.HandlerFunc{handles.ExtPublicPrefixDispatch}, handlers...)...)
	})
}

// admin wires the /api/admin control surface. The group itself keeps
// middlewares.AuthAdmin (see Init) as a FAIL-CLOSED backstop: every route below
// is additionally gated per (domain, verb) via RequireSystem, but any route
// that forgets its mapping still denies non-admins by default (spec §8).
//
// Two gate kinds are used:
//   - RequireSystem(domain, verb) — routed through op.Authorize; admins
//     short-circuit, non-admin operators need system.<domain>.<verb> (§2.5).
//   - RequireAdmin — IsAdmin-only, never delegable: the authorization control
//     plane (role/permission mutations + bindings) and admin-token minting
//     (§5). These are NOT RequireSystem so an operator can never rewrite "who
//     can do what".
func admin(g *gin.RouterGroup) {
	meta := g.Group("/meta")
	meta.GET("/list", middlewares.RequireSystem("meta", "view"), handles.ListMetas)
	meta.GET("/get", middlewares.RequireSystem("meta", "view"), handles.GetMeta)
	meta.POST("/create", middlewares.RequireSystem("meta", "manage"), handles.CreateMeta)
	meta.POST("/update", middlewares.RequireSystem("meta", "manage"), handles.UpdateMeta)
	meta.POST("/delete", middlewares.RequireSystem("meta", "manage"), handles.DeleteMeta)

	user := g.Group("/user")
	user.GET("/list", middlewares.RequireSystem("user", "view"), handles.ListUsers)
	user.GET("/get", middlewares.RequireSystem("user", "view"), handles.GetUser)
	user.POST("/create", middlewares.RequireSystem("user", "manage"), handles.CreateUser)
	// user/update carries a handler-layer IsAdmin gate on role-binding changes
	// (spec §5.3): user.manage covers lifecycle, but changing a role binding is
	// control-plane and stays IsAdmin-only inside UpdateUser.
	user.POST("/update", middlewares.RequireSystem("user", "manage"), handles.UpdateUser)
	user.POST("/delete", middlewares.RequireSystem("user", "manage"), handles.DeleteUser)
	user.POST("/del_cache", middlewares.RequireSystem("user", "manage"), handles.DelUserCache)

	role := g.Group("/role")
	// role reads are RequireSystem view; every role MUTATION is IsAdmin-only —
	// role manage is the authorization graph and converges to RequireAdmin (§5).
	role.GET("/list", middlewares.RequireSystem("role", "view"), handles.ListRoles)
	role.GET("/get", middlewares.RequireSystem("role", "view"), handles.GetRole)
	role.GET("/permissions", middlewares.RequireSystem("role", "view"), handles.RolePermissions)
	role.POST("/create", middlewares.RequireAdmin, handles.CreateRole)
	role.POST("/update", middlewares.RequireAdmin, handles.UpdateRole)
	role.POST("/delete", middlewares.RequireAdmin, handles.DeleteRole)
	role.POST("/set_permissions", middlewares.RequireAdmin, handles.SetRolePermissions)

	permission := g.Group("/permission")
	// permission reads are RequireSystem view; every permission MUTATION is
	// IsAdmin-only (authorization graph, §5) and additionally guarded at the
	// handler layer for system-type rows (defense-in-depth).
	permission.GET("/catalog", middlewares.RequireSystem("permission", "view"), handles.GetPermissionCatalog)
	permission.GET("/list", middlewares.RequireSystem("permission", "view"), handles.ListPermissions)
	permission.GET("/get", middlewares.RequireSystem("permission", "view"), handles.GetPermission)
	permission.GET("/roles", middlewares.RequireSystem("permission", "view"), handles.PermissionRoles)
	permission.POST("/create", middlewares.RequireAdmin, handles.CreatePermission)
	permission.POST("/update", middlewares.RequireAdmin, handles.UpdatePermission)
	permission.POST("/delete", middlewares.RequireAdmin, handles.DeletePermission)

	storage := g.Group("/storage")
	storage.GET("/list", middlewares.RequireSystem("storage", "view"), handles.ListStorages)
	storage.GET("/get", middlewares.RequireSystem("storage", "view"), handles.GetStorage)
	storage.POST("/create", middlewares.RequireSystem("storage", "manage"), handles.CreateStorage)
	storage.POST("/update", middlewares.RequireSystem("storage", "manage"), handles.UpdateStorage)
	storage.POST("/delete", middlewares.RequireSystem("storage", "manage"), handles.DeleteStorage)
	storage.POST("/enable", middlewares.RequireSystem("storage", "manage"), handles.EnableStorage)
	storage.POST("/disable", middlewares.RequireSystem("storage", "manage"), handles.DisableStorage)
	storage.POST("/load_all", middlewares.RequireSystem("storage", "manage"), handles.LoadAllStorages)

	// driver is read-only and was previously ungated within the admin group;
	// it now requires system.driver.view so its schema never leaks to a
	// non-admin operator (spec §2.5/§8).
	driver := g.Group("/driver", middlewares.RequireSystem("driver", "view"))
	driver.GET("/list", handles.ListDriverInfo)
	driver.GET("/names", handles.ListDriverNames)
	driver.GET("/info", handles.GetDriverInfo)

	setting := g.Group("/setting")
	setting.GET("/get", middlewares.RequireSystem("setting", "view"), handles.GetSetting)
	setting.GET("/list", middlewares.RequireSystem("setting", "view"), handles.ListSettings)
	setting.POST("/save", middlewares.RequireSystem("setting", "manage"), handles.SaveSettings)
	setting.POST("/delete", middlewares.RequireSystem("setting", "manage"), handles.DeleteSetting)
	// token minting is split out of setting.manage and stays IsAdmin-only
	// (spec §3.2/§5.4): the static admin token is an admin-equivalent credential.
	setting.GET("/get_token", middlewares.RequireAdmin, handles.GetToken)
	setting.POST("/reset_token", middlewares.RequireAdmin, handles.ResetToken)
	setting.POST("/set_token", middlewares.RequireAdmin, handles.SetToken)

	// Legacy /admin/task alias kept for compatibility with older automation.
	// Gated at system.task.view as a floor; the task handlers themselves scope
	// results per creator and retain an inline IsAdmin() check (task.go) that
	// spec §8 records as an explicit IsAdmin-only exception (Step 13 converges
	// the task verbs onto RequireSystem).
	_task(g.Group("/task", middlewares.RequireSystem("task", "view")))

	signingKey := g.Group("/signing_key")
	signingKey.GET("/list", middlewares.RequireSystem("signing_key", "view"), handles.ListSigningKeys)
	signingKey.POST("/rotate", middlewares.RequireSystem("signing_key", "manage"), handles.RotateSigningKey)
	signingKey.POST("/gc", middlewares.RequireSystem("signing_key", "manage"), handles.GCSigningKeys)

	session := g.Group("/session")
	session.GET("/list", middlewares.RequireSystem("session", "view"), handles.ListSessions)
	session.POST("/evict", middlewares.RequireSystem("session", "manage"), handles.EvictSession)
}

func _fs(g *gin.RouterGroup) {
	g.Any("/list", handles.FsList)
	g.Any("/get", handles.FsGet)
	g.Any("/other", handles.FsOther)
	g.Any("/dirs", handles.FsDirs)
	g.POST("/mkdir", handles.FsMkdir)
	g.POST("/rename", handles.FsRename)
	g.POST("/batch_rename", handles.FsBatchRename)
	g.POST("/regex_rename", handles.FsRegexRename)
	g.POST("/move", handles.FsMove)
	g.POST("/recursive_move", handles.FsRecursiveMove)
	g.POST("/copy", handles.FsCopy)
	g.POST("/remove", handles.FsRemove)
	g.POST("/remove_empty_directory", handles.FsRemoveEmptyDirectory)
	uploadLimiter := middlewares.UploadRateLimiter(stream.ClientUploadLimit)
	g.PUT("/put", middlewares.FsUp, uploadLimiter, handles.FsStream)
	g.PUT("/form", middlewares.FsUp, uploadLimiter, handles.FsForm)
	g.POST("/link", middlewares.AuthAdmin, handles.Link)
}

func _task(g *gin.RouterGroup) {
	handles.SetupTaskRoute(g)
}

func Cors(r *gin.Engine) {
	config := cors.DefaultConfig()
	// config.AllowAllOrigins = true
	config.AllowOrigins = conf.Conf.Cors.AllowOrigins
	config.AllowHeaders = conf.Conf.Cors.AllowHeaders
	config.AllowMethods = conf.Conf.Cors.AllowMethods
	r.Use(cors.New(config))
}
