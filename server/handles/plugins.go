package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/bridge"
	"github.com/alist-org/alist/v3/internal/extension/pluginproc"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/net"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// init wires the extension Manager's admin trust policy to the plugin_policy
// setting. internal/extension may not import internal/setting (import cycle), so
// the server layer — which may — installs the provider here. It is read live, so
// an admin changing plugin_policy takes effect on the next install/enable.
//
// It also wires GetStorageDriver's plugin branch: an enabled gRPC plugin entry
// is turned into a driver.Driver via bridge.NewPluginDriver, sharing the
// process-wide pluginproc.Default manager so every storage mounted on the same
// plugin ID reuses one subprocess.
//
// It also wires GetAuthProvider's plugin branch (WS-8): an enabled
// Kind=AuthProvider gRPC plugin entry is dispensed through the same
// pluginproc.Default manager, sharing its launch/reuse/refcount discipline
// with storage-driver plugin processes.
//
// It also wires DispatchAPIRoute's plugin branch (W8a): an enabled
// Kind=APIRoute gRPC plugin entry is dispensed through the same
// pluginproc.Default manager, mirroring the AuthProvider wiring one Kind over.
//
// It also wires InvokeExtensionAction's gRPC branch (W8b): an enabled
// Kind=Action gRPC plugin entry with no in-process invoker is dispensed
// through the same pluginproc.Default manager, mirroring the APIRoute wiring
// one Kind over.
//
// It also wires DispatchEvent's and the Scheduler's gRPC branches (W7): an
// enabled Kind=EventHook / Kind=ScheduledJob gRPC plugin entry with no
// in-process instance is dispensed through the same pluginproc.Default
// manager, mirroring the APIRoute/Action wiring one Kind over.
//
// It also wires DispatchNotification's gRPC branch (W7b): an enabled
// Kind=NotificationChannel gRPC plugin entry with no in-process instance is
// dispensed through the same pluginproc.Default manager, mirroring the
// EventHook wiring one Kind over.
//
// It also wires the FileDecorator and AccessProtocol (TransportHTTP) gRPC
// branches (W9a): an enabled Kind=FileDecorator / Kind=AccessProtocol gRPC
// plugin entry with no in-process instance is dispensed through the same
// pluginproc.Default manager, mirroring the EventHook/APIRoute wiring one
// Kind over. TransportConn AccessProtocol plugins (FTP/SFTP) never reach this
// factory — they are native/in-process only (design §5).
//
// It also wires resolveOfflineDownloadTool's gRPC branch (Phase E ring 1): an
// enabled Kind=OfflineDownloadTool gRPC plugin entry with no in-process
// instance is dispensed through the same pluginproc.Default manager,
// mirroring the Notification wiring one Kind over.
//
// It also wires resolveSearchIndexer's gRPC branch (Phase E ring 1b): an
// enabled Kind=SearchIndexer gRPC plugin entry with no in-process instance is
// dispensed through the same pluginproc.Default manager, mirroring the
// OfflineDownloadTool wiring one Kind over.
//
// It also wires SubmitAssist's gRPC branch (assist gRPC leg, Task 2): a
// declared assist form with no in-process AssistProvider is dispensed through
// the same pluginproc.Default manager, but into its own assistProcs pool
// (Manager.AcquireAssist) rather than sharing the entry's primary-Kind
// process — the pool is dedicated so an impl already Init'd on some other
// Kind's shared subprocess is never Init'd a second time (design note, assist
// gRPC leg plan, Task 2).
func init() {
	extension.SetPolicyProvider(func() extension.Policy {
		return extension.ParsePolicy(setting.GetStr(conf.PluginPolicy))
	})
	extension.SetPluginDriverFactory(func(e *extension.Entry) driver.Driver {
		return bridge.NewPluginDriver(e.ID(), e.BinPath(), e.Capabilities(), extension.IsPathAnchored(e.Manifest), pluginproc.Default)
	})
	extension.SetAuthProviderFactory(func(e *extension.Entry) (ext.AuthProvider, func(), error) {
		return pluginproc.Default.AcquireAuthProvider(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetAuthNMethodFactory(func(e *extension.Entry) (ext.AuthNMethod, func(), error) {
		return pluginproc.Default.AcquireAuthNMethod(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetBackgroundServiceFactory(func(e *extension.Entry) (ext.BackgroundService, func(), error) {
		return pluginproc.Default.AcquireBackgroundService(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetAPIRouteFactory(func(e *extension.Entry) (ext.APIRoute, func(), error) {
		return pluginproc.Default.AcquireAPIRoute(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetActionFactory(func(e *extension.Entry) (ext.Action, func(), error) {
		return pluginproc.Default.AcquireAction(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetAssistFactory(func(e *extension.Entry) (ext.Assist, func(), error) {
		return pluginproc.Default.AcquireAssist(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetEventHookFactory(func(e *extension.Entry) (ext.EventHook, func(), error) {
		return pluginproc.Default.AcquireEventHook(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetScheduledJobFactory(func(e *extension.Entry) (ext.ScheduledJob, func(), error) {
		return pluginproc.Default.AcquireScheduledJob(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetNotificationFactory(func(e *extension.Entry) (ext.Notification, func(), error) {
		return pluginproc.Default.AcquireNotification(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetFileDecoratorFactory(func(e *extension.Entry) (ext.FileDecorator, func(), error) {
		return pluginproc.Default.AcquireFileDecorator(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetAccessProtocolFactory(func(e *extension.Entry) (ext.AccessProtocol, func(), error) {
		return pluginproc.Default.AcquireAccessProtocol(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetOfflineDownloadToolFactory(func(e *extension.Entry) (ext.OfflineDownloadTool, func(), error) {
		return pluginproc.Default.AcquireOfflineDownloadTool(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetSearchIndexerFactory(func(e *extension.Entry) (ext.SearchIndexer, func(), error) {
		return pluginproc.Default.AcquireSearchIndexer(context.Background(), e.ID(), e.BinPath())
	})
	extension.SetArchiveToolFactory(func(e *extension.Entry) (ext.ArchiveTool, func(), error) {
		return pluginproc.Default.AcquireArchiveTool(context.Background(), e.ID(), e.BinPath())
	})
}

// PluginInstallReq points at a plugin package as either a local .tar.gz path or
// a download URL. Exactly one is used (path wins if both are set). A URL install
// is an explicit admin action, so it is exempt from the default-no-outbound
// baseline (Step 2a).
type PluginInstallReq struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	// Grant is the admin-approved consent selection to record for the plugin
	// (design §6.4 consent submit). Optional: when omitted, install records the
	// default Grant (approve the manifest's requests). A wizard-driven install
	// submits the admin's actual selection here so the runtime authority reflects
	// exactly what was approved, not what the manifest declared.
	Grant *extension.Grant `json:"grant,omitempty"`
}

// PluginGrantReq carries an admin's approved consent Grant for a post-install
// adjust (design §6.5 "permissions & network" settings slot / §6.7 POST /grant).
type PluginGrantReq struct {
	Grant *extension.Grant `json:"grant"`
	// Acknowledged confirms the admin accepts the risk of granting the plugin
	// network hosts BEYOND what its manifest declared. Required (true) whenever
	// the submitted grant's network set contains a host the manifest did not
	// request; ignored otherwise. Only network may exceed the manifest.
	Acknowledged bool `json:"acknowledged"`
}

// pluginStateView is the admin-facing shape of extension.PluginState:
// identical in every field except Config, which here ALWAYS holds the
// redacted value (extension.RedactSecretConfigFields) — never the raw
// stored one PluginState.Config carries. G3 review round 1 (CRITICAL):
// ListPlugins and installOrUpdate used to return mgr.List()/mgr.Get()
// straight through common.SuccessResp, so "list installed plugins" and
// "install/update response" both echoed a plugin's x-secret config_form
// values in full — entirely bypassing GetPluginConfig's redaction, which
// only ever guarded its own endpoint. Wrapping in a dedicated view type
// (rather than redacting a mutated PluginState copy in place) closes this
// off structurally, not just at these two call sites: there is no
// raw-Config-bearing value for a FUTURE plugin-info handler to reach for by
// mistake — toPluginStateView is the only way to produce one of these, and
// it always redacts.
type pluginStateView struct {
	ID              string            `json:"id"`
	Kind            ext.Kind          `json:"kind"`
	Name            map[string]string `json:"name,omitempty"`
	ActiveVersion   string            `json:"active_version"`
	PreviousVersion string            `json:"previous_version,omitempty"`
	Versions        []string          `json:"versions"`
	Enabled         bool              `json:"enabled"`
	Tier            string            `json:"tier,omitempty"`
	ExecModeName    string            `json:"exec_mode,omitempty"`
	InstalledAt     time.Time         `json:"installed_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Grant           *extension.Grant  `json:"grant,omitempty"`
	NeedsReconsent  bool              `json:"needs_reconsent,omitempty"`
	Config          json.RawMessage   `json:"config,omitempty"`
}

// toPluginStateView redacts ps.Config (when non-empty — an empty Config
// stays empty, preserving PluginState.Config's existing omitempty "no
// config key at all when nothing was ever saved" shape) and copies every
// other field through unchanged.
func toPluginStateView(mgr *extension.Manager, ps extension.PluginState) pluginStateView {
	cfg := ps.Config
	if len(cfg) > 0 {
		cfg = extension.RedactSecretConfigFields(cfg, mgr.SecretConfigFields(ps.ID))
	}
	return pluginStateView{
		ID:              ps.ID,
		Kind:            ps.Kind,
		Name:            ps.Name,
		ActiveVersion:   ps.ActiveVersion,
		PreviousVersion: ps.PreviousVersion,
		Versions:        ps.Versions,
		Enabled:         ps.Enabled,
		Tier:            ps.Tier,
		ExecModeName:    ps.ExecModeName,
		InstalledAt:     ps.InstalledAt,
		UpdatedAt:       ps.UpdatedAt,
		Grant:           ps.Grant,
		NeedsReconsent:  ps.NeedsReconsent,
		Config:          cfg,
	}
}

// ListPlugins returns every installed plugin and its lifecycle state.
// @Summary List installed plugins
// @Description Lists installed plugins with their active/previous version and enabled state. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[[]pluginStateView]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/plugins [get]
func ListPlugins(c *gin.Context) {
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	list := mgr.List()
	views := make([]pluginStateView, len(list))
	for i, ps := range list {
		views[i] = toPluginStateView(mgr, ps)
	}
	common.SuccessResp(c, views)
}

// InstallPlugin verifies and installs a plugin package.
// @Summary Install plugin
// @Description Installs a plugin from a local .tar.gz path or a URL. The package signature and checksums are verified against the trusted roots (official key + imported plugin sources) before anything is written into place; a tampered package is rejected outright, an unsigned one classifies as third-party and must pass the risk confirmation wizard. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body PluginInstallReq true "package location"
// @Success 200 {object} common.Resp[pluginStateView]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/install [post]
func InstallPlugin(c *gin.Context) {
	installOrUpdate(c, false)
}

// UpdatePlugin installs a new version of an installed plugin and switches to it.
// @Summary Update plugin
// @Description Installs a new version alongside the current one and switches the active version to it, keeping the previous version for rollback. Same verification as install. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "plugin id"
// @Param body body PluginInstallReq true "package location"
// @Success 200 {object} common.Resp[pluginStateView]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/update [post]
func UpdatePlugin(c *gin.Context) {
	installOrUpdate(c, true)
}

func installOrUpdate(c *gin.Context, isUpdate bool) {
	user := c.MustGet(ctxkey.User).(*model.User)
	action := "plugin.install"
	if isUpdate {
		action = "plugin.update"
	}
	var req PluginInstallReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	path := req.Path
	if path == "" {
		if req.URL == "" {
			common.ErrorStrResp(c, "one of path or url is required", 400)
			return
		}
		var cleanup func()
		path, cleanup, err = downloadPackage(c.Request.Context(), req.URL)
		if err != nil {
			audit.Event(c, user.ID, user.Username, action, "plugin:", false)
			common.ErrorResp(c, err, 400)
			return
		}
		defer cleanup()
	}
	var man ext.Manifest
	if isUpdate {
		man, err = mgr.Update(path)
	} else {
		man, err = mgr.Install(path)
	}
	if err != nil {
		audit.Event(c, user.ID, user.Username, action, "plugin:", false)
		common.ErrorResp(c, err, 400)
		return
	}
	// Record the admin's consent selection, when supplied (design §6.4). Install
	// already stored the default (approve-manifest) Grant, so an omitted grant
	// leaves that in place; a supplied grant replaces it with exactly what the
	// admin approved — the runtime authority. A malformed grant fails the whole
	// install/update loudly rather than silently leaving default consent.
	if req.Grant != nil {
		// Install/update never allows an over-manifest network grant: a wizard
		// submitting the admin's actual selection here can only narrow, never
		// widen, what the manifest declares. Widening beyond the manifest is a
		// separate, explicit, risk-acknowledged act — the post-install grant
		// endpoint below.
		if err := mgr.SetGrant(man.ID, req.Grant, false); err != nil {
			audit.Event(c, user.ID, user.Username, action, "plugin:"+man.ID+"@"+man.Version, false)
			common.ErrorResp(c, err, 400)
			return
		}
	}
	audit.Event(c, user.ID, user.Username, action, "plugin:"+man.ID+"@"+man.Version, true)
	st, _ := mgr.Get(man.ID)
	common.SuccessResp(c, toPluginStateView(mgr, st))
}

// GetPluginPermissions returns a plugin's declared (requested) permissions and
// its current approved Grant.
// @Summary Get plugin permissions
// @Description Returns the {requested, granted} permission document for an installed plugin (design §6.7): Requested from the active version's manifest, Granted the persisted admin-approved consent Grant, and needs_reconsent flagging an update that asked for more than the current Grant approves. This is the data source for both the install consent page and the post-install "permissions & network" settings slot. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "plugin id"
// @Success 200 {object} common.Resp[extension.PermissionView]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/permissions [get]
func GetPluginPermissions(c *gin.Context) {
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	view, err := mgr.PermissionView(c.Param("id"))
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	common.SuccessResp(c, view)
}

// SetPluginGrant records an admin's updated consent Grant for an installed
// plugin.
// @Summary Update plugin permission grant
// @Description Replaces a plugin's approved consent Grant wholesale with the submitted set (design §6.5): revoke a capability, narrow the network allowlist, or re-approve after an update. Takes effect immediately — the runtime dispatch gate reads this Grant on every dispatch. Clears needs_reconsent. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "plugin id"
// @Param body body PluginGrantReq true "approved consent grant"
// @Success 200 {object} common.Resp[extension.PermissionView]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/grant [post]
func SetPluginGrant(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	var req PluginGrantReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// Detect network hosts the submitted grant asks for beyond the manifest's
	// declared allowlist. Those require an explicit risk acknowledgement; without
	// it we refuse rather than silently dropping them, so the UI can prompt.
	view, err := mgr.PermissionView(id)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	declared := map[string]bool{}
	for _, h := range view.Requested.Network {
		declared[h] = true
	}
	var extra []string
	if req.Grant != nil {
		for _, h := range req.Grant.Network {
			if !declared[h] {
				extra = append(extra, h)
			}
		}
	}
	if len(extra) > 0 && !req.Acknowledged {
		audit.Event(c, user.ID, user.Username, "plugin.grant", "plugin:"+id, false)
		common.ErrorResp(c, fmt.Errorf(
			"granting network hosts beyond the plugin manifest (%s) requires acknowledged=true",
			strings.Join(extra, ", ")), 400)
		return
	}
	if err := mgr.SetGrant(id, req.Grant, req.Acknowledged); err != nil {
		audit.Event(c, user.ID, user.Username, "plugin.grant", "plugin:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	// Evict any cached non-bg subprocess so it relaunches bound to the new Grant
	// (the network allowlist is closed over at launch). Best-effort: a no-op when
	// nothing is cached, or when the plugin isn't a registered (enabled)
	// StorageDriver entry at all; bg services were already rebound inside
	// SetGrant.
	if entry, ok := extension.DefaultRegistry.Get(id); ok {
		_ = pluginproc.Default.Evict(c.Request.Context(), id, entry.BinPath())
	}
	detail := "plugin:" + id
	if len(extra) > 0 {
		detail += " +net:" + strings.Join(extra, ",")
	}
	audit.Event(c, user.ID, user.Username, "plugin.grant", detail, true)
	out, err := mgr.PermissionView(id)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	common.SuccessResp(c, out)
}

// PluginConfigReq is the body of POST /admin/plugins/:id/config.
type PluginConfigReq struct {
	Config json.RawMessage `json:"config"`
}

// GetPluginConfig returns the admin-saved config_form values for an installed
// plugin (an empty object when none saved). Admin only. Stage 1: values are
// stored/served for the settings UI; the plugin cannot read them back yet.
// @Summary Get plugin config
// @Description Returns the admin-saved config_form values for an installed plugin (design "settings" slot, stage 1): stored and served for the admin UI only — the plugin cannot read it back yet (that needs the reverse host bridge, a later wave). Empty object when nothing has been saved. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "plugin id"
// @Success 200 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/config [get]
func GetPluginConfig(c *gin.Context) {
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	id := c.Param("id")
	cfg, err := mgr.GetConfig(id)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}
	// G3 §1: never echo a stored x-secret config_form field back in full —
	// see extension.RedactSecretConfigFields.
	cfg = extension.RedactSecretConfigFields(cfg, mgr.SecretConfigFields(id))
	common.SuccessResp(c, gin.H{"config": cfg})
}

// SetPluginConfig persists the admin-supplied config_form values. Admin only.
// @Summary Set plugin config
// @Description Persists the admin-supplied config_form values for an installed plugin (design "settings" slot, stage 1: stored/served for the admin UI only, no reverse-bridge read-back or subprocess rebind). Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "plugin id"
// @Param body body PluginConfigReq true "config_form values"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/config [post]
func SetPluginConfig(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	var req PluginConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// [Minor 10] Check the CALLER-SUBMITTED req.Config here, before the merge
	// below folds the old (possibly genuinely-enc:v1:-prefixed) stored value
	// in for a blank field — merging first would make a legitimate
	// already-ciphertext value indistinguishable from a fresh value that
	// merely collides with the reserved prefix (see
	// crypto.RejectReservedPrefix's doc comment).
	if err := extension.RejectReservedPrefixConfig(req.Config, mgr.SecretConfigFields(id)); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// G3 §1: a blank x-secret field in the incoming config means "leave it
	// as-is" — the admin UI never received the real value back
	// (RedactSecretConfigFields), so it can only ever resubmit blank for a
	// field it didn't deliberately change. Mirrors
	// op.MergeStorageAdditionSecrets / UpdateSSOProvider's identical rule for
	// client_secret. oldCfg's own error is ignored: a not-installed id fails
	// the exact same way at SetConfig below, and any other failure just
	// means no fallback value exists (best-effort merge).
	oldCfg, _ := mgr.GetConfig(id)
	cfg := extension.MergeSecretConfigFields(oldCfg, req.Config, mgr.SecretConfigFields(id))
	if err := mgr.SetConfig(id, cfg); err != nil {
		audit.Event(c, user.ID, user.Username, "plugin.config", "plugin:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	audit.Event(c, user.ID, user.Username, "plugin.config", "plugin:"+id, true)
	cfg, _ = mgr.GetConfig(id)
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}
	cfg = extension.RedactSecretConfigFields(cfg, mgr.SecretConfigFields(id))
	common.SuccessResp(c, gin.H{"config": cfg})
}

// EnablePlugin registers the plugin's active version and marks it enabled.
// @Summary Enable plugin
// @Description Registers the plugin's active version with the extension registry (gRPC execution mode) and marks it enabled. Admin only. Calling this API directly implies informed admin consent; the tier risk interstitial is enforced by the install wizard flow, not at this endpoint.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "plugin id"
// @Success 200 {object} common.Resp[string]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/enable [post]
func EnablePlugin(c *gin.Context) {
	pluginStateChange(c, "plugin.enable", func(m *extension.Manager, id string) error { return m.Enable(id) })
}

// DisablePlugin removes the plugin from the registry and marks it disabled.
// @Summary Disable plugin
// @Description Removes the plugin from the extension registry and marks it disabled. On-disk versions are kept. Refused with 409 if storages are still mounted on the plugin, unless ?force=true (which disables those storages first). Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "plugin id"
// @Param force query bool false "cascade-disable mounted storages first"
// @Success 200 {object} common.Resp[string]
// @Failure 400 {object} common.Resp[any]
// @Failure 409 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/disable [post]
func DisablePlugin(c *gin.Context) {
	if !guardPluginStorageDeps(c, c.Param("id"), "plugin.disable") {
		return
	}
	pluginStateChange(c, "plugin.disable", func(m *extension.Manager, id string) error { return m.Disable(id) })
}

// RollbackPlugin switches the active version back to the previous one.
// @Summary Rollback plugin
// @Description Switches the active version back to the previously-active version kept from the last update. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "plugin id"
// @Success 200 {object} common.Resp[string]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/rollback [post]
func RollbackPlugin(c *gin.Context) {
	pluginStateChange(c, "plugin.rollback", func(m *extension.Manager, id string) error { return m.Rollback(id) })
}

// UninstallPlugin removes a plugin's versions and state.
// @Summary Uninstall plugin
// @Description Deregisters the plugin and removes all its on-disk versions and its state entry. Refused with 409 if storages are still mounted on the plugin, unless ?force=true (which disables those storages first). Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "plugin id"
// @Param force query bool false "cascade-disable mounted storages first"
// @Success 200 {object} common.Resp[string]
// @Failure 400 {object} common.Resp[any]
// @Failure 409 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id} [delete]
func UninstallPlugin(c *gin.Context) {
	if !guardPluginStorageDeps(c, c.Param("id"), "plugin.uninstall") {
		return
	}
	pluginStateChange(c, "plugin.uninstall", func(m *extension.Manager, id string) error { return m.Uninstall(id) })
}

func pluginStateChange(c *gin.Context, action string, fn func(*extension.Manager, string) error) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if err := fn(mgr, id); err != nil {
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	audit.Event(c, user.ID, user.Username, action, "plugin:"+id, true)
	common.SuccessResp(c, "ok")
}

// mountedStorageDep is the wire shape of one storage reported as a blocker in
// a 409 "plugin has mounted storages" response.
type mountedStorageDep struct {
	ID        uint   `json:"id"`
	MountPath string `json:"mount_path"`
	Driver    string `json:"driver"`
}

// pluginStorages returns every live storage mounted on the plugin identified
// by id (matched by driver name == plugin id), so Disable/Uninstall can
// refuse to strand them.
func pluginStorages(id string) []*model.Storage {
	var out []*model.Storage
	for _, d := range op.GetAllStorages() {
		if s := d.GetStorage(); s != nil && s.Driver == id {
			out = append(out, s)
		}
	}
	return out
}

// guardPluginStorageDeps is the shared dependency guard for DisablePlugin and
// UninstallPlugin: a plugin backing live storages cannot be disabled or
// uninstalled out from under them. Without ?force=true it refuses with 409
// and the list of dependent storages (id/mount_path/driver) so the admin UI
// can show exactly what is blocking the operation. With force=true it
// cascades: every dependent storage is disabled first, then the caller's
// state change proceeds. Returns true when it is clear for the caller to
// proceed, false when a response has already been written (409 conflict or a
// cascade failure) and the caller must stop. action is the caller's audit
// action name ("plugin.disable"/"plugin.uninstall"), recorded against both
// failure paths below to match installOrUpdate/pluginStateChange's
// audit-every-failure convention.
func guardPluginStorageDeps(c *gin.Context, id, action string) bool {
	deps := pluginStorages(id)
	if len(deps) == 0 {
		return true
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	if c.Query("force") != "true" {
		mounts := make([]string, len(deps))
		list := make([]mountedStorageDep, len(deps))
		for i, s := range deps {
			mounts[i] = s.MountPath
			list[i] = mountedStorageDep{ID: s.ID, MountPath: s.MountPath, Driver: s.Driver}
		}
		err := i18n.Error(i18n.Lang(c), "plugin.has_mounted_storages", nil, "mounts", strings.Join(mounts, ", "))
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorWithDataResp(c, err, 409, gin.H{"storages": list})
		return false
	}
	for _, s := range deps {
		// Not transactional: if DisableStorage fails partway through the
		// cascade, the storages disabled so far stay disabled and the rest
		// remain live — a partial-disable state, reported below as a single
		// 500. Retrying is safe: the next attempt only sees the storages
		// still live and disables those. Wrapping this in a transaction is
		// out of scope here.
		if err := op.DisableStorage(c, s.ID); err != nil {
			audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
			common.ErrorResp(c, err, 500)
			return false
		}
	}
	return true
}

// PluginIndexResp is the aggregated browse surface plus per-source fetch
// warnings (a dead source degrades, it does not blank the list).
type PluginIndexResp struct {
	Entries  []extension.IndexEntry `json:"entries"`
	Warnings []string               `json:"warnings,omitempty"`
}

// GetPluginIndex fetches and merges the official plugin index and every
// imported source's index.
// @Summary Browse plugin index
// @Description Aggregates the official plugin index (when configured) with every imported source's index; entries carry their source name. Returns 400 when neither an official index URL nor any imported source is configured. Fetched only on explicit admin request (privacy baseline). Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[PluginIndexResp]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/index [get]
func GetPluginIndex(c *gin.Context) {
	officialURL := setting.GetStr(conf.PluginIndexURL)
	if officialURL == "" && len(extension.CurrentSources()) == 0 {
		common.ErrorStrResp(c, "no plugin source is configured", 400)
		return
	}
	entries, warns := extension.AggregateIndex(c.Request.Context(), officialURL)
	common.SuccessResp(c, PluginIndexResp{Entries: entries, Warnings: warns})
}

// downloadPackage fetches a plugin .tar.gz to a temp file for install. Returns
// the temp path and a cleanup func. Only invoked on an explicit admin install
// with a URL, so it does not breach the default-no-outbound baseline.
func downloadPackage(ctx context.Context, url string) (string, func(), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", func() {}, err
	}
	resp, err := net.HttpClient().Do(req)
	if err != nil {
		return "", func() {}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", func() {}, fmt.Errorf("download package: unexpected status %d", resp.StatusCode)
	}
	f, err := os.CreateTemp("", "alist-plugin-*.tar.gz")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 256<<20)); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	f.Close()
	return f.Name(), cleanup, nil
}
