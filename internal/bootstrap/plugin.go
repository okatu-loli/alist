package bootstrap

import (
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// LoadPlugins re-registers every plugin marked enabled in
// <DataDir>/plugins/state.json with the extension Registry. It MUST run before
// LoadStorages: the plugin Manager is built lazily (extension.DefaultManager,
// on the first admin API call) and only reads state.json on construction — it
// does not touch the Registry on its own — so without this call a restarted
// server has no driver registered for a plugin-backed storage until an admin
// happens to hit the plugin API and re-enables it by hand, and any storage
// using that driver fails to load with "no driver named" in the meantime.
//
// Failures are logged and never abort boot: a Manager that fails to build, or
// one or more plugins that fail to re-register (missing binary, tier now
// disallowed by a tightened plugin_policy, ...), must not prevent the rest of
// the server — including other, healthy storages — from starting.
func LoadPlugins() {
	mgr, err := extension.DefaultManager()
	if err != nil {
		utils.Log.Warnf("extension: failed to build plugin manager at boot: %+v", err)
		return
	}
	if err := mgr.LoadEnabled(); err != nil {
		utils.Log.Warnf("extension: failed to load enabled plugins at boot: %+v", err)
	}
}
