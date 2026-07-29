package bootstrap

import (
	"encoding/json"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// settingDisabledStore is the setting-backed extension.DisabledStore: it persists
// the disabled built-in id set as a JSON string array in the
// conf.DisabledBuiltinExtensions setting. It lives here (not in
// internal/extension) because reading/writing settings goes through internal/op,
// which already imports internal/extension — putting the store there would cycle
// (see internal/extension/builtin_manage.go).
type settingDisabledStore struct{}

func (settingDisabledStore) Load() ([]string, error) {
	raw := setting.GetStr(conf.DisabledBuiltinExtensions, "[]")
	if raw == "" {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func (settingDisabledStore) Save(ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return op.SaveSettingItem(&model.SettingItem{
		Key:   conf.DisabledBuiltinExtensions,
		Value: string(b),
		Type:  conf.TypeText,
		Group: model.SITE,
		Flag:  model.PRIVATE,
	})
}

// InitBuiltinManage wires the setting-backed disabled-store into the process-wide
// built-in manager and re-applies the persisted disabled set. It MUST run after
// every Init* that registers a built-in extension (InitPasskey/InitLabels/
// InitFrp) so all built-ins exist, and before StartBackgroundServices so the
// supervisor never starts a service the admin turned off. A load failure is
// logged, not fatal: a bad persisted value must not block boot (every built-in
// simply stays enabled).
func InitBuiltinManage() {
	extension.SetDisabledStore(settingDisabledStore{})
	if err := extension.ApplyDisabledBuiltins(); err != nil {
		utils.Log.Errorf("failed to apply disabled built-in extensions: %+v", err)
	}
}
