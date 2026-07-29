package handles

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// sourcesMu serializes the read-modify-write of the plugin_sources setting.
// AddPluginSource and DeletePluginSource both read the current source list,
// mutate it in memory, and write the whole list back; without a lock,
// concurrent admin requests race and one's change can silently overwrite the
// other's.
var sourcesMu sync.Mutex

// init wires the extension trust layer's imported-source list to the
// plugin_sources setting, read live (same import-cycle pattern as the
// plugin_policy provider in plugins.go). A malformed setting yields zero
// sources — fail closed, no trust granted.
func init() {
	extension.SetSourcesProvider(func() []extension.PluginSource {
		ss, err := extension.ParseSources(setting.GetStr(conf.PluginSources))
		if err != nil {
			return nil
		}
		return ss
	})
}

// PluginSourceView is one entry of the sources list: an imported source, or
// the built-in official source (embedded trust root + plugin_index_url).
type PluginSourceView struct {
	extension.PluginSource
	Builtin bool `json:"builtin"`
}

// AddPluginSourceResp reports a successful add plus an optional reachability
// warning (the index probe is best-effort and never blocks the add).
type AddPluginSourceResp struct {
	Sources []PluginSourceView `json:"sources"`
	Warning string             `json:"warning,omitempty"`
}

// ListPluginSources lists the built-in official source and all imported ones.
// @Summary List plugin sources
// @Description Lists the built-in official plugin source and every admin-imported source (name, index URL, public key). Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[[]PluginSourceView]
// @Router /v4/admin/plugins/sources [get]
func ListPluginSources(c *gin.Context) {
	common.SuccessResp(c, sourceViews())
}

func sourceViews() []PluginSourceView {
	views := []PluginSourceView{{
		PluginSource: extension.PluginSource{
			Name:     extension.OfficialSourceName,
			IndexURL: setting.GetStr(conf.PluginIndexURL),
			PubKey:   extension.OfficialPublicKeyHex(),
		},
		Builtin: true,
	}}
	for _, s := range extension.CurrentSources() {
		views = append(views, PluginSourceView{PluginSource: s})
	}
	return views
}

// AddPluginSource imports a plugin source.
// @Summary Add plugin source
// @Description Imports a plugin source: a registry index URL plus the ed25519 public key its packages are signed with. Packages signed by this key install as tier-verified (🟡) with a source-attribution warning. The index is probed best-effort; an unreachable index yields a warning, not an error. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body extension.PluginSource true "source to import"
// @Success 200 {object} common.Resp[AddPluginSourceResp]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/sources [post]
func AddPluginSource(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	var src extension.PluginSource
	if err := c.ShouldBindJSON(&src); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := src.Validate(); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	sourcesMu.Lock()
	current := extension.CurrentSources()
	for _, s := range current {
		if strings.EqualFold(s.Name, src.Name) {
			sourcesMu.Unlock()
			common.ErrorStrResp(c, "a source with this name already exists", 400)
			return
		}
	}
	err := saveSources(append(current, src))
	sourcesMu.Unlock()
	if err != nil {
		audit.Event(c, user.ID, user.Username, "plugin.source.add", "source:"+src.Name, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "plugin.source.add", "source:"+src.Name, true)
	// Best-effort reachability probe: admin-triggered, so it does not breach the
	// no-default-outbound baseline; failure is a warning, never a rollback.
	warning := ""
	probeCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if _, err := extension.FetchIndex(probeCtx, src.IndexURL); err != nil {
		warning = "source saved, but its index could not be fetched: " + err.Error()
	}
	common.SuccessResp(c, AddPluginSourceResp{Sources: sourceViews(), Warning: warning})
}

// DeletePluginSource removes an imported plugin source.
// @Summary Remove plugin source
// @Description Removes an imported plugin source by name. Installed plugins are kept, but on their next enable they are re-classified against the remaining trust roots (a plugin vouched only by the removed source drops to third-party). The built-in official source cannot be removed. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param name path string true "source name"
// @Success 200 {object} common.Resp[[]PluginSourceView]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/plugins/sources/{name} [delete]
func DeletePluginSource(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	name := c.Param("name")
	if name == extension.OfficialSourceName {
		common.ErrorStrResp(c, "the built-in official source cannot be removed", 400)
		return
	}
	sourcesMu.Lock()
	current := extension.CurrentSources()
	kept := make([]extension.PluginSource, 0, len(current))
	for _, s := range current {
		if s.Name != name {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(current) {
		sourcesMu.Unlock()
		common.ErrorStrResp(c, "source not found", 404)
		return
	}
	err := saveSources(kept)
	sourcesMu.Unlock()
	if err != nil {
		audit.Event(c, user.ID, user.Username, "plugin.source.remove", "source:"+name, false)
		common.ErrorResp(c, err, 500)
		return
	}
	audit.Event(c, user.ID, user.Username, "plugin.source.remove", "source:"+name, true)
	common.SuccessResp(c, sourceViews())
}

// saveSources persists the source list to the plugin_sources setting.
// op.SaveSettingItem is a whole-row upsert, so it must be seeded from the
// existing setting item (preserving its Help text and Index ordering set by
// the seed data) rather than constructing a bare item with only Key/Value,
// which would silently wipe those fields on first save.
func saveSources(ss []extension.PluginSource) error {
	b, err := json.Marshal(ss)
	if err != nil {
		return err
	}
	item, err := op.GetSettingItemByKey(conf.PluginSources)
	if err != nil || item == nil {
		item = &model.SettingItem{
			Key:  conf.PluginSources,
			Type: conf.TypeText, Group: model.GLOBAL, Flag: model.PRIVATE,
		}
	} else {
		clone := *item
		item = &clone
	}
	item.Value = string(b)
	return op.SaveSettingItem(item)
}
