package op

import (
	"encoding/json"

	"github.com/alist-org/alist/v3/internal/model"
)

// secretItemNames returns the Additional item Names driverName's driver.Info
// marks Secret (a built-in driver's confidential:"true" struct tag, or a
// plugin driver's Type=="password" convention — see
// internal/extension/info.go's getAdditionalItems/mapExtItems), or nil if
// the driver is unknown or declares no secret fields.
func secretItemNames(driverName string) map[string]bool {
	info, ok := GetDriverInfoMap()[driverName]
	if !ok {
		return nil
	}
	var names map[string]bool
	for _, item := range info.Additional {
		if item.Secret {
			if names == nil {
				names = map[string]bool{}
			}
			names[item.Name] = true
		}
	}
	return names
}

// RedactStorageAddition returns a copy of storage with every secret
// Additional field (per secretItemNames) blanked out of its Addition JSON
// and marked "<field>_set": true in its place, whenever that field held a
// non-empty value — mirrors server/handles/sso_admin.go's write-only
// ClientSecretSet sentinel one layer down, for a free-form JSON blob instead
// of a typed Go field. A field with nothing stored is left exactly as-is
// (no "_set" key added at all — its absence IS the "never configured"
// signal; see the phG3 report for the frontend contract this establishes).
//
// Best-effort by design (G3 §1 is declare + read-back sentinel only, not a
// hardening pass on Addition's own storage format): storage is returned
// completely unchanged if its driver is unknown, declares no secret fields,
// or Addition is not a JSON object (a malformed/legacy row is not this
// function's problem to fix, and must not be allowed to make a read fail).
func RedactStorageAddition(storage model.Storage) model.Storage {
	secrets := secretItemNames(storage.Driver)
	if len(secrets) == 0 {
		return storage
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(storage.Addition), &m); err != nil {
		return storage
	}
	changed := false
	for name := range secrets {
		v, ok := m[name]
		if !ok || v == nil {
			continue
		}
		// Review round 1, item 3: `s, _ := v.(string)` alone conflates "the
		// type assertion failed" (v is an object/number/array/bool — a
		// type-confused secret, e.g. submitted as JSON instead of a plain
		// string) with "v is a genuinely empty string" — both produced
		// s=="" and were treated as "nothing stored," so a non-string
		// secret value passed through in full on every subsequent read.
		// Only an ACTUAL empty string means "never configured"; every
		// other non-nil value (including a non-empty string, or any
		// non-string type at all) must be redacted.
		if s, isString := v.(string); isString && s == "" {
			continue
		}
		m[name] = ""
		m[name+"_set"] = true
		changed = true
	}
	if !changed {
		return storage
	}
	b, err := json.Marshal(m)
	if err != nil {
		return storage
	}
	storage.Addition = string(b)
	return storage
}

// MergeStorageAdditionSecrets fills a blank secret field in newAddition from
// oldAddition's stored value, so an update whose submitter only ever saw the
// redacted blank (RedactStorageAddition) — the common "edit some OTHER
// field, resubmit the form" case — cannot silently wipe a previously-stored
// secret. Mirrors UpdateSSOProvider's "blank client_secret keeps the stored
// one" rule (server/handles/sso_admin.go:169-173) for the free-form
// Addition blob. A secret field the caller DID resubmit a non-blank value
// for is left alone (the caller is deliberately changing/clearing... no,
// deliberately SETTING it — there is no way to distinguish "leave blank to
// keep" from "explicitly want it blank" in a plain string field, so, like
// UpdateSSOProvider, blank always means "keep", never "clear"; clearing a
// secret requires setting a new, different value or the driver's own
// mechanism for it, if any).
//
// Best-effort: returns newAddition unchanged if the driver is unknown,
// declares no secret fields, or either Addition string is not a JSON
// object.
func MergeStorageAdditionSecrets(driverName, oldAddition, newAddition string) string {
	secrets := secretItemNames(driverName)
	if len(secrets) == 0 {
		return newAddition
	}
	var newM map[string]any
	if err := json.Unmarshal([]byte(newAddition), &newM); err != nil {
		return newAddition
	}
	var oldM map[string]any
	_ = json.Unmarshal([]byte(oldAddition), &oldM) // best-effort; nil map on failure just yields no fallback values
	changed := false
	for name := range secrets {
		nv, _ := newM[name].(string)
		if nv != "" {
			continue
		}
		if ov, ok := oldM[name]; ok {
			newM[name] = ov
			changed = true
		}
	}
	if !changed {
		return newAddition
	}
	b, err := json.Marshal(newM)
	if err != nil {
		return newAddition
	}
	return string(b)
}
