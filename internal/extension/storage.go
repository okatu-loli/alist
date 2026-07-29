package extension

import (
	"fmt"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
)

// RegisterStorageDriver registers a built-in storage driver as an extension
// (Kind=StorageDriver, constructor mode). It subsumes the v3
// op.RegisterDriver/driverMap path: the driver's implemented capabilities are
// derived by reflection and recorded on a synthesized Manifest, and its config
// form (driver.Info) is built from the Addition tags.
//
// For built-ins the declared capability set equals the implemented set, so
// validation always passes; the loud-failure path (9.0 §4) is reachable through
// Register with a hand-written mismatched Manifest.
func (r *Registry) RegisterStorageDriver(constructor DriverConstructor) error {
	tmp := constructor()
	config := tmp.Config()
	if config.Name == "" {
		return fmt.Errorf("extension: storage driver with empty config name")
	}
	caps := DeriveStorageCapabilities(tmp)
	entry := &Entry{
		Manifest: ext.Manifest{
			ID:           config.Name,
			Name:         map[string]string{"en": config.Name},
			Version:      "builtin",
			CoreAPI:      "builtin",
			Kinds:        []ext.Kind{ext.KindStorageDriver},
			Capabilities: caps,
			Provenance:   ext.Provenance{Source: "official", License: "builtin"},
		},
		Kind:        ext.KindStorageDriver,
		Mode:        InstanceModeConstructor,
		implemented: caps,
		constructor: constructor,
		info:        BuildDriverInfo(config, tmp.GetAddition()),
		enabled:     true,
	}
	return r.Register(entry)
}

// pluginDriverFactory builds a driver.Driver for an enabled gRPC plugin entry.
// Registered by the server layer (SetPluginDriverFactory) to avoid an import
// cycle: internal/extension cannot import internal/extension/bridge+pluginproc
// at the storage.go level without pulling go-plugin into every extension build;
// the provider idiom matches SetPolicyProvider/SetSourcesProvider.
var pluginDriverFactory func(e *Entry) driver.Driver

// SetPluginDriverFactory installs the provider that turns an enabled gRPC
// plugin Entry into a driver.Driver constructor. The server layer calls this
// once at init (server/handles/plugins.go), wiring bridge.NewPluginDriver in
// without internal/extension importing bridge/pluginproc directly.
func SetPluginDriverFactory(fn func(e *Entry) driver.Driver) { pluginDriverFactory = fn }

// GetStorageDriver returns the constructor for a storage driver by name, or an
// error if none is registered/enabled under that name. Built-in drivers
// (constructor mode) are returned directly; an enabled gRPC plugin entry is
// returned as a constructor that defers to pluginDriverFactory, one plugin
// process shared across every mounted storage of that plugin.
func (r *Registry) GetStorageDriver(name string) (DriverConstructor, error) {
	r.mu.RLock()
	e, ok := r.byID[name]
	var constructor DriverConstructor
	var execMode ExecMode
	var binPath string
	if ok {
		ok = e.Kind == ext.KindStorageDriver && e.enabled
		constructor = e.constructor
		execMode = e.execMode
		binPath = e.binPath
	}
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no driver named: %s", name)
	}
	if constructor != nil {
		return constructor, nil // built-in
	}
	if execMode == ExecGRPC && binPath != "" && pluginDriverFactory != nil {
		entry := e
		return func() driver.Driver { return pluginDriverFactory(entry) }, nil
	}
	return nil, fmt.Errorf("no driver named: %s", name)
}

// StorageDriverNames returns the names of all enabled built-in storage drivers.
func (r *Registry) StorageDriverNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for id, e := range r.byKind[ext.KindStorageDriver] {
		if e.enabled {
			names = append(names, id)
		}
	}
	return names
}

// StorageDriverInfoMap returns the driver.Info of every enabled storage
// driver, keyed by name. This is the replacement for v3's op.driverInfoMap.
// Plugin-backed entries are localized to DefaultLocale (English) — callers
// that need a request-negotiated locale must use StorageDriverInfoMapLocale.
func (r *Registry) StorageDriverInfoMap() map[string]driver.Info {
	return r.StorageDriverInfoMapLocale(DefaultLocale)
}

// StorageDriverInfoMapLocale returns the driver.Info of every enabled storage
// driver, keyed by name, with plugin-backed drivers' Additional item
// Label/Help localized to locale from that plugin's bundled i18n/ locale
// files (Task ②: per-plugin bundled locale files). Built-in drivers
// (constructor != nil) are host-generated in English and are unaffected by
// locale — their cached Entry.info is returned as-is, matching pre-i18n
// behavior exactly (zero regression). Plugin entries are rebuilt on every
// call rather than cached per-locale: config forms are requested rarely
// (admin "add storage" UI, not a hot path), so the reflection-free rebuild
// (a slice copy + map lookups) is not worth the cache-invalidation
// complexity a per-locale cache would add.
func (r *Registry) StorageDriverInfoMapLocale(locale string) map[string]driver.Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]driver.Info)
	for id, e := range r.byKind[ext.KindStorageDriver] {
		if !e.enabled {
			continue
		}
		if e.constructor == nil { // plugin-backed: no in-process constructor
			out[id] = BuildPluginDriverInfo(e.Manifest, locale, e.bundle)
		} else {
			// Built-in: Entry.info is cached English (Common Label empty). The
			// Additional items stay English (built-in drivers have no bundled
			// locales), but the shared common fields (mount_path, ...) are
			// localized from the host catalog on a copy of Common so the cached
			// info is never mutated.
			info := e.info
			info.Common = localizeCommonItems(cloneItems(info.Common), locale)
			out[id] = info
		}
	}
	return out
}
