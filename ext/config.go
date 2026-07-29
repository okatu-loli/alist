package ext

// DriverConfig is a storage driver's static configuration, returned by
// StorageDriver.DriverConfig. It mirrors the fields v3 carried on
// internal/driver.Config that affect host behavior.
//
// 9.1 note: built-in drivers still expose their config through the legacy
// internal/driver.Config reflection path; DriverConfig is the frozen SDK shape
// future native (out-of-tree) drivers will use. Seam.
type DriverConfig struct {
	Name              string `json:"name"`
	LocalSort         bool   `json:"local_sort"`
	OnlyLocal         bool   `json:"only_local"`
	OnlyProxy         bool   `json:"only_proxy"`
	NoCache           bool   `json:"no_cache"`
	NoUpload          bool   `json:"no_upload"`
	DefaultRoot       string `json:"default_root"`
	CheckStatus       bool   `json:"check_status"`
	Alert             string `json:"alert,omitempty"`
	NoOverwriteUpload bool   `json:"no_overwrite_upload"`
	ProxyRangeOption  bool   `json:"proxy_range_option"`
}

// Item is one field of a driver's declarative config form.
type Item struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Default  string `json:"default,omitempty"`
	Options  string `json:"options,omitempty"`
	Required bool   `json:"required,omitempty"`
	Help     string `json:"help,omitempty"`
}

// Info is the host-facing description of a driver: the config form plus its
// static config.
type Info struct {
	Common     []Item       `json:"common"`
	Additional []Item       `json:"additional"`
	Config     DriverConfig `json:"config"`
}
