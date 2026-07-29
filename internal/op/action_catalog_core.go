package op

import "github.com/alist-org/alist/v3/internal/model"

// Core action registration runs at package init so the vocabulary is complete
// before the catalog API serves and before preset seeding (T6): both import op,
// so op's init() is guaranteed to run first. Registration touches no DB, so it
// is safe this early in startup.
func init() {
	if err := RegisterActions("core", coreActionDefs()...); err != nil {
		panic("op: registering core actions: " + err.Error())
	}
}

// fsActions is the 18-verb RBAC5 filesystem vocabulary (model.Action constants).
// These stay bare-named and remain bound to the LegacyBits bridge.
var fsActions = []model.Action{
	model.ActRead, model.ActSeeHides, model.ActAccessWithoutPwd, model.ActOfflineDownload,
	model.ActWrite, model.ActRename, model.ActMove, model.ActCopy, model.ActRemove,
	model.ActWebdavRead, model.ActWebdavManage, model.ActFTPAccess, model.ActFTPManage,
	model.ActReadArchives, model.ActDecompress, model.ActPathLimit, model.ActMCPAccess, model.ActMCPManage,
}

// systemDomainDefs enumerates the system control-plane domains and which verbs
// they expose (spec §3.2). Every domain gets view; all but driver get manage
// (driver is read-only — it exposes no mutating control-plane surface).
// manageDanger flags the manage verb as authorization-graph / blast-radius
// sensitive (role/permission = authz graph, signing_key = all direct links).
var systemDomainDefs = []struct {
	name         string
	hasManage    bool
	manageDanger bool
}{
	{"storage", true, false},
	{"user", true, false},
	{"role", true, true},
	{"permission", true, true},
	{"driver", false, false},
	{"setting", true, false},
	{"signing_key", true, true},
	{"session", true, false},
	{"meta", true, false},
	{"offline_download", true, false},
	{"task", true, false},
}

// coreActionDefs materializes the core vocabulary: 18 fs verbs + each system
// domain's view/manage. manage implies view (same domain) so granting manage
// never leaves a "granted manage but GET 403" foot-gun (§6.1 F4).
func coreActionDefs() []ActionDef {
	defs := make([]ActionDef, 0, len(fsActions)+len(systemDomainDefs)*2)
	for _, a := range fsActions {
		defs = append(defs, ActionDef{
			Name:         a,
			ResourceType: model.ResourceFS,
			Verb:         string(a),
			I18nKey:      "perm.action.fs." + string(a),
			Source:       "core",
		})
	}
	for _, d := range systemDomainDefs {
		defs = append(defs, ActionDef{
			Name:         model.SystemAction(d.name, "view"),
			ResourceType: model.ResourceSystem,
			Domain:       d.name,
			Verb:         "view",
			I18nKey:      "perm.action.system." + d.name + ".view",
			Source:       "core",
		})
		if d.hasManage {
			defs = append(defs, ActionDef{
				Name:         model.SystemAction(d.name, "manage"),
				ResourceType: model.ResourceSystem,
				Domain:       d.name,
				Verb:         "manage",
				Implies:      []model.Action{model.SystemAction(d.name, "view")},
				I18nKey:      "perm.action.system." + d.name + ".manage",
				Danger:       d.manageDanger,
				Source:       "core",
			})
		}
	}
	return defs
}
