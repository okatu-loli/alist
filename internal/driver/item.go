package driver

type Additional interface{}

type Select string

type Item struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Default  string `json:"default"`
	Options  string `json:"options"`
	Required bool   `json:"required"`
	Help     string `json:"help"`

	// Label is the localized display label for this field, resolved from a
	// plugin's bundled i18n/<lang>.json locale files (per-plugin bundled
	// locales, Task ②). Empty for every built-in driver (they render Name
	// as-is) and for plugin fields with no matching bundle entry — the
	// frontend falls back to Name when Label is empty. json:"label,omitempty"
	// keeps existing driver.Info payloads byte-identical when unset, so this
	// is a zero-regression addition for built-in drivers.
	Label string `json:"label,omitempty"`

	// Secret marks this field as holding a credential a saved storage/plugin
	// config must never echo back in full once stored (G3 §1: secret as a
	// first-class citizen, step 1 — declare + read-back sentinel only, no
	// encryption yet). For a built-in (Go-struct, reflection-based) driver
	// this is populated from the `confidential:"true"` struct tag
	// (getAdditionalItems, internal/extension/info.go — previously declared
	// on drivers/crypt/meta.go's Password/Salt fields but never read by
	// anything); for a plugin-backed driver it is derived from the existing
	// `Type == "password"` convention (mapExtItems, same file — zero SDK
	// change, ext.Item gets no new field). json:"secret,omitempty" keeps
	// every existing driver.Info payload byte-identical for a driver with no
	// secret fields.
	Secret bool `json:"secret,omitempty"`
}

type Info struct {
	Common     []Item `json:"common"`
	Additional []Item `json:"additional"`
	Config     Config `json:"config"`
}

type IRootPath interface {
	GetRootPath() string
}

type IRootId interface {
	GetRootId() string
}

type RootPath struct {
	RootFolderPath string `json:"root_folder_path"`
}

type RootID struct {
	RootFolderID string `json:"root_folder_id"`
}

func (r RootPath) GetRootPath() string {
	return r.RootFolderPath
}

func (r *RootPath) SetRootPath(path string) {
	r.RootFolderPath = path
}

func (r RootID) GetRootId() string {
	return r.RootFolderID
}
