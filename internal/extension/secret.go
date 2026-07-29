package extension

import (
	"encoding/json"
	"fmt"

	"github.com/alist-org/alist/v3/internal/crypto"
)

// SecretsKey returns the process-wide AES-256 key used to encrypt every
// secret-at-rest this host manages (G4 §1): storage Addition fields
// (internal/op), plugin config_form fields (SetConfig/GetConfig below), and
// — via hostcap.LoadOrCreateSecretsKey, called independently with the same
// dir at boot (internal/bootstrap/plugin_host_bridge.go) — a plugin's own
// Store().Secrets() KV. All three converge on the IDENTICAL key file
// (m.Dir()/crypto.SecretsKeyFile) because crypto.LoadOrCreateKey is
// idempotent per (dir, filename): whichever caller asks first creates it;
// every later caller, in this process or a restarted one, gets the same
// bytes back. One key file, one backup/restore story (G4 §4) covers
// everything it protects.
func (m *Manager) SecretsKey() ([]byte, error) {
	return crypto.LoadOrCreateKey(m.Dir(), crypto.SecretsKeyFile)
}

// SecretConfigFields returns the top-level config_form property names id's
// active manifest marks x-secret (ext.JSONSchema.Secret — G3 §1), or nil if
// the plugin, its manifest, or its config_form can't be resolved, or it
// simply declares no secret fields. Callers treat nil the same as "nothing
// is secret" (no redaction, no merge-on-save).
//
// This is a thin, error-dropping wrapper over secretConfigFieldsOrErr for
// callers where failing open (treating an unresolvable manifest as "no
// secrets declared") is the right tradeoff — e.g. GetPluginConfig's
// best-effort redaction DTO, which never writes anything and would rather
// show an admin a possibly-under-redacted value than hard-fail listing
// plugins. SetConfig/GetConfig (lifecycle.go) do NOT use this: see
// secretConfigFieldsOrErr's doc comment for why they need the distinction
// this drops.
func (m *Manager) SecretConfigFields(id string) map[string]bool {
	fields, _ := m.secretConfigFieldsOrErr(id)
	return fields
}

// secretConfigFieldsOrErr is SecretConfigFields, but distinguishes "the
// manifest legitimately declares no secret fields" (nil, nil) from "the
// manifest could not be resolved at all" (nil, err) — e.g. a plugin that IS
// installed but whose active version's manifest.json is corrupt or missing
// on disk.
//
// [Minor 3, G4 red team] SetConfig/GetConfig need this distinction:
// collapsing both cases into a bare nil (as SecretConfigFields above does)
// gives a caller no way to tell "genuinely nothing to encrypt" apart from
// "couldn't check" — treating the latter as the former would silently
// downgrade a plugin's config to plaintext-on-disk (SetConfig, the moment
// its manifest transiently fails to resolve) or hand back still-encrypted
// ciphertext masquerading as the real config value (GetConfig) — exactly
// when the caller has the least ability to tell either happened. Both now
// reject the call instead when resolution itself fails.
func (m *Manager) secretConfigFieldsOrErr(id string) (map[string]bool, error) {
	man, err := m.ActiveManifest(id)
	if err != nil {
		return nil, err
	}
	if man.Contributions.ConfigForm == nil {
		return nil, nil
	}
	var names map[string]bool
	for name, prop := range man.Contributions.ConfigForm.Properties {
		if prop != nil && prop.Secret {
			if names == nil {
				names = map[string]bool{}
			}
			names[name] = true
		}
	}
	return names, nil
}

// RejectReservedPrefixConfig rejects cfg (a JSON object) if any field named
// in secretFields holds a plaintext string value starting with the reserved
// crypto.EncPrefixV1 prefix — see crypto.RejectReservedPrefix's doc comment
// for why (Minor 10, G4 red team: a fresh user-submitted value colliding
// with the prefix would otherwise slip past encryption entirely, stored in
// cleartext indistinguishable from real ciphertext). Callers must run this
// on the caller-submitted cfg BEFORE MergeSecretConfigFields folds an old,
// legitimately-already-encrypted value in for a blank field — merging first
// would make that legitimate case indistinguishable from what this rejects.
func RejectReservedPrefixConfig(cfg json.RawMessage, secretFields map[string]bool) error {
	return crypto.RejectReservedPrefix(cfg, secretFields)
}

// RedactSecretConfigFields returns cfg (a JSON object) with every field
// named in secretFields blanked out and marked "<field>_set": true whenever
// it held a non-empty string — GetPluginConfig's read-back sentinel,
// mirroring op.RedactStorageAddition one config shape over (see its doc
// comment for the "_set absent means never configured" contract this also
// follows). Best-effort: returns cfg completely unchanged if it isn't a
// JSON object or secretFields is empty.
func RedactSecretConfigFields(cfg json.RawMessage, secretFields map[string]bool) json.RawMessage {
	if len(secretFields) == 0 || len(cfg) == 0 {
		return cfg
	}
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil {
		return cfg
	}
	changed := false
	for name := range secretFields {
		v, ok := m[name]
		if !ok || v == nil {
			continue
		}
		// Review round 1, item 3 (see op.RedactStorageAddition's identical
		// fix, internal/op/storage_secret.go): a bare `s, _ := v.(string)`
		// conflates "type assertion failed" (a type-confused secret
		// submitted as an object/number/array/bool) with "genuinely empty
		// string," silently passing the former through in plaintext on
		// every read. Only an actual empty string means "never
		// configured" — anything else non-nil is redacted.
		if s, isString := v.(string); isString && s == "" {
			continue
		}
		m[name] = ""
		m[name+"_set"] = true
		changed = true
	}
	if !changed {
		return cfg
	}
	b, err := json.Marshal(m)
	if err != nil {
		return cfg
	}
	return b
}

// MergeSecretConfigFields fills a blank secret field in newCfg from oldCfg's
// stored value, so SetPluginConfig saving a form that never received the
// redacted secret back cannot silently wipe it — mirrors
// op.MergeStorageAdditionSecrets one config shape over. Best-effort: returns
// newCfg unchanged if it isn't a JSON object or secretFields is empty;
// oldCfg failing to parse (or being empty — nothing saved yet) just means no
// fallback value exists, not an error.
func MergeSecretConfigFields(oldCfg, newCfg json.RawMessage, secretFields map[string]bool) json.RawMessage {
	if len(secretFields) == 0 || len(newCfg) == 0 {
		return newCfg
	}
	var newM map[string]any
	if err := json.Unmarshal(newCfg, &newM); err != nil {
		return newCfg
	}
	var oldM map[string]any
	if len(oldCfg) > 0 {
		_ = json.Unmarshal(oldCfg, &oldM)
	}
	changed := false
	for name := range secretFields {
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
		return newCfg
	}
	b, err := json.Marshal(newM)
	if err != nil {
		return newCfg
	}
	return b
}

// encryptSecretConfigFields seals every field named in secretFields within
// cfg via crypto.EncryptJSONFields (G4 §3), using m.SecretsKey. Idempotent
// — an already-sealed field is left alone — and a no-op when secretFields
// is empty. Called from SetConfig with the lock NOT held (see its doc
// comment) so crypto.EncryptJSONFields's lazy key resolution never nests
// inside m.mu.
func (m *Manager) encryptSecretConfigFields(cfg json.RawMessage, secretFields map[string]bool) (json.RawMessage, error) {
	if len(secretFields) == 0 {
		return cfg, nil
	}
	out, err := crypto.EncryptJSONFields(m.SecretsKey, cfg, secretFields)
	if err != nil {
		return nil, fmt.Errorf("extension: encrypt plugin config secrets: %w", err)
	}
	return out, nil
}

// decryptSecretConfigFields reverses encryptSecretConfigFields, called from
// GetConfig. A value with no enc:v1: prefix passes through unchanged (lazy
// migration: a pre-G4 plaintext config, or a non-secret field, reads fine
// as-is). FAILS LOUD (G4 §5) if a prefixed value cannot be decrypted —
// GetConfig surfaces this as a real error, never a silently blank/garbage
// config.
func (m *Manager) decryptSecretConfigFields(cfg json.RawMessage, secretFields map[string]bool) (json.RawMessage, error) {
	if len(secretFields) == 0 {
		return cfg, nil
	}
	out, err := crypto.DecryptJSONFields(m.SecretsKey, cfg, secretFields)
	if err != nil {
		return nil, fmt.Errorf("extension: decrypt plugin config secrets failed (the secrets key may be missing or has changed — re-enter this plugin's configuration): %w", err)
	}
	return out, nil
}
