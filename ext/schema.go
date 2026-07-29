package ext

// JSONSchema is the declarative form/field description shared by config forms,
// wizard form steps, and assist sub-forms (SDK doc §4). It is a deliberately
// small subset of JSON Schema: enough for the host's wizard engine to validate
// answers and for the frontend to render a form. Unknown JSON keys are ignored
// on decode, so a manifest may carry richer schema the frontend understands
// without the host needing to model it.
type JSONSchema struct {
	Type       string                 `json:"type,omitempty"` // object|string|number|integer|boolean
	Title      map[string]string      `json:"title,omitempty"`
	Properties map[string]*JSONSchema `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
	Enum       []string               `json:"enum,omitempty"`
	Default    string                 `json:"default,omitempty"`
	Minimum    *float64               `json:"minimum,omitempty"`
	Maximum    *float64               `json:"maximum,omitempty"`

	// DependsOn / Assist are the SDK §4.1 extension keywords. The host carries
	// them verbatim to the frontend (page-zero assist); it does not act on them.
	DependsOn *DependsOn `json:"x-depends-on,omitempty"`
	Assist    *AssistRef `json:"x-assist,omitempty"`

	// Secret marks this property as holding a credential (G3 §1: secret as a
	// first-class citizen, step 1 — declare + read-back sentinel only, no
	// encryption yet). A plugin's config_form uses this to mark a
	// GetPluginConfig/SetPluginConfig field the host must never echo back in
	// full once saved — the storage-driver Additional-item equivalent is the
	// existing Type=="password" convention (ext.Item has no such flag of its
	// own; a storage driver's fields go through driver.Item.Secret instead,
	// derived from Type=="password" for a plugin driver or the
	// `confidential:"true"` Go struct tag for a built-in one — see
	// internal/extension/info.go). x-secret follows the same x- extension-key
	// style as x-depends-on/x-assist above; the host carries it verbatim to
	// the frontend and also reads it itself to redact GetPluginConfig's
	// response and to preserve a stored value when SetPluginConfig receives a
	// blank resubmission.
	Secret bool `json:"x-secret,omitempty"`
}

// DependsOn drives conditional show/hide/required of a field on another field's
// value (SDK §4.1).
type DependsOn struct {
	Field string `json:"field"`
	Value any    `json:"value,omitempty"`
}

// AssistRef points a field at an AssistForm for in-place completion (SDK §4.1).
type AssistRef struct {
	Form       string            `json:"form"`
	Permission string            `json:"permission,omitempty"`
	Label      map[string]string `json:"label,omitempty"`
}
