package extension

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/crypto"
)

// G4 §3/§5/§6: plugin config_form field encryption-at-rest — round trip,
// lazy migration of a pre-existing plaintext config, and fail-loud behavior
// when the secrets key is missing or has changed. Mirrors
// internal/op/storage_crypto_test.go's coverage for storage Addition one
// config shape over. This file is `package extension` (not `extension_test`)
// specifically to reach encryptSecretConfigFields/decryptSecretConfigFields
// directly, the same way storage_crypto_test.go reaches
// encrypt/decryptStorageAddition — secretFields is passed straight in, so
// these tests don't need a real installed plugin/manifest to declare
// x-secret fields.

func newBareTestManager(t *testing.T) *Manager {
	t.Helper()
	pub, _ := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	mgr, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func TestEncryptDecryptSecretConfigFieldsRoundTrip(t *testing.T) {
	mgr := newBareTestManager(t)
	secretFields := map[string]bool{"password": true}
	plain := json.RawMessage(`{"remote_path":"/x","password":"round-trip-secret"}`)

	sealed, err := mgr.encryptSecretConfigFields(plain, secretFields)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if strings.Contains(string(sealed), "round-trip-secret") {
		t.Fatalf("sealed config must not contain the plaintext secret: %s", sealed)
	}
	var m map[string]any
	if err := json.Unmarshal(sealed, &m); err != nil {
		t.Fatal(err)
	}
	pw, _ := m["password"].(string)
	if !strings.HasPrefix(pw, "enc:v1:") {
		t.Fatalf("password must carry the enc:v1: prefix, got %q", pw)
	}

	opened, err := mgr.decryptSecretConfigFields(sealed, secretFields)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(opened, &m2); err != nil {
		t.Fatal(err)
	}
	if m2["password"] != "round-trip-secret" {
		t.Fatalf("round trip mismatch: got %v", m2["password"])
	}
	if m2["remote_path"] != "/x" {
		t.Fatalf("non-secret field must survive the round trip, got %v", m2["remote_path"])
	}
}

// TestEncryptSecretConfigFieldsIdempotentOnAlreadyEncryptedValue mirrors
// op's TestEncryptStorageAdditionIdempotentOnAlreadyEncryptedValue: a
// re-encrypt of an already-sealed field must be a no-op, never
// double-wrapping it.
func TestEncryptSecretConfigFieldsIdempotentOnAlreadyEncryptedValue(t *testing.T) {
	mgr := newBareTestManager(t)
	secretFields := map[string]bool{"password": true}
	plain := json.RawMessage(`{"password":"idempotent-secret"}`)

	once, err := mgr.encryptSecretConfigFields(plain, secretFields)
	if err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	twice, err := mgr.encryptSecretConfigFields(once, secretFields)
	if err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	if string(once) != string(twice) {
		t.Fatalf("re-encrypting an already-encrypted value must be a no-op:\nfirst:  %s\nsecond: %s", once, twice)
	}
}

// TestDecryptSecretConfigFieldsPassesThroughPlaintextConfig pins lazy
// migration's read side: a pre-G4 plaintext config (no enc:v1: prefix at
// all) reads fine as-is.
func TestDecryptSecretConfigFieldsPassesThroughPlaintextConfig(t *testing.T) {
	mgr := newBareTestManager(t)
	plain := json.RawMessage(`{"password":"legacy-plaintext-secret"}`)
	out, err := mgr.decryptSecretConfigFields(plain, map[string]bool{"password": true})
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(out) != string(plain) {
		t.Fatalf("a plaintext config must pass through unchanged, got %s", out)
	}
}

// secretConfigManifest is validManifest() plus a config_form declaring
// "password" x-secret (ext.JSONSchema.Secret) — the real path SetConfig's
// encryption goes through: it resolves secretFields from the ACTIVE
// manifest (SecretConfigFields, secret.go), not from a caller-supplied map,
// so a test exercising that resolution (rather than calling
// encrypt/decryptSecretConfigFields directly, as the other tests in this
// file do) needs a manifest that actually declares the field secret.
func secretConfigManifest() ext.Manifest {
	m := validManifest()
	m.Contributions.ConfigForm = &ext.JSONSchema{
		Type: "object",
		Properties: map[string]*ext.JSONSchema{
			"password": {Type: "string", Secret: true},
		},
	}
	return m
}

// TestLazySecretConfigMigrationPlaintextBecomesEncryptedOnSave pins the same
// "lazy migration" contract op's storage test pins: a plugin config saved
// while carrying a secret always ends up encrypted on disk, even if it was
// never explicitly re-saved for that purpose — because SetConfig always
// encrypts on write. Simulates "predates G4" by writing a plaintext
// PluginState.Config directly into state.json (bypassing SetConfig), then
// confirms the very next SetConfig call (any save, not necessarily one that
// touches the secret) leaves it sealed.
func TestLazySecretConfigMigrationPlaintextBecomesEncryptedOnSave(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	mgr, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := secretConfigManifest()
	pkg := buildSignedPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	id := m.ID

	const legacySecret = "legacy-plaintext-secret-for-migration-test"
	secretFields := map[string]bool{"password": true}
	if err := mgr.SetConfig(id, json.RawMessage(`{"password":"`+legacySecret+`"}`)); err != nil {
		t.Fatalf("seed via SetConfig: %v", err)
	}

	// SetConfig above already encrypts (that's the feature under test one
	// layer up) — to genuinely simulate a pre-G4 plaintext row, bypass it and
	// write the plaintext PluginState.Config directly, then confirm the next
	// SetConfig call (a routine save, unrelated field) re-encrypts it.
	mgr.mu.Lock()
	mgr.state.Plugins[id].Config = json.RawMessage(`{"password":"` + legacySecret + `"}`)
	mgr.mu.Unlock()
	if err := mgr.persistState(); err != nil {
		t.Fatalf("persist plaintext state: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(mgr.Dir(), StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), legacySecret) {
		t.Fatalf("test setup bug: state.json does not contain the seeded plaintext secret: %s", raw)
	}

	// Any save (SetConfig always encrypts on write, regardless of whether
	// the caller's new value touches the secret) upgrades the plaintext row.
	if err := mgr.SetConfig(id, json.RawMessage(`{"password":"`+legacySecret+`"}`)); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	raw2, err := os.ReadFile(filepath.Join(mgr.Dir(), StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw2), legacySecret) {
		t.Fatalf("REGRESSION: plaintext secret must be encrypted after a save, got %s", raw2)
	}
	if !strings.Contains(string(raw2), "enc:v1:") {
		t.Fatalf("expected an enc:v1:-prefixed value on disk after re-save, got %s", raw2)
	}

	got, err := mgr.decryptSecretConfigFields(mgr.state.Plugins[id].Config, secretFields)
	if err != nil {
		t.Fatalf("decrypt after migration: %v", err)
	}
	var gm map[string]any
	if err := json.Unmarshal(got, &gm); err != nil {
		t.Fatal(err)
	}
	if gm["password"] != legacySecret {
		t.Fatalf("decrypted value after migration = %v, want %q", gm["password"], legacySecret)
	}
}

// TestDecryptSecretConfigFieldsFailsLoudOnWrongKey pins G4 §5 (fail-loud):
// if the secrets key changes after a config secret was sealed under the old
// one, decrypting it must return a clear error.
func TestDecryptSecretConfigFieldsFailsLoudOnWrongKey(t *testing.T) {
	mgr := newBareTestManager(t)
	secretFields := map[string]bool{"password": true}
	sealed, err := mgr.encryptSecretConfigFields(json.RawMessage(`{"password":"key-mismatch-secret"}`), secretFields)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	keyPath := filepath.Join(mgr.Dir(), crypto.SecretsKeyFile)
	wrongKey := make([]byte, crypto.KeySize)
	if _, err := rand.Read(wrongKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, wrongKey, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = mgr.decryptSecretConfigFields(sealed, secretFields)
	if err == nil {
		t.Fatal("decrypt under the wrong key must fail loud, got nil error")
	}
	if !strings.Contains(err.Error(), "secrets key") {
		t.Fatalf("error should mention the secrets key, got: %v", err)
	}
}

// TestDecryptSecretConfigFieldsFailsLoudOnMissingKeyFile mirrors
// TestDecryptStorageAdditionFailsLoudOnMissingKeyFile: a deleted key file
// (a fresh one gets minted on next resolution) must not silently succeed
// with garbage — it must fail loud, same failure mode as the wrong-key case.
func TestDecryptSecretConfigFieldsFailsLoudOnMissingKeyFile(t *testing.T) {
	mgr := newBareTestManager(t)
	secretFields := map[string]bool{"password": true}
	sealed, err := mgr.encryptSecretConfigFields(json.RawMessage(`{"password":"missing-key-secret"}`), secretFields)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	keyPath := filepath.Join(mgr.Dir(), crypto.SecretsKeyFile)
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key file: %v", err)
	}

	_, err = mgr.decryptSecretConfigFields(sealed, secretFields)
	if err == nil {
		t.Fatal("decrypt after the key file disappeared (and a fresh one gets minted) must fail loud, got nil error")
	}
}

// TestGetConfigWithWrongKeyDoesNotTouchPersistedState is I5's process-level
// counterpart to op's TestLoadStorageWithWrongKeyDoesNotDestroyCiphertext
// (internal/op/storage_crypto_test.go): every other test in this file calls
// encryptSecretConfigFields/decryptSecretConfigFields DIRECTLY, which is
// exactly the gap the [CRITICAL C1] storage bug slipped through — a pure
// function test proves the crypto primitive itself is fail-loud, but says
// nothing about whether some caller further up the REAL load/save chain
// still (perhaps unconditionally) persists a mutated value on top of a
// failed read.
//
// Unlike initStorage (which unconditionally resaves on every load — the
// mechanism that let C1 happen), GetConfig never writes state.json at all:
// it only reads ps.Config and returns decryptSecretConfigFields's result
// (see lifecycle.go). This test pins that property through the real
// Manager API rather than just reading the source: install a plugin with an
// x-secret field, SetConfig a real secret (round-tripping through the real
// encrypt-on-write path), swap in a wrong key, call the real mgr.GetConfig,
// and confirm state.json on disk is byte-for-byte unchanged and the
// original secret is still recoverable once the right key is back.
func TestGetConfigWithWrongKeyDoesNotTouchPersistedState(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	mgr, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := secretConfigManifest()
	pkg := buildSignedPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	id := m.ID

	const originalSecret = "must-survive-a-wrong-key-getconfig"
	if err := mgr.SetConfig(id, json.RawMessage(`{"password":"`+originalSecret+`"}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	stateFilePath := filepath.Join(mgr.Dir(), StateFile)
	before, err := os.ReadFile(stateFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "enc:v1:") {
		t.Fatalf("sanity: state.json must hold an enc:v1:-prefixed value before the key swap, got %s", before)
	}

	keyPath := filepath.Join(mgr.Dir(), crypto.SecretsKeyFile)
	original, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read original secrets key: %v", err)
	}
	wrongKey := make([]byte, crypto.KeySize)
	if _, err := rand.Read(wrongKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, wrongKey, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.GetConfig(id); err == nil {
		t.Fatal("GetConfig under the wrong key must fail loud, got nil error")
	}

	after, err := os.ReadFile(stateFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("REGRESSION: state.json must survive a GetConfig decrypt failure byte-for-byte unchanged,\n before: %s\n after:  %s", before, after)
	}

	// Sanity: restoring the correct key still recovers the original value —
	// nothing about the failed read/attempt corrupted anything.
	if err := os.WriteFile(keyPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := mgr.GetConfig(id)
	if err != nil {
		t.Fatalf("GetConfig after restoring the correct key: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(cfg, &got); err != nil {
		t.Fatal(err)
	}
	if got["password"] != originalSecret {
		t.Fatalf("recovered config password = %v, want %q", got["password"], originalSecret)
	}
}

// TestSetConfigRejectsWhenManifestUnresolvable pins Minor 3: SetConfig must
// REJECT the save when its active manifest cannot be resolved at all
// (corrupt/missing manifest.json on disk for an otherwise-installed plugin)
// — not silently treat that as "no secret fields declared" and write the
// caller's value to disk in plaintext. secretConfigFieldsOrErr (secret.go)
// exists specifically to make this distinguishable from the legitimate
// "installed, manifest loads fine, just declares no secrets" case.
func TestSetConfigRejectsWhenManifestUnresolvable(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	mgr, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := secretConfigManifest()
	pkg := buildSignedPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	id := m.ID

	// Corrupt the on-disk manifest for the active version — LoadManifest
	// (via ActiveManifest) must now fail.
	manifestPath := filepath.Join(mgr.versionDir(id, m.Version), ManifestFile)
	if err := os.WriteFile(manifestPath, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	const wouldBeSecret = "must-not-reach-disk-in-plaintext"
	if err := mgr.SetConfig(id, json.RawMessage(`{"password":"`+wouldBeSecret+`"}`)); err == nil {
		t.Fatal("SetConfig must reject when the manifest cannot be resolved, got nil error")
	}

	raw, err := os.ReadFile(filepath.Join(mgr.Dir(), StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), wouldBeSecret) {
		t.Fatalf("REGRESSION (Minor 3): rejected SetConfig must not have written the value to disk at all, got %s", raw)
	}
}

// TestGetConfigRejectsWhenManifestUnresolvable is TestSetConfigRejects...'s
// GetConfig-side counterpart: an unresolvable manifest must not make
// GetConfig silently skip decryption and hand back raw ciphertext as if it
// were the real config value.
func TestGetConfigRejectsWhenManifestUnresolvable(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	mgr, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := secretConfigManifest()
	pkg := buildSignedPackage(t, m, priv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	id := m.ID
	if err := mgr.SetConfig(id, json.RawMessage(`{"password":"before-corruption"}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	manifestPath := filepath.Join(mgr.versionDir(id, m.Version), ManifestFile)
	if err := os.WriteFile(manifestPath, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.GetConfig(id); err == nil {
		t.Fatal("GetConfig must reject when the manifest cannot be resolved, got nil error")
	}
}
