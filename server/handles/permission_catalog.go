package handles

import (
	"sort"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// catalogDomain is one system control-plane domain the editor renders in the
// system resource-type's domain dropdown.
type catalogDomain struct {
	Name    string `json:"name"`
	I18nKey string `json:"i18n_key"`
}

// catalogResourceType describes a selectable resource type: fs is path-based
// (path + recursive), system is domain-based (a domain dropdown, no path).
type catalogResourceType struct {
	Type      string          `json:"type"`
	I18nKey   string          `json:"i18n_key"`
	PathBased bool            `json:"path_based"`
	Domains   []catalogDomain `json:"domains,omitempty"`
}

type catalogResp struct {
	Generation    int                   `json:"generation"`
	ResourceTypes []catalogResourceType `json:"resource_types"`
	Actions       []op.CatalogAction    `json:"actions"`
}

// GetPermissionCatalog returns the action vocabulary the permission editor
// renders from (spec §3.3): a generation counter, the selectable resource types
// (fs path-based, system with its domain list), and every action definition
// including implies/danger/source and orphaned flags.
// @Summary Permission action catalog
// @Description Returns the action vocabulary (resource types, system domains, and all action definitions incl. implies/danger/source/orphaned) the permission editor renders from.
// @Tags admin-permission
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/permission/catalog [get]
// @Router /v4/admin/permissions/catalog [get]
func GetPermissionCatalog(c *gin.Context) {
	snap, err := op.ActionCatalog()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	// Derive the system domain list from the registered system actions so
	// plugin-contributed domains appear automatically.
	seen := map[string]struct{}{}
	var domains []catalogDomain
	for _, a := range snap.Actions {
		if a.Orphaned || a.ResourceType != model.ResourceSystem || a.Domain == "" {
			continue
		}
		if _, ok := seen[a.Domain]; ok {
			continue
		}
		seen[a.Domain] = struct{}{}
		domains = append(domains, catalogDomain{Name: a.Domain, I18nKey: "perm.domain." + a.Domain})
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].Name < domains[j].Name })

	common.SuccessResp(c, catalogResp{
		Generation: snap.Generation,
		ResourceTypes: []catalogResourceType{
			{Type: model.ResourceFS, I18nKey: "perm.resource.fs", PathBased: true},
			{Type: model.ResourceSystem, I18nKey: "perm.resource.system", PathBased: false, Domains: domains},
		},
		Actions: snap.Actions,
	})
}
