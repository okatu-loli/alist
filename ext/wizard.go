package ext

// Wizard is an extension's declarative install flow (SDK doc §4). The host runs
// it as a step state machine (internal/extension/wizard.go); the frontend
// renders each step natively.
type Wizard struct {
	Steps []WizardStep `json:"steps,omitempty"`
}

// WizardStep kinds. The host's engine understands each kind; oauth is a seam
// (embedded auth broker, Step 2b) and risk is reserved for Step 11's risk
// interstitial — it renders whatever the manifest declares but carries no
// enforcement yet.
const (
	WizardInfo              = "info"
	WizardPermissionConsent = "permission-consent"
	WizardForm              = "form"
	WizardOAuth             = "oauth"
	WizardChoice            = "choice"
	WizardTest              = "test"
	WizardRisk              = "risk"
	WizardDone              = "done"
)

// WizardStep is one node of the install flow.
type WizardStep struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Title   map[string]string `json:"title,omitempty"`
	Body    map[string]string `json:"body,omitempty"`     // info / risk / consent copy
	Schema  *JSONSchema       `json:"schema,omitempty"`   // form: field definitions
	Options []WizardOption    `json:"options,omitempty"`  // choice: branches
	ProbeID string            `json:"probe_id,omitempty"` // test: probe selector
	// Provider is the AuthProvider extension id an oauth step drives (WS-8).
	// Required when Type == WizardOAuth; the host resolves it to a live
	// ext.AuthProvider and calls AuthURL/Exchange against it — locally, never
	// through an external broker.
	Provider string `json:"provider,omitempty"`
	// Scopes are the OAuth scopes requested from Provider. oauth only.
	Scopes []string `json:"scopes,omitempty"`
}

// WizardOption is one branch of a choice step. NextStep, when set, names the
// step id to jump to after the option is chosen (else the flow advances to the
// next step in order).
type WizardOption struct {
	ID       string            `json:"id"`
	Label    map[string]string `json:"label"`
	NextStep string            `json:"next_step,omitempty"`
}
