package handles

import (
	"errors"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// pluginAssetExts is the closed set of file types a theme plugin may serve under
// /plugin-assets/<id>/ (theming spec §5): the token JSON, the restricted CSS, its
// bundled woff2 fonts, and same-origin images a css block may reference. HTML/JS
// are DELIBERATELY excluded so a plugin can never get executable or navigable
// content served from the app's own origin (defense-in-depth alongside nosniff).
var pluginAssetExts = map[string]bool{
	".json": true, ".css": true, ".woff2": true,
	".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true, ".avif": true,
}

// oauthCallbackURL builds the host's own OAuth callback URL (WS-8) from the
// incoming request, reusing common.GetApiUrl so it honors conf.Conf.SiteURL
// when configured and otherwise derives scheme+host from the request itself
// (X-Forwarded-* aware) — the same base every other absolute URL in the
// codebase (e.g. driver-relative Link completion) is built from.
func oauthCallbackURL(c *gin.Context) string {
	return common.GetApiUrl(c.Request) + "/api/v4/extensions/oauth/callback"
}

// ExtensionContributions returns the merged front-end contributions of every
// enabled extension.
// @Summary Aggregate extension contributions
// @Description Merges the declarative front-end contributions (config forms, menu actions, previewers, ui panels, dashboard cards, ...) of every ENABLED extension into one versioned document. Built-in drivers appear with an empty contributions block. The output changes as plugins are installed/enabled/disabled/uninstalled. Requires a logged-in user.
// @Tags extension
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[extension.ContributionsDoc]
// @Router /v4/extensions/contributions [get]
func ExtensionContributions(c *gin.Context) {
	common.SuccessResp(c, extension.AggregateContributions(extension.DefaultRegistry))
}

// ExtensionActionReq carries the arguments passed to an extension menu action.
type ExtensionActionReq struct {
	Args map[string]any `json:"args"`
}

// InvokeExtensionAction routes a menu-action call to an extension's handler.
// @Summary Invoke an extension action
// @Description Routes POST to an extension's declared menu action. Returns 404 if the extension is not enabled or did not declare the action id. Admin only — the conservative default until per-action permission scopes land (Step 11).
// @Tags extension
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "extension id"
// @Param aid path string true "action id"
// @Param body body ExtensionActionReq false "action arguments"
// @Success 200 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Failure 501 {object} common.Resp[any]
// @Router /v4/extensions/{id}/actions/{aid} [post]
func InvokeExtensionAction(c *gin.Context) {
	var req ExtensionActionReq
	_ = c.ShouldBindJSON(&req) // body optional; a global action may take no args
	user := c.MustGet(ctxkey.User).(*model.User)
	result, err := extension.InvokeExtensionAction(c.Request.Context(), extension.DefaultRegistry,
		ext.Principal(user.Username), c.Param("id"), c.Param("aid"), req.Args)
	if err != nil {
		if errors.Is(err, extension.ErrActionNotDeclared) {
			common.ErrorResp(c, err, 404)
			return
		}
		if code, ok := ext.CodeOf(err); ok && code == ext.ErrCodeNotImplement {
			common.ErrorResp(c, err, 501)
			return
		}
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, result)
}

// ExtensionAssistReq carries the input submitted to an extension assist form.
type ExtensionAssistReq struct {
	Input map[string]any `json:"input"`
}

// SubmitExtensionAssist routes a field-level assist-form submission to an
// extension's handler.
// @Summary Submit an extension assist form
// @Description Routes POST to an extension's declared assist form (SDK §4.1, x-assist). Returns 404 if the extension is not enabled or did not declare the form id. Admin only — the conservative default until per-action permission scopes land (Step 11).
// @Tags extension
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "extension id"
// @Param fid path string true "assist form id"
// @Param body body ExtensionAssistReq false "assist form input"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Failure 501 {object} common.Resp[any]
// @Router /v4/extensions/{id}/assist/{fid} [post]
func SubmitExtensionAssist(c *gin.Context) {
	var req ExtensionAssistReq
	_ = c.ShouldBindJSON(&req) // body optional; an assist form may take no input
	user := c.MustGet(ctxkey.User).(*model.User)
	result, err := extension.SubmitAssist(c.Request.Context(), extension.DefaultRegistry,
		ext.Principal(user.Username), c.Param("id"), c.Param("fid"), req.Input)
	if err != nil {
		if errors.Is(err, extension.ErrAssistNotDeclared) {
			common.ErrorResp(c, err, 404)
			return
		}
		if errors.Is(err, extension.ErrAssistInvalidInput) {
			common.ErrorResp(c, err, 400)
			return
		}
		if code, ok := ext.CodeOf(err); ok && code == ext.ErrCodeNotImplement {
			common.ErrorResp(c, err, 501)
			return
		}
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, result)
}

// ExtensionServiceStatus returns the runtime status of a Kind=BackgroundService
// extension for the frontend status card (design §2.4, §4 StatusProvider). A
// StatusProvider contribution points its PollPath here.
// @Summary Poll a background-service extension's status
// @Description Resolves the extension's BackgroundService.Status() (running state, detail, metrics) for the services status card. 404 if the id is not an enabled background service; 501 if it has no host-side handler wired.
// @Tags extension
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "extension id"
// @Param sid path string true "status provider id"
// @Success 200 {object} common.Resp[ext.ServiceStatus]
// @Failure 404 {object} common.Resp[any]
// @Failure 501 {object} common.Resp[any]
// @Router /v4/extensions/{id}/status/{sid} [get]
func ExtensionServiceStatus(c *gin.Context) {
	status, err := extension.ServiceStatusOf(c.Request.Context(), extension.DefaultRegistry, c.Param("id"))
	if err != nil {
		if code, ok := ext.CodeOf(err); ok && code == ext.ErrCodeNotImplement {
			common.ErrorResp(c, err, 501)
			return
		}
		common.ErrorResp(c, err, 404)
		return
	}
	common.SuccessResp(c, status)
}

// ExtensionUI serves a file from an installed extension's iframe bundle.
// @Summary Serve extension UI bundle file
// @Description Serves a static file from the active version's ui/ subdir, for the sandboxed iframe host (SDK §5). Path traversal is rejected. Responses carry Content-Security-Policy: sandbox and X-Content-Type-Options: nosniff so the bundle runs in an opaque origin.
// @Tags extension
// @Security ApiKeyAuth
// @Param id path string true "extension id"
// @Param filepath path string true "file path within ui/"
// @Success 200 {file} file
// @Failure 404 {object} common.Resp[any]
// @Router /v4/extensions/{id}/ui/{filepath} [get]
func ExtensionUI(c *gin.Context) {
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	baseDir, err := mgr.UIDir(c.Param("id"))
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	target, err := ResolveExtensionUIFile(baseDir, c.Param("filepath"))
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	// The bundle is untrusted third-party UI: sandbox it (opaque origin, scripts
	// only) and forbid mime sniffing so a mislabeled asset can't be run as HTML
	// outside the iframe.
	c.Header("Content-Security-Policy", "sandbox allow-scripts")
	c.Header("X-Content-Type-Options", "nosniff")
	c.File(target)
}

// PluginAsset serves a read-only theme asset (token JSON, restricted css,
// bundled woff2, or a css-referenced image) from an ENABLED plugin's active
// version dir, under /plugin-assets/<id>/<path> (theming spec §5.4). It is the
// same-origin backing for a theme's rewritten url()/@font-face src.
//
// Deliberately ANONYMOUS: the browser loads these as subresources of a css
// url()/@font-face — requests that carry no Authorization header — so an authed
// route could never satisfy them. That is safe: only ENABLED plugins are
// reachable, the file type is allowlisted (no HTML/JS), path traversal is
// rejected (ResolveUIFile), and theme assets hold no secrets.
// @Summary Serve a plugin theme asset
// @Description Read-only static file from an enabled plugin's active version dir, for theme token/css/font/image assets. Anonymous (loaded as css subresources). File type is allowlisted (json/css/woff2/images); path traversal and non-enabled plugins are rejected.
// @Tags extension
// @Param id path string true "extension id"
// @Param filepath path string true "asset path within the plugin dir"
// @Success 200 {file} file
// @Failure 404 {object} common.Resp[any]
// @Router /../plugin-assets/{id}/{filepath} [get]
func PluginAsset(c *gin.Context) {
	id := c.Param("id")
	// Real HTTP statuses (not the 200+JSON envelope): these are browser
	// subresource loads (css url()/@font-face src), so a 404 must be an actual
	// 404, and a served asset must be the raw file with its own content-type.
	//
	// Only enabled plugins expose assets — mirrors the frontend, which only
	// registers enabled plugins' themes. DefaultRegistry.Get returns enabled
	// entries; a disabled/uninstalled plugin's assets are simply 404.
	if _, ok := extension.DefaultRegistry.Get(id); !ok {
		c.AbortWithStatus(404)
		return
	}
	mgr, err := extension.DefaultManager()
	if err != nil {
		c.AbortWithStatus(500)
		return
	}
	baseDir, err := mgr.AssetDir(id)
	if err != nil {
		c.AbortWithStatus(404)
		return
	}
	target, err := ResolveExtensionUIFile(baseDir, c.Param("filepath"))
	if err != nil {
		c.AbortWithStatus(404)
		return
	}
	dot := strings.LastIndex(target, ".")
	if dot < 0 || !pluginAssetExts[strings.ToLower(target[dot:])] {
		c.AbortWithStatus(404)
		return
	}
	// nosniff so a mislabeled asset can't be reinterpreted (e.g. as HTML) in the
	// app origin; long cache since a plugin's active-version assets are immutable.
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "public, max-age=15552000")
	// Defense-in-depth for the one allowlisted type that can carry script: an
	// SVG served same-origin executes its inline <script> if a victim navigates
	// DIRECTLY to it (nosniff does not stop that — the type is already correct).
	// The app-document CSP is not applied to this route, so set an asset-local,
	// script-killing CSP here. `sandbox` (no allow-scripts) + default-src 'none'
	// neuters top-level execution while leaving subresource use (background-image
	// url(), @font-face src) unaffected — those are governed by the app page's
	// own CSP (img-src/font-src 'self'), not the asset response's.
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.File(target)
}

// ResolveExtensionUIFile is a thin wrapper over extension.ResolveUIFile so the
// handler-layer test can exercise the traversal gate without importing the
// internal package's unexported helpers.
func ResolveExtensionUIFile(baseDir, reqPath string) (string, error) {
	return extension.ResolveUIFile(baseDir, reqPath)
}

// StartExtensionWizard opens an install-wizard session for an extension.
// @Summary Start extension install wizard
// @Description Opens a declarative install-wizard session driven by the extension's Manifest.Wizard and returns the first step. Admin only. Returns 404 if the extension is not installed or declares no wizard.
// @Tags extension
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "extension id"
// @Success 200 {object} common.Resp[extension.WizardStepView]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/extensions/{id}/wizard/start [post]
func StartExtensionWizard(c *gin.Context) {
	steps, err := resolveWizardSteps(c.Param("id"))
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	view, err := extension.DefaultWizardEngine.Start(c.Request.Context(), c.Param("id"), steps, oauthCallbackURL(c))
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, view)
}

// ExtensionWizardNext advances an install-wizard session.
// @Summary Advance extension install wizard
// @Description Submits the current step's answers and returns the next step or a done marker. A bad form answer / missing consent / failed probe re-prompts the same step with an error. An oauth step (WS-8) re-prompts itself with a "waiting" note (poll again) until the admin's browser completes the vendor authorization and hits the oauth callback route; once that has recorded a token, Next advances past it like any other step. Admin only.
// @Tags extension
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "extension id"
// @Param session path string true "wizard session id"
// @Param body body map[string]interface{} false "step answers"
// @Success 200 {object} common.Resp[extension.WizardStepView]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/extensions/{id}/wizard/{session}/next [post]
func ExtensionWizardNext(c *gin.Context) {
	var answers map[string]any
	_ = c.ShouldBindJSON(&answers) // some steps (info/test) take no answers
	view, err := extension.DefaultWizardEngine.Next(c.Request.Context(), c.Param("session"), answers, oauthCallbackURL(c))
	if err != nil {
		if code, ok := ext.CodeOf(err); ok && code == ext.ErrCodeNotImplement {
			common.ErrorResp(c, err, 501)
			return
		}
		if errors.Is(err, extension.ErrWizardSessionNotFound) || errors.Is(err, extension.ErrWizardSessionExpired) {
			common.ErrorResp(c, err, 404)
			return
		}
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, view)
}

// ExtensionOAuthCallback receives the vendor's OAuth redirect and completes
// the wizard's in-flight oauth step (WS-8). It is deliberately anonymous
// (registered outside the auth-required v4 group): the redirect is a raw
// top-level browser navigation from the vendor, not a fetch() from the SPA, so
// it carries no Authorization header. state — validated against the pending
// AuthURL request the wizard minted — is the sole access control here, exactly
// as any standard OAuth callback: an attacker without a valid, unused,
// unexpired state learns nothing and can complete no exchange.
//
// @Summary OAuth callback for an extension install wizard
// @Description Validates state against the wizard session that requested it, then exchanges code for a token by calling the AuthProvider plugin directly — locally, never through an external broker — and records the token as that oauth step's answer. Does not itself advance the wizard session; the frontend's main tab still calls wizard/next to continue once it learns the callback completed.
// @Tags extension
// @Produce json
// @Param code query string true "authorization code"
// @Param state query string true "anti-CSRF state minted by the wizard's oauth step"
// @Success 200 {object} common.Resp[extension.WizardStepView]
// @Failure 400 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/extensions/oauth/callback [get]
func ExtensionOAuthCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")
	view, err := extension.DefaultWizardEngine.HandleOAuthCallback(c.Request.Context(), code, state)
	if err != nil {
		if errors.Is(err, extension.ErrOAuthStateInvalid) {
			common.ErrorResp(c, err, 400)
			return
		}
		if errors.Is(err, extension.ErrWizardSessionNotFound) || errors.Is(err, extension.ErrWizardSessionExpired) {
			common.ErrorResp(c, err, 404)
			return
		}
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, view)
}

// resolveWizardSteps loads an extension's declared wizard steps and runs them
// through BuildWizardSteps, which strips plugin-declared risk steps and — for
// any non-official install — injects a tier-appropriate, non-skippable risk
// interstitial. So a 🔴/🟡 plugin's wizard always begins with the risk page
// and a 🟢 plugin's never shows one, regardless of what the manifest declared.
//
// Source of truth for the manifest+tier: an ENABLED registry entry (built-ins
// and already-enabled plugins) is trusted — it passed install gating already,
// so its tier is not third-party and it needs no risk page; its manifest comes
// straight from the registry without touching the on-disk install manager. Only
// a not-yet-enabled, disk-installed package is resolved through the manager,
// which classifies its real trust tier (this is the pre-enable install wizard).
func resolveWizardSteps(id string) ([]ext.WizardStep, error) {
	if e, ok := extension.DefaultRegistry.Get(id); ok {
		// An enabled entry already passed install-time trust gating; treat it as
		// official for wizard purposes (no risk interstitial on a re-run wizard
		// of an installed, enabled extension).
		steps := extension.BuildWizardSteps(e.Manifest.Wizard.Steps, extension.TierOfficial, "", e.Manifest)
		if len(steps) == 0 {
			return nil, errors.New("extension declares no wizard")
		}
		return steps, nil
	}
	mgr, err := extension.DefaultManager()
	if err != nil {
		return nil, err
	}
	tier, source, man, err := mgr.ActiveTier(id)
	if err != nil {
		return nil, err
	}
	steps := extension.BuildWizardSteps(man.Wizard.Steps, tier, source, man)
	if len(steps) == 0 {
		return nil, errors.New("extension declares no wizard")
	}
	return steps, nil
}
