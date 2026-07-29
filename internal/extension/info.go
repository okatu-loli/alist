package extension

import (
	"reflect"
	"strings"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/i18n"
)

// BuildDriverInfo builds the host-facing driver.Info (config form + static
// config) for a storage driver by reflecting over its Addition tags. Moved
// verbatim from internal/op so the registry owns driver-info assembly (the op
// package now delegates registration to the registry).
func BuildDriverInfo(config driver.Config, addition driver.Additional) driver.Info {
	tAddition := reflect.TypeOf(addition)
	for tAddition.Kind() == reflect.Pointer {
		tAddition = tAddition.Elem()
	}
	return driver.Info{
		Common:     getMainItems(config),
		Additional: getAdditionalItems(tAddition, config.DefaultRoot),
		Config:     config,
	}
}

func getMainItems(config driver.Config) []driver.Item {
	items := []driver.Item{{
		Name:     "mount_path",
		Type:     conf.TypeString,
		Required: true,
		Help:     "The path you want to mount to, it is unique and cannot be repeated",
	}, {
		Name: "order",
		Type: conf.TypeNumber,
		Help: "use to sort",
	}, {
		Name: "remark",
		Type: conf.TypeText,
	}}
	if !config.NoCache {
		items = append(items, driver.Item{
			Name:     "cache_expiration",
			Type:     conf.TypeNumber,
			Default:  "30",
			Required: true,
			Help:     "The cache expiration time for this storage",
		})
	}
	if !config.OnlyProxy && !config.OnlyLocal {
		items = append(items, []driver.Item{{
			Name: "web_proxy",
			Type: conf.TypeBool,
		}, {
			Name:     "webdav_policy",
			Type:     conf.TypeSelect,
			Options:  "302_redirect,use_proxy_url,native_proxy",
			Default:  "302_redirect",
			Required: true,
		},
		}...)
		if config.ProxyRangeOption {
			item := driver.Item{
				Name: "proxy_range",
				Type: conf.TypeBool,
				Help: "Need to enable proxy",
			}
			if config.Name == "139Yun" {
				item.Default = "true"
			}
			items = append(items, item)
		}
	} else {
		items = append(items, driver.Item{
			Name:     "webdav_policy",
			Type:     conf.TypeSelect,
			Default:  "native_proxy",
			Options:  "use_proxy_url,native_proxy",
			Required: true,
		})
	}
	items = append(items, driver.Item{
		Name: "down_proxy_url",
		Type: conf.TypeText,
	})
	items = append(items, driver.Item{
		Name:    "down_proxy_sign",
		Type:    conf.TypeBool,
		Default: "true",
	})
	if config.LocalSort {
		items = append(items, []driver.Item{{
			Name:    "order_by",
			Type:    conf.TypeSelect,
			Options: "name,size,modified",
		}, {
			Name:    "order_direction",
			Type:    conf.TypeSelect,
			Options: "asc,desc",
		}}...)
	}
	items = append(items, driver.Item{
		Name:    "extract_folder",
		Type:    conf.TypeSelect,
		Options: "front,back",
	})
	items = append(items, driver.Item{
		Name:     "disable_index",
		Type:     conf.TypeBool,
		Default:  "false",
		Required: true,
	})
	items = append(items, driver.Item{
		Name:     "enable_sign",
		Type:     conf.TypeBool,
		Default:  "false",
		Required: true,
	})
	return items
}

// mapExtItems copies ext.Item slice to driver.Item (field-identical structs
// plus the host-only Label, left empty here — localizeItems fills it — and
// Secret, derived rather than copied: ext.Item has no such field of its own
// (G3 §1 keeps the SDK untouched), so a plugin driver's secret Additional
// fields are recognized by the existing Type=="password" convention
// (already used by tools/123open-plugin/driver.go's client_secret/
// private_key fields) instead.
func mapExtItems(in []ext.Item) []driver.Item {
	if len(in) == 0 {
		return nil
	}
	out := make([]driver.Item, len(in))
	for i, it := range in {
		out[i] = driver.Item{Name: it.Name, Type: it.Type, Default: it.Default, Options: it.Options, Required: it.Required, Help: it.Help, Secret: it.Type == "password"}
	}
	return out
}

// localizeItems overlays each item's Label and Help with the plugin's bundled
// translation for locale, using the "field.<name>.label" / "field.<name>.help"
// key convention (plan doc ②, Task 1). Label is set only when the bundle
// carries one — there is no manifest-declared label to fall back to, so an
// unresolved Label stays "" and the frontend renders Name instead. Help falls
// back to the manifest's English default (already on the item from
// mapExtItems) when the bundle has no translation for it, so no plugin ever
// regresses to an empty Help. items is mutated in place and returned.
func localizeItems(items []driver.Item, locale string, bundle map[string]map[string]string) []driver.Item {
	for i := range items {
		prefix := "field." + items[i].Name
		if label := resolve(bundle, locale, prefix+".label"); label != "" {
			items[i].Label = label
		}
		if help := resolve(bundle, locale, prefix+".help"); help != "" {
			items[i].Help = help
		}
	}
	return items
}

// commonFieldKeyPrefix is the host i18n catalog namespace for the shared
// storage config fields (mount_path, order, ...). Keys are
// "storage.field.<name>.label" / "storage.field.<name>.help".
const commonFieldKeyPrefix = "storage.field."

// localizeCommonItems fills each host-generated common item's Label (and
// overrides its English Help when a translation exists) from the shared host
// i18n catalog (internal/i18n). Unlike a plugin's Additional items — which
// carry per-plugin bundled translations (localizeItems) — the common fields
// are identical across every driver, so their translations live in the host
// catalog keyed by "storage.field.<name>.label"/".help". The v4 frontend
// renders item.Label || item.Name and item.Help verbatim (no per-field-name
// dictionary of its own), so without this the common fields (mount_path, ...)
// would show raw snake_case names and English help in every locale, for both
// built-in and plugin drivers. items is mutated in place and returned.
func localizeCommonItems(items []driver.Item, locale string) []driver.Item {
	for i := range items {
		base := commonFieldKeyPrefix + items[i].Name
		if i18n.Has(i18n.DefaultLang, base+".label") {
			items[i].Label = i18n.T(locale, base+".label")
		}
		if i18n.Has(i18n.DefaultLang, base+".help") {
			items[i].Help = i18n.T(locale, base+".help")
		}
	}
	return items
}

// cloneItems returns a shallow copy of items (a fresh backing array of the
// same driver.Item values), so a per-request localization pass can mutate the
// copy without touching a cached driver.Info's Common slice.
func cloneItems(items []driver.Item) []driver.Item {
	if len(items) == 0 {
		return nil
	}
	out := make([]driver.Item, len(items))
	copy(out, items)
	return out
}

func extConfigToDriverConfig(c ext.DriverConfig) driver.Config {
	return driver.Config{
		Name: c.Name, LocalSort: c.LocalSort, OnlyLocal: c.OnlyLocal, OnlyProxy: c.OnlyProxy,
		NoCache: c.NoCache, NoUpload: c.NoUpload, DefaultRoot: c.DefaultRoot, CheckStatus: c.CheckStatus,
		Alert: c.Alert, NoOverwriteUpload: c.NoOverwriteUpload, ProxyRangeOption: c.ProxyRangeOption,
	}
}

// IsPathAnchored reports whether a plugin's declared config form marks it as
// a path-anchored storage driver (a "root_folder_path" Additional field),
// as opposed to the ext-contract default of ID-anchored (ObjRef.Anchor is an
// opaque driver-internal ID, typically declared via "root_folder_id").
// bridge.NewPluginDriver's caller (server/handles/plugins.go's
// SetPluginDriverFactory) uses this to decide which of ID/Path the plugin's
// synthesized root object should carry — see grpcdriver.go's GetRoot for why
// setting the wrong one breaks nested browsing on ID-anchored drivers.
func IsPathAnchored(m ext.Manifest) bool {
	if m.Info == nil {
		return false
	}
	for _, item := range m.Info.Additional {
		if item.Name == "root_folder_path" {
			return true
		}
	}
	return false
}

// BuildPluginDriverInfo synthesizes a driver.Info for a plugin's "add storage"
// form: the host generates the generic common fields (mount_path, order, …) from
// the declared DriverConfig, and copies the plugin-declared Additional items,
// localizing each item's Label/Help to locale using bundle (the plugin's
// loaded i18n/*.json locale files — see LoadPluginLocales). bundle may be nil
// or empty (a plugin with no bundled locales); every Additional item then
// keeps its manifest-declared English Help and an empty Label, exactly the
// pre-i18n behavior. Common items are host-generated and are NOT localized
// this wave (plan doc ② Task 1 scope).
func BuildPluginDriverInfo(m ext.Manifest, locale string, bundle map[string]map[string]string) driver.Info {
	if m.Info == nil {
		return driver.Info{}
	}
	cfg := extConfigToDriverConfig(m.Info.Config)
	return driver.Info{
		Common:     localizeCommonItems(getMainItems(cfg), locale),
		Additional: localizeItems(mapExtItems(m.Info.Additional), locale, bundle),
		Config:     cfg,
	}
}

func getAdditionalItems(t reflect.Type, defaultRoot string) []driver.Item {
	var items []driver.Item
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Type.Kind() == reflect.Struct {
			items = append(items, getAdditionalItems(field.Type, defaultRoot)...)
			continue
		}
		tag := field.Tag
		ignore, ok1 := tag.Lookup("ignore")
		name, ok2 := tag.Lookup("json")
		if (ok1 && ignore == "true") || !ok2 {
			continue
		}
		item := driver.Item{
			Name:     name,
			Type:     strings.ToLower(field.Type.Name()),
			Default:  tag.Get("default"),
			Options:  tag.Get("options"),
			Required: tag.Get("required") == "true",
			Help:     tag.Get("help"),
			// G3 §1: activates the confidential:"true" tag (drivers/crypt/
			// meta.go's Password/Salt) that nothing previously read — a
			// built-in driver's secret Additional fields are declared this
			// way, not via Type (that stays the Go field's own kind: string,
			// bool, ...).
			Secret: tag.Get("confidential") == "true",
		}
		if tag.Get("type") != "" {
			item.Type = tag.Get("type")
		}
		if item.Name == "root_folder_id" || item.Name == "root_folder_path" {
			if item.Default == "" {
				item.Default = defaultRoot
			}
			item.Required = item.Default != ""
		}
		// set default type to string
		if item.Type == "" {
			item.Type = "string"
		}
		items = append(items, item)
	}
	return items
}
