package op

import (
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension"
)

// DriverConstructor builds a fresh storage driver instance.
type DriverConstructor = extension.DriverConstructor

// RegisterDriver registers a built-in storage driver. Registration now flows
// through the unified ExtensionRegistry (Kind=StorageDriver, constructor mode);
// the old package-level driverMap/driverInfoMap are subsumed by it (9.1). This
// keeps the driver-init blank-import contract (drivers/all.go) unchanged.
func RegisterDriver(constructor DriverConstructor) {
	if err := extension.DefaultRegistry.RegisterStorageDriver(constructor); err != nil {
		// A built-in driver whose declared capabilities do not match its
		// implementation is a programming error: fail loudly at startup.
		panic(err)
	}
}

func GetDriver(name string) (DriverConstructor, error) {
	return extension.DefaultRegistry.GetStorageDriver(name)
}

func GetDriverNames() []string {
	return extension.DefaultRegistry.StorageDriverNames()
}

func GetDriverInfoMap() map[string]driver.Info {
	return extension.DefaultRegistry.StorageDriverInfoMap()
}

// GetDriverInfoMapLocale is GetDriverInfoMap localized to locale: plugin-backed
// drivers' Additional item Label/Help are resolved from that plugin's bundled
// i18n/ locale files (Task ②); built-in drivers are unaffected by locale.
func GetDriverInfoMapLocale(locale string) map[string]driver.Info {
	return extension.DefaultRegistry.StorageDriverInfoMapLocale(locale)
}
