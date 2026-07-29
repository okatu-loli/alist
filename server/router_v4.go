package server

import (
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/server/handles"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// apiV4 mounts the versioned /api/v4 contract (rollout Step 13): the same
// handlers as the legacy /api tree behind normalized verbs (reads are GET,
// state changes are POST/PUT/DELETE — gin returns 405 for a wrong method) and
// RESTful path-param resources. Handlers are shared with the legacy mount:
// identifier reads go through handles.pathOrQuery (path param first, query
// fallback) and PUT-with-:id bodies through middlewares.ParamIntoJSONBody, so
// both mounts share one implementation and one test surface.
//
// The envelope under this mount carries `version: "v4"` (middlewares.APIVersion
// → common.RespVersion); the legacy /api mount advertises deprecation headers
// and is kept alive for exactly one major release.
func apiV4(g *gin.RouterGroup) {
	v4 := g.Group("/api/v4", middlewares.APIVersion("v4"), middlewares.AuditContext("http"), middlewares.I18n)
	auth := v4.Group("", middlewares.Auth, middlewares.I18n)

	// --- anonymous ---
	v4.POST("/auth/login", handles.Login)
	v4.POST("/auth/login/hash", handles.LoginHash)
	// TOTP login step-up completion (Phase C: internal TOTP second factor):
	// anonymous by necessity (no session exists until this succeeds) — the
	// challenge id, minted by loginHash only after a correct password, is the
	// access control. See server/handles/login_stepup.go.
	v4.POST("/auth/login/2fa", handles.LoginTOTPStepUp)
	v4.POST("/auth/refresh", handles.Refresh)
	// AuthNMethod login path (design §2.2): a NEW, opt-in, purely additive way
	// to log in via an installed login-method extension (SSO/OIDC, LDAP). It
	// does NOT touch the built-in /auth/login password path. The plugin only
	// proves identity; the core alone mints the session (see server/handles/
	// authn.go for the token-issuance, admin-cap and auto-provision gates).
	v4.POST("/auth/method/:id/begin", handles.AuthNMethodBegin)
	v4.POST("/auth/method/:id/finish", handles.AuthNMethodFinish)
	// Generic second-factor step-up completion (Phase F generalization: TOTP's
	// /auth/login/2fa above was the first, no-longer-only, caller of this
	// dispatch). Any installed second_factor AuthNMethod's step-up challenge
	// (begun internally by loginHash/completeSSOLogin, never by the anonymous
	// /begin above) completes here; a primary/federated method id is refused
	// with 400 — see handles.AuthNMethodStepUpFinish.
	v4.POST("/auth/method/:id/stepup/finish", handles.AuthNMethodStepUpFinish)
	// Federated (OIDC SSO) callback: the external IdP redirects a raw browser
	// navigation here with no Authorization header, so it is anonymous — the
	// single-use state parked at begin is the access control. It exchanges the
	// code, verifies the id_token, resolves identity (enforcing "admin only if
	// already linked"), then redirects back to the SPA (see handles.sso.go).
	//
	// Gated on builtin.sso (Project A-2): the callback is the completion of a
	// federated SSO login, so disabling the SSO built-in makes it 403 and no new
	// SSO session can be minted (existing sessions are untouched). The shared
	// /auth/method/:id/begin is NOT gated here because it also serves the built-in
	// passkey method; the callback is SSO-only, so it is the precise boundary.
	v4.GET("/auth/method/:id/callback",
		middlewares.RequireExtensionEnabled("builtin.sso"), handles.AuthNMethodCallback)
	// Anonymous login-page listing of ENABLED SSO providers (id/key/name/preset
	// only — no secrets, no issuer internals) so the login page can render a
	// button per provider. Gated on builtin.sso so a disabled SSO built-in hands
	// the login page an empty/403 provider list (no buttons to start a flow).
	v4.GET("/auth/sso/providers",
		middlewares.RequireExtensionEnabled("builtin.sso"), handles.PublicSSOProviders)
	// Anonymous login-page listing of every ENABLED extension's declared
	// login_methods (plugin id + id/kind/label/icon only — no secrets), so the
	// login page can render a button per method (design §4, W10c). Ungated on
	// any single extension: it flattens login_methods across ALL enabled
	// extensions (unlike the SSO-specific list above), and a login method is
	// just render metadata — nothing here can start a privileged action on its
	// own (that happens at the existing, already-anonymous /auth/method/:id/begin).
	v4.GET("/auth/login-methods", handles.PublicLoginMethods)
	v4.GET("/public/settings", handles.PublicSettings)
	// Installed theme-marketplace themes (theming spec §J5, I1 remediation):
	// a DEDICATED, ETag-cacheable endpoint — deliberately NOT folded into
	// /public/settings above, which has no conditional-caching support and is
	// fetched on every anonymous page load.
	v4.GET("/public/themes", handles.PublicMarketThemes)
	// OAuth callback for an extension install wizard's oauth step (WS-8): the
	// vendor's redirect is a raw browser navigation, so it carries no
	// Authorization header; state is the access control (see the handler doc).
	v4.GET("/extensions/oauth/callback", handles.ExtensionOAuthCallback)
	// Public plugin routes (F1 PublicPage): anonymous share landing pages etc.,
	// mounted OUTSIDE auth. Reaching a plugin here requires the "public_routes"
	// grant + a declared Public route; its Handle runs with a delegated-only Host
	// (see handles.ExtPublicRoute). A distinct top-level segment (/ext-public/)
	// keeps it clear of both the authed /ext/ mount and the /extensions group.
	v4.Any("/ext-public/:pluginid/*proxypath", handles.ExtPublicRoute)

	// Remote-agent pairing completion + heartbeat (design W12-2a): anonymous
	// — the pairing code / ed25519 signature IS the credential, mirroring how
	// /auth/sso/providers and /auth/login-methods sit outside the auth group
	// above. pair/complete is additionally per-IP rate-limited on top of the
	// code's own entropy; heartbeat is self-authenticated by the agent's
	// ed25519 signature (never a session) — see server/handles/agent_pair.go
	// for the full protocol/replay-defense reasoning. Distinct from the
	// admin-only /admin/agents/* management surface (adminV4) below.
	agentAnon := v4.Group("/agent")
	agentAnon.POST("/pair/complete", handles.PairAgentComplete)
	agentAnon.POST("/heartbeat", handles.AgentHeartbeat)
	// Remote-agent task-delegation capability endpoints (design W12-3a): also
	// anonymous, also self-authenticated — but by a short-lived, per-task
	// capability token (cap_token) instead of the agent's long-term ed25519
	// key. "/task/:id/*" (singular) is deliberately distinct from the authed
	// "/agent/tasks" creation endpoint (plural) below, mounted under `auth`.
	// See server/handles/agent_task.go's top-of-file doc for the full
	// "capability is not a bypass" re-verification chain every one of these
	// runs on every call.
	agentAnon.POST("/task/:id/src-link", handles.AgentTaskSrcLink)
	agentAnon.POST("/task/:id/rapid", handles.AgentTaskTryRapid)
	agentAnon.PUT("/task/:id/content", handles.AgentTaskPutContent)
	agentAnon.POST("/task/:id/progress", handles.AgentTaskProgress)
	agentAnon.POST("/task/:id/status", handles.AgentTaskStatus)
	// Cap-token renewal (design cap-renewal): outbound-mode CapClient's HTTP
	// leg, so a copy that outlives one token's TTL doesn't start 401ing.
	agentAnon.POST("/task/:id/renew", handles.AgentTaskRenewCap)

	// Public share access (design §2.3-B): unauthenticated, token-bearer read of
	// a share and its confined subtree. Mounted on v4 (OUTSIDE auth) on purpose —
	// the token is the credential, so these never see a user in context and never
	// read Authorization/Cookie. ResolveShare gives a uniform 404 for missing/
	// expired/disabled (enumeration defense), an optional password gates list/
	// download, ShareSubPath confines every ?sub= to the shared subtree, and
	// download 302s to the existing /d sign channel. The authed create/list/delete
	// surface is the /shares group under auth below.
	// Gated on builtin.share (Project A-2): the whole public access surface goes
	// 403 when an admin disables sharing, so existing share links stop resolving
	// (not merely hidden in the UI). The gate is the first thing this anonymous
	// group meets, before token resolution.
	s := v4.Group("/s", middlewares.RequireExtensionEnabled("builtin.share"))
	s.GET("/:token", handles.ShareInfo)
	s.GET("/:token/list", handles.ShareList)
	s.GET("/:token/download", handles.ShareDownload)

	// --- account (self-service) ---
	auth.POST("/auth/logout", handles.LogOut) // v3 used GET for a state change
	auth.GET("/me", handles.CurrentUser)
	auth.PUT("/me", handles.UpdateCurrent)
	auth.GET("/me/sessions", handles.ListMySessions)
	auth.DELETE("/me/sessions/:id",
		middlewares.ParamIntoJSONBody("id", "session_id"), handles.EvictMySession)

	// App passwords self-service (design W3): protocol clients (WebDAV/FTP and
	// friends) can't do 2FA, so a logged-in user mints/lists/revokes per-client
	// credentials here instead of handing out their real password. Self-scoped
	// only — see server/handles/app_password.go and op.DeleteAppPassword (no
	// admin bypass).
	auth.GET("/me/app-passwords", handles.ListAppPasswords)
	auth.POST("/me/app-passwords", handles.CreateAppPassword)
	auth.DELETE("/me/app-passwords/:id", handles.DeleteAppPassword)

	// Remote-agent task delegation (design W12-3a): a logged-in user (not
	// admin-only — a user may delegate their own copy) creates a cross-drive
	// copy task for a paired agent. handles.CreateAgentTask independently
	// re-authorizes the creator (read src, write dst) and re-checks the
	// agent's storage whitelist before ever creating a row — see that
	// handler's doc. The agent-facing capability endpoints that actually move
	// bytes are anonymous/cap_token-authed, grouped with agentAnon above.
	auth.POST("/agent/tasks", handles.CreateAgentTask)

	// SSO account-linking self-service (design §4): a logged-in user starts a
	// link flow (binds the verified subject to their OWN account via the state),
	// lists their linked identity, and unlinks it. Linking is the only way an
	// admin gains SSO login capability. The link flow completes at the shared
	// anonymous callback above.
	auth.POST("/auth/method/:id/link", handles.SSOLinkBegin)
	auth.GET("/me/sso", handles.SSOListLinks)
	auth.DELETE("/me/sso/:key", handles.SSOUnlink)

	// Passkey self-management (authenticated): a user registers/lists/removes
	// their OWN passkeys. The anonymous passwordless LOGIN path is the built-in
	// passkey AuthNMethod at /auth/method/builtin.passkey/{begin,finish} above —
	// registration must stay authed so a passkey can only be added to the caller's
	// own account, never anonymously to someone else's.
	// Gated on builtin.passkey (design W5): disabling the passkey built-in makes
	// register/list/delete all 403, so passkey is dead on the self-management path
	// too — matching the login-dispatch gate in handles/authn.go — not merely
	// hidden in the account UI.
	passkeys := auth.Group("/auth/passkey", middlewares.RequireExtensionEnabled("builtin.passkey"))
	passkeys.GET("", handles.PasskeyList)
	passkeys.POST("/register/begin", handles.PasskeyRegisterBegin)
	passkeys.POST("/register/finish", handles.PasskeyRegisterFinish)
	passkeys.DELETE("/:credid", handles.PasskeyDelete)

	// TOTP self-management (authenticated, Phase C: internal TOTP second
	// factor): a user begins/confirms their OWN enrollment, unbinds it, and
	// regenerates their own recovery codes. Gated on builtin.totp (same
	// treatment as builtin.passkey above): disabling it 403s this whole
	// surface AND makes the login step-up check (server/handles/auth.go)
	// degrade to "no second factor" even for an already-enrolled user.
	totpGroup := auth.Group("/me/2fa/totp", middlewares.RequireExtensionEnabled("builtin.totp"))
	totpGroup.GET("/status", handles.TOTPStatus)
	totpGroup.POST("/begin", handles.TOTPBegin)
	totpGroup.POST("/confirm", handles.TOTPConfirm)
	totpGroup.DELETE("", handles.TOTPDelete)
	totpGroup.POST("/recovery-codes/regenerate", handles.TOTPRegenerateRecoveryCodes)

	// --- fs: reads are GET (query-bound), mutations keep their POST/PUT ---
	fs := auth.Group("/fs")
	fs.GET("/list", handles.FsList)
	// Recursive file-name search under a subtree. GET (query-bound, like /list).
	// Gated inside the handler on ActRead over the search root; per-child
	// ActRead + hide-meta filtering happens as the walk descends (search pkg),
	// so results never contain files the caller cannot read.
	fs.GET("/search", middlewares.RequireExtensionEnabled("builtin.search"), handles.FsSearch)
	fs.GET("/get", handles.FsGet)
	// Image thumbnail: a resized JPEG preview for the file-listing UI. Real
	// HTTP statuses (not the 200+JSON envelope), since this is a plain <img
	// src> subresource load -- see handles.FsThumb's doc comment. Gated on
	// op.Authorize(ActRead) inside the handler, same boundary as every other
	// fs read.
	fs.GET("/thumb", handles.FsThumb)
	fs.GET("/dirs", handles.FsDirs)
	// Archive browsing (internal/archive): browse INTO an archive file's
	// entries without downloading the whole thing first. Format support is
	// dispatched by file extension against every registered Kind=ArchiveTool
	// (the built-in zip tool, plus any installed plugin adding rar/7z/...) --
	// see internal/extension.FindArchiveToolForExt; driver-native
	// ext.ArchiveReader is a separate, orthogonal seam. op.Authorize(ActRead)
	// on the archive path is the sole access boundary (enforced inside
	// internal/archive, same pattern as FsThumb). meta/list are the usual
	// 200+JSON envelope; extract streams bytes with a real HTTP status, same
	// reasoning as /fs/thumb.
	fs.GET("/archive/meta", handles.FsArchiveMeta)
	fs.GET("/archive/list", handles.FsArchiveList)
	fs.GET("/archive/extract", handles.FsArchiveExtract)
	// Extension field-group backfill (F2d): recompute FileDecorator columns
	// /fs/list returned as pending. Each ref is authorized for the caller before
	// any decorator sees it.
	fs.GET("/fields", handles.FsFields)
	fs.POST("/other", handles.FsOther) // driver RPC passthrough, not a plain read
	fs.POST("/mkdir", handles.FsMkdir)
	fs.POST("/rename", handles.FsRename)
	fs.POST("/batch_rename", handles.FsBatchRename)
	fs.POST("/regex_rename", handles.FsRegexRename)
	fs.POST("/move", handles.FsMove)
	fs.POST("/recursive_move", handles.FsRecursiveMove)
	fs.POST("/copy", handles.FsCopy)
	fs.POST("/remove", handles.FsRemove)
	fs.POST("/remove_empty_directory", handles.FsRemoveEmptyDirectory)
	// Offline download: queue one background DownloadTask per URL into the target
	// directory. Gated inside the handler on BOTH ActWrite and ActOfflineDownload
	// over the target path (labels-style handler-layer authz). Task progress is
	// polled through /task/offline_download/* (SetupTaskRoute).
	fs.POST("/add_offline_download",
		middlewares.RequireExtensionEnabled("builtin.offline_download"), handles.AddOfflineDownload)
	// The per-request tool picker reads the tools this caller may select (SimpleHttp
	// + every enabled daemon tool) + the default to preselect. Same authed group and
	// feature gate as add_offline_download so a logged-in user with the offline-download
	// feature can populate the dialog.
	fs.GET("/offline_download/tools",
		middlewares.RequireExtensionEnabled("builtin.offline_download"), handles.ListOfflineDownloadTools)
	uploadLimiter := middlewares.UploadRateLimiter(stream.ClientUploadLimit)
	fs.PUT("/put", middlewares.FsUp, uploadLimiter, handles.FsStream)
	fs.PUT("/form", middlewares.FsUp, uploadLimiter, handles.FsForm)

	// --- labels (built-in FeatureBundle): shared file labels. Reads are GET;
	// apply/remove are POST and gated on ActWrite over the target path inside the
	// handler (labels are shared file metadata). The vocabulary CRUD is admin-only
	// and lives under /admin/labels (adminV4). Filtering (/labels/:id/files)
	// per-path ActRead-filters its result. ---
	labels := auth.Group("/labels")
	labels.GET("", handles.LabelsList)
	labels.GET("/of", handles.LabelsOf)
	labels.GET("/:id/files", handles.LabelFiles)
	labels.POST("/apply", handles.LabelApply)
	labels.POST("/remove", handles.LabelRemove)

	// --- shares (built-in, design §2.3-A): a logged-in user mints public share
	// links for paths they can read, lists their OWN links, and revokes them.
	// Create is gated inside the handler on ActRead over the target path (a share
	// delegates the creator's read right); list/delete are confined to the creator
	// (delete also allows an admin). The unauthenticated public access surface
	// (/s/:token) is mounted OUTSIDE this auth group (Wave 3). ---
	shares := auth.Group("/shares")
	// Create is gated on builtin.share (Project A-2): disabling sharing stops new
	// links from being minted. List/delete of one's OWN links are left ungated on
	// purpose — a user must still be able to see and revoke links they created
	// after the feature is turned off (data is never destroyed, only new creation
	// and public access are blocked).
	shares.POST("", middlewares.RequireExtensionEnabled("builtin.share"), handles.CreateShare)
	shares.GET("", handles.ListShares)
	shares.DELETE("/:id", handles.DeleteShare)

	// --- tasks (same contract as v3; undone/done are already GET) ---
	handles.SetupTaskRoute(auth.Group("/task", middlewares.AuthNotGuest))

	extensionsV4(auth.Group("/extensions"))

	// Authenticated plugin API routes (F1 APIRoute): a plugin's non-public
	// sub-routes, mounted behind auth. Enablement + a declared (authenticated)
	// route prefix are checked in extension.DispatchAPIRoute (handles.ExtRoute).
	auth.Any("/ext/:pluginid/*proxypath", handles.ExtRoute)

	adminV4(auth.Group("/admin", middlewares.AuthAdmin))
}

// extensionsV4 wires the Step 10 extension contract. Reads that drive the
// frontend renderer (contributions, ui bundles) are available to any logged-in
// user; anything that mutates or runs plugin code (actions, install wizard) is
// RequireAdmin — the conservative default until per-action permission scopes
// land (Step 11).
func extensionsV4(g *gin.RouterGroup) {
	g.GET("/contributions", handles.ExtensionContributions)
	g.GET("/:id/ui/*filepath", handles.ExtensionUI)
	// StatusProvider poll target (F2b): a background-service / access-protocol
	// status card polls this to render running state + metrics. Any logged-in user
	// may read it (it drives the services page renderer), like /contributions.
	g.GET("/:id/status/:sid", handles.ExtensionServiceStatus)
	g.POST("/:id/actions/:aid", middlewares.RequireAdmin, handles.InvokeExtensionAction)
	g.POST("/:id/assist/:fid", middlewares.RequireAdmin, handles.SubmitExtensionAssist)
	g.POST("/:id/wizard/start", middlewares.RequireAdmin, handles.StartExtensionWizard)
	g.POST("/:id/wizard/:session/next", middlewares.RequireAdmin, handles.ExtensionWizardNext)
}

// adminV4 wires the /api/v4/admin resources. Gate placement mirrors the v3
// admin() wiring exactly — AuthAdmin backstop on the group, RequireSystem per
// (domain, verb), RequireAdmin for the authorization control plane — only the
// route shapes change (path-param resources, normalized verbs).
func adminV4(g *gin.RouterGroup) {
	intoID := middlewares.ParamIntoJSONBody("id", "id")

	// Label vocabulary management is admin-only (spec §3): a label is shared
	// across all users, so creating/editing/deleting one stays on the admin
	// control plane (the group's AuthAdmin backstop). Apply/remove/list/filter
	// live under the authed /labels group above.
	adminLabels := g.Group("/labels")
	adminLabels.POST("", handles.LabelCreate)
	adminLabels.PUT("/:id", middlewares.ParamIntoJSONBody("id", "id"), handles.LabelUpdate)
	adminLabels.DELETE("/:id", handles.LabelDelete)

	metas := g.Group("/metas")
	metas.GET("", middlewares.RequireSystem("meta", "view"), handles.ListMetas)
	metas.GET("/:id", middlewares.RequireSystem("meta", "view"), handles.GetMeta)
	metas.POST("", middlewares.RequireSystem("meta", "manage"), handles.CreateMeta)
	metas.PUT("/:id", middlewares.RequireSystem("meta", "manage"), intoID, handles.UpdateMeta)
	metas.DELETE("/:id", middlewares.RequireSystem("meta", "manage"), handles.DeleteMeta)

	users := g.Group("/users")
	users.GET("", middlewares.RequireSystem("user", "view"), handles.ListUsers)
	users.GET("/:id", middlewares.RequireSystem("user", "view"), handles.GetUser)
	users.POST("", middlewares.RequireSystem("user", "manage"), handles.CreateUser)
	users.PUT("/:id", middlewares.RequireSystem("user", "manage"), intoID, handles.UpdateUser)
	users.DELETE("/:id", middlewares.RequireSystem("user", "manage"), handles.DeleteUser)
	// Admin TOTP-reset recovery path (Phase C red-team C1): clears a user's
	// second factor with no proof of possession, so — unlike the sibling
	// user CRUD routes above — it is gated on the non-delegable RequireAdmin,
	// not RequireSystem("user","manage").
	users.POST("/:id/cancel_2fa", middlewares.RequireAdmin, handles.AdminCancel2FA)
	// Cache eviction is keyed by username, not id (v3 /user/del_cache); a
	// separate resource segment because gin forbids a second param name
	// (:username) at /users/:id's position.
	g.DELETE("/user_cache/:username", middlewares.RequireSystem("user", "manage"), handles.DelUserCache)

	roles := g.Group("/roles")
	roles.GET("", middlewares.RequireSystem("role", "view"), handles.ListRoles)
	roles.GET("/:id", middlewares.RequireSystem("role", "view"), handles.GetRole)
	roles.GET("/:id/permissions", middlewares.RequireSystem("role", "view"), handles.RolePermissions)
	roles.POST("", middlewares.RequireAdmin, handles.CreateRole)
	roles.PUT("/:id", middlewares.RequireAdmin, intoID, handles.UpdateRole)
	roles.DELETE("/:id", middlewares.RequireAdmin, handles.DeleteRole)
	roles.PUT("/:id/permissions", middlewares.RequireAdmin,
		middlewares.ParamIntoJSONBody("id", "role_id"), handles.SetRolePermissions)

	permissions := g.Group("/permissions")
	permissions.GET("/catalog", middlewares.RequireSystem("permission", "view"), handles.GetPermissionCatalog)
	permissions.GET("", middlewares.RequireSystem("permission", "view"), handles.ListPermissions)
	permissions.GET("/:id", middlewares.RequireSystem("permission", "view"), handles.GetPermission)
	permissions.GET("/:id/roles", middlewares.RequireSystem("permission", "view"), handles.PermissionRoles)
	permissions.POST("", middlewares.RequireAdmin, handles.CreatePermission)
	permissions.PUT("/:id", middlewares.RequireAdmin, intoID, handles.UpdatePermission)
	permissions.DELETE("/:id", middlewares.RequireAdmin, handles.DeletePermission)

	storages := g.Group("/storages")
	storages.GET("", middlewares.RequireSystem("storage", "view"), handles.ListStorages)
	storages.GET("/:id", middlewares.RequireSystem("storage", "view"), handles.GetStorage)
	storages.POST("", middlewares.RequireSystem("storage", "manage"), handles.CreateStorage)
	storages.PUT("/:id", middlewares.RequireSystem("storage", "manage"), intoID, handles.UpdateStorage)
	storages.DELETE("/:id", middlewares.RequireSystem("storage", "manage"), handles.DeleteStorage)
	storages.POST("/:id/enable", middlewares.RequireSystem("storage", "manage"), handles.EnableStorage)
	storages.POST("/:id/disable", middlewares.RequireSystem("storage", "manage"), handles.DisableStorage)
	storages.POST("/load_all", middlewares.RequireSystem("storage", "manage"), handles.LoadAllStorages)

	drivers := g.Group("/drivers", middlewares.RequireSystem("driver", "view"))
	drivers.GET("", handles.ListDriverInfo)
	drivers.GET("/names", handles.ListDriverNames)
	drivers.GET("/:driver", handles.GetDriverInfo)

	settings := g.Group("/settings")
	settings.GET("", middlewares.RequireSystem("setting", "view"), handles.ListSettings)
	settings.PUT("", middlewares.RequireSystem("setting", "manage"), handles.SaveSettings)
	// The admin API token is a settings-domain credential, not a signing key
	// (those have their own /signing_keys resource since Step 6); registered
	// before /:key so the literal segment wins the match.
	settings.GET("/token", middlewares.RequireAdmin, handles.GetToken)
	settings.PUT("/token", middlewares.RequireAdmin, handles.SetToken)
	settings.POST("/token/reset", middlewares.RequireAdmin, handles.ResetToken)
	// The ONLY endpoint that can save a site_theme value carrying custom JS
	// (SaveSettings above refuses one outright — see its doc comment and
	// site_theme.go's SetSiteTheme doc comment for the full HIGH-severity
	// red-team fix this closes). RequireAdmin, not RequireSystem("setting",
	// "manage") — deliberately the non-delegable control, since this can run
	// arbitrary JS for every visitor including anonymous ones. Registered
	// before /:key so the literal segment wins the match.
	settings.POST("/site-theme", middlewares.RequireAdmin, handles.SetSiteTheme)
	// First-run setup wizard status (reskin wave 4). Authenticated-admin only —
	// the wizard runs post-login, not as an unauthenticated installer flow —
	// so these stay behind the same auth middleware as the rest of /admin
	// rather than introducing any new unauthenticated setup surface.
	// Registered before /:key so the literal segments win the match.
	settings.GET("/setup", middlewares.RequireAdmin, handles.GetSetupStatus)
	settings.POST("/setup/complete", middlewares.RequireAdmin, handles.CompleteSetup)
	settings.GET("/:key", middlewares.RequireSystem("setting", "view"), handles.GetSetting)
	settings.DELETE("/:key", middlewares.RequireSystem("setting", "manage"), handles.DeleteSetting)

	handles.SetupTaskRoute(g.Group("/task", middlewares.RequireSystem("task", "view")))

	signingKeys := g.Group("/signing_keys")
	signingKeys.GET("", middlewares.RequireSystem("signing_key", "view"), handles.ListSigningKeys)
	signingKeys.POST("/rotate", middlewares.RequireSystem("signing_key", "manage"), handles.RotateSigningKey)
	signingKeys.POST("/gc", middlewares.RequireSystem("signing_key", "manage"), handles.GCSigningKeys)

	sessions := g.Group("/sessions")
	sessions.GET("", middlewares.RequireSystem("session", "view"), handles.ListSessions)
	sessions.DELETE("/:id", middlewares.RequireSystem("session", "manage"),
		middlewares.ParamIntoJSONBody("id", "session_id"), handles.EvictSession)

	// Search index management (子项目 A, wave A3). RequireAdmin, not delegable:
	// a build walks every storage as admin and stores full paths, so kicking one
	// off (or clearing/stopping it) stays on the admin control plane. Progress is
	// a read the same admins poll during a build.
	index := g.Group("/index", middlewares.RequireAdmin)
	index.POST("/build", handles.BuildIndex)
	index.POST("/update", handles.UpdateIndex)
	index.POST("/stop", handles.StopIndex)
	index.POST("/clear", handles.ClearIndex)
	index.GET("/progress", handles.GetProgress)

	// Audit trail (Step 12) is admin-only and not delegable: it exposes other
	// users' activity (IPs, paths), which no operator grant should unlock.
	g.GET("/audit", middlewares.RequireAdmin, handles.ListAuditLogs)

	// Backup/restore (Step 13b) writes and reads the whole configuration
	// surface (credentials included), so every verb is RequireAdmin and not
	// delegable to an operator grant. :name is validated against path traversal
	// in the handlers.
	backupG := g.Group("/backup", middlewares.RequireAdmin)
	backupG.POST("", handles.CreateBackup)
	backupG.GET("", handles.ListBackups)
	backupG.GET("/:name/download", handles.DownloadBackup)
	backupG.DELETE("/:name", handles.DeleteBackup)
	backupG.POST("/restore", handles.RestoreBackup)

	// Plugin lifecycle (Step 9.3). Every verb is RequireAdmin and not delegable:
	// installing/enabling plugins runs third-party code with storage-token
	// reach, so it stays on the admin control plane. Mutations are audited; the
	// index fetch is the only outbound call and only fires when an admin has
	// configured plugin_index_url (privacy baseline, Step 2a).
	plugins := g.Group("/plugins", middlewares.RequireAdmin)
	plugins.GET("", handles.ListPlugins)
	plugins.GET("/index", handles.GetPluginIndex)
	plugins.GET("/sources", handles.ListPluginSources)
	plugins.POST("/sources", handles.AddPluginSource)
	plugins.DELETE("/sources/:name", handles.DeletePluginSource)
	plugins.POST("/install", handles.InstallPlugin)
	plugins.GET("/:id/permissions", handles.GetPluginPermissions)
	plugins.POST("/:id/grant", handles.SetPluginGrant)
	plugins.GET("/:id/config", handles.GetPluginConfig)
	plugins.POST("/:id/config", handles.SetPluginConfig)
	// Plugin UI sandbox bridge (SP1): the controlled surface a sandboxed
	// plugin ui/ bundle reaches through the front-end hostBridge, clamped to
	// the plugin's approved Grant and audited (server/handles/plugin_ui.go).
	plugins.POST("/:id/scoped-token", handles.IssuePluginUiScopedToken)
	plugins.POST("/:id/sign-link", handles.SignPluginUiLink)
	plugins.GET("/:id/kv/:key", handles.GetPluginUiKv)
	plugins.PUT("/:id/kv/:key", handles.SetPluginUiKv)
	plugins.POST("/:id/enable", handles.EnablePlugin)
	plugins.POST("/:id/disable", handles.DisablePlugin)
	plugins.POST("/:id/update", handles.UpdatePlugin)
	plugins.POST("/:id/rollback", handles.RollbackPlugin)
	plugins.DELETE("/:id", handles.UninstallPlugin)
	// dev-only: hot-load an unsigned plugin straight from a local directory
	// (plugin dev CLI). RegisterDevPluginRoutes itself no-ops unless the
	// server is running with --dev; the route must not exist at all otherwise.
	handles.RegisterDevPluginRoutes(plugins)

	// Theme marketplace (theming spec §J5, final wave). A theme is a
	// declarative JSON descriptor with no code-execution risk beyond the
	// SAME per-theme js-trust gate every other theme source already goes
	// through (see server/handles/theme_market.go's module doc) — so unlike
	// /plugins above there is no manifest/gRPC lifecycle here, just
	// source management + catalog fetch + install/uninstall. Every verb is
	// RequireAdmin and not delegable, same posture as /plugins: importing a
	// source or installing a theme changes what every visitor's picker can
	// select. Mutations are audited (theme_market.source.add/remove,
	// .install/.update/.uninstall).
	themeMarket := g.Group("/theme-market", middlewares.RequireAdmin)
	themeMarket.GET("/sources", handles.ListThemeMarketSources)
	themeMarket.POST("/sources", handles.AddThemeMarketSource)
	themeMarket.DELETE("/sources/:name", handles.DeleteThemeMarketSource)
	themeMarket.GET("/sources/:name/catalog", handles.GetThemeMarketCatalog)
	themeMarket.POST("/install", handles.InstallThemeMarketTheme)
	themeMarket.GET("/installed", handles.ListInstalledThemeMarketThemes)
	// Keyed by (source, entry id) — the install identity (I5) — not by the
	// theme's own descriptor id, which is display-only.
	themeMarket.DELETE("/installed/:source/:entryId", handles.UninstallThemeMarketTheme)

	// Unified extension manager (Project A §2.5): the built-in half of the
	// plugin/extension page. The list merges host-registered built-ins (labels/
	// SSO/passkey/FRP…) with installed disk plugins; enable/disable here ONLY
	// accept built-ins (EnableBuiltin/DisableBuiltin reject a disk-plugin/unknown
	// id — disk plugins keep using /plugins/:id/{enable,disable} above). Same
	// RequireAdmin, not delegable: toggling a built-in changes what code runs and
	// what contributions surface, so it stays on the admin control plane. Distinct
	// from the authed, non-admin GET /extensions/contributions (extensionsV4) — a
	// different mount prefix (/admin/extensions), no route collision.
	exts := g.Group("/extensions", middlewares.RequireAdmin)
	exts.GET("", handles.ListManagedExtensions)
	exts.POST("/:id/enable", handles.EnableBuiltinExtension)
	exts.POST("/:id/disable", handles.DisableBuiltinExtension)

	// SSO provider management (federated OIDC login). Every verb is RequireAdmin
	// and not delegable: a provider holds a client secret and grants login
	// capability, so it stays on the admin control plane. client_secret is
	// write-only — never returned by the list endpoint.
	ssoProviders := g.Group("/sso", middlewares.RequireAdmin)
	ssoProviders.GET("/providers", handles.ListSSOProviders)
	ssoProviders.GET("/presets", handles.ListSSOPresets)
	ssoProviders.POST("/providers", handles.CreateSSOProvider)
	ssoProviders.PUT("/providers/:key", handles.UpdateSSOProvider)
	ssoProviders.DELETE("/providers/:key", handles.DeleteSSOProvider)
	ssoProviders.POST("/providers/:key/enable", handles.EnableSSOProvider)
	ssoProviders.POST("/providers/:key/disable", handles.DisableSSOProvider)

	// v3 exposed /fs/link inline-gated inside the user fs group; v4 moves it
	// under the admin mount so the gate placement is structural (spec: no
	// inline AuthAdmin exceptions).
	g.POST("/fs/link", handles.Link)

	// Remote agents (design W12-1 data/admin foundation + W12-2b SAS visual
	// MITM-confirmation, see internal/agent/{pairing,sas}.go and
	// server/handles/agent.go). RequireAdmin and not delegable, same posture
	// as /plugins and /sso above: pairing an agent, confirming its SAS, and
	// granting it a storage whitelist are all control-plane actions, not a
	// per-resource permission scope.
	agents := g.Group("/agents", middlewares.RequireAdmin)
	agents.POST("/pairing", handles.StartAgentPairing)
	// design F (token-authenticated reverse pairing) — a TWO-STEP flow: STEP 1
	// mints a one-time token the operator copies into the agent's pairing web,
	// STEP 2 dials the agent's public /agentpair endpoint keyed with that token
	// (looked up by token_id) and, on a passing mutual verify, persists an
	// AUTO-ENABLED reverse row (no separate SAS-confirm step — the token already
	// authenticated the agent).
	agents.POST("/pairing/reverse/token", handles.ReverseMintPairingToken)
	agents.POST("/pairing/reverse", handles.ReverseStartAgentPairing)
	agents.GET("", handles.ListAgents)
	agents.PATCH("/:id", handles.UpdateAgent)
	agents.DELETE("/:id", handles.DeleteAgent)
	// design W12-2b: the SAS-gated confirm path that actually activates a
	// freshly (re-)paired agent — see handles.ConfirmAgent's doc for why
	// this is deliberately separate from the PATCH {enabled:true} override
	// above.
	agents.GET("/:id/pairing-sas", handles.AgentPairingSAS)
	agents.POST("/:id/confirm", handles.ConfirmAgent)
	// design R4: live reverse-connection status for a reverse-mode agent
	// (connected/dialing/error + last-seen over the channel), tracked by the
	// server's reverse supervisor.
	agents.GET("/:id/reverse-status", handles.AgentReverseStatus)
	// design SP-G1: admin observability — tail a connected agent's recent logs
	// over the authenticated reverse channel (fails closed if not connected),
	// and list its dispatched tasks with live progress. Same admin gate as the
	// rest of /agents.
	agents.GET("/:id/logs", handles.AgentLogs)
	agents.GET("/:id/tasks", handles.AgentTasks)

	// Agent task visibility (design W12-4: reliability wave) — a read-only
	// admin view over db.ListAgentTasks, same admin gate as /agents above.
	// No frontend consumes this yet; the endpoint itself is the deliverable
	// this wave.
	g.GET("/agent-tasks", middlewares.RequireAdmin, handles.AdminListAgentTasks)
}
