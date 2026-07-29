package op

import (
	"encoding/json"
	"fmt"

	"github.com/alist-org/alist/v3/internal/crypto"
	"github.com/alist-org/alist/v3/internal/extension"
)

// storageSecretsKey resolves the shared secrets key (G4 §1/§4: the SAME key
// file hostcap's plugin Store().Secrets() and plugin config_form encryption
// use — extension.PluginsDir()/crypto.SecretsKeyFile). Deliberately calls
// extension.PluginsDir() (a pure path computation) rather than
// extension.DefaultManager(): storage encryption has no need for the full
// plugin Manager singleton (trust verification, policy wiring, background
// service supervision, ...) just to resolve a key file, and going through
// it would make storage encryption's very first use anywhere in a process
// accidentally decide (via DefaultManager's sync.Once) what directory the
// REAL plugin Manager singleton initializes against for the rest of that
// process's life — surprising in production and a real test-isolation
// hazard (a storage-only test that never touches plugins could otherwise
// leak a "plugins" scratch directory into whatever the process's current
// working directory happens to be).
//
// Every call site below treats a failure here as fatal to the operation
// it's part of (fail-loud, G4 §5) — it never silently falls back to
// storing or using plaintext because the key was momentarily unavailable.
func storageSecretsKey() ([]byte, error) {
	key, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		return nil, fmt.Errorf("op: resolve secrets key: %w", err)
	}
	return key, nil
}

// rejectReservedPrefixAddition rejects a FRESH, caller-submitted addition
// (never one already merged with an old stored value — see
// crypto.RejectReservedPrefix's doc comment for why the distinction
// matters) whose Secret-marked field literally starts with the
// crypto.EncPrefixV1 reserved prefix (Minor 10, G4 red team): without this,
// such a value would sail through encryptStorageAddition's "already
// encrypted, skip" check untouched, landing in the database in cleartext
// forever, indistinguishable from real ciphertext to any later reader.
func rejectReservedPrefixAddition(driverName, addition string) error {
	secrets := secretItemNames(driverName)
	if len(secrets) == 0 {
		return nil
	}
	if err := crypto.RejectReservedPrefix(json.RawMessage(addition), secrets); err != nil {
		return fmt.Errorf("op: %w", err)
	}
	return nil
}

// encryptStorageAddition seals every Secret-marked Additional field's value
// in addition (driverName's driver.Info — the same lookup
// RedactStorageAddition/MergeStorageAdditionSecrets use, storage_secret.go)
// via crypto.EncryptJSONFields (G4 §1: the shared field-level sealing logic
// storage and plugin config_form encryption both use). Idempotent — an
// already-sealed field (enc:v1: prefix) is left alone — and a no-op for a
// driver with no secret fields.
func encryptStorageAddition(driverName, addition string) (string, error) {
	secrets := secretItemNames(driverName)
	if len(secrets) == 0 {
		return addition, nil
	}
	out, err := crypto.EncryptJSONFields(storageSecretsKey, json.RawMessage(addition), secrets)
	if err != nil {
		return "", fmt.Errorf("op: encrypt storage secrets: %w", err)
	}
	return string(out), nil
}

// decryptStorageAddition reverses encryptStorageAddition, called before the
// caller (initStorage) unmarshals Addition into the driver's typed struct.
// A value with no enc:v1: prefix passes through unchanged (lazy migration:
// a pre-G4 plaintext row, or a non-secret field, reads fine as-is).
//
// FAILS LOUD (G4 §5): a value that DOES carry the prefix but cannot be
// decrypted (wrong/missing/rotated key, corrupt data) returns a real error
// instead of silently swallowing it — the caller surfaces this as the
// storage's init failure/status, prompting re-entering credentials, never a
// 500 with no explanation or a driver silently handed garbage.
func decryptStorageAddition(driverName, addition string) (string, error) {
	secrets := secretItemNames(driverName)
	if len(secrets) == 0 {
		return addition, nil
	}
	out, err := crypto.DecryptJSONFields(storageSecretsKey, json.RawMessage(addition), secrets)
	if err != nil {
		return "", fmt.Errorf("op: decrypt storage secrets failed (the secrets key may be missing or has changed — re-enter this storage's credentials): %w", err)
	}
	return string(out), nil
}
