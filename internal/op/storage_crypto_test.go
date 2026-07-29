package op

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/crypto"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
)

// This package's tests never configured flags.DataDir before G4 (nothing in
// internal/op touched it): it defaults to "", so extension.PluginsDir()
// (storageSecretsKey's dir — see storage_crypto.go) would resolve to a bare
// relative "plugins", landing wherever this test binary's process happens
// to start (this package's own source directory under `go test`), leaking
// a scratch "plugins/.secrets.key" into the repo. Redirect it to a fresh
// temp dir once, in this file's own init() — guaranteed to run before any
// test in the package, regardless of file/declaration order — mirroring
// server/handles/plugins_test.go's setupPluginGuardEnv one package over.
func init() {
	dir, err := os.MkdirTemp("", "alist-op-secrets-test-*")
	if err != nil {
		panic("storage_crypto_test.go: MkdirTemp: " + err.Error())
	}
	flags.DataDir = dir
}

// cryptoTestAddition/cryptoTestDriver is a minimal driver.Driver with one
// Secret-marked field (the confidential:"true" tag), registered under a
// name distinct from the real "Crypt" driver so
// TestLazyMigrationPlaintextRowBecomesEncryptedOnLoad's LoadStorage call
// doesn't need a real backing remote storage to Init() against (unlike
// Crypt, which wraps another mounted storage) — it only needs to prove
// initStorage's decrypt-then-always-resave cycle upgrades a plaintext row,
// independent of whether the specific driver's own Init logic succeeds.
type cryptoTestAddition struct {
	RemotePath string `json:"remote_path"`
	Password   string `json:"password" confidential:"true"`
}

type cryptoTestDriver struct {
	model.Storage
	Addition cryptoTestAddition
}

func (d *cryptoTestDriver) Config() driver.Config {
	return driver.Config{Name: "CryptoRoundTripTestDriver", NoCache: true}
}
func (d *cryptoTestDriver) GetAddition() driver.Additional { return &d.Addition }
func (d *cryptoTestDriver) Init(ctx context.Context) error { return nil }
func (d *cryptoTestDriver) Drop(ctx context.Context) error { return nil }
func (d *cryptoTestDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}
func (d *cryptoTestDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return nil, nil
}

func init() {
	RegisterDriver(func() driver.Driver { return &cryptoTestDriver{} })
}

// G4 §2/§5/§6: storage Addition encryption-at-rest — round trip, idempotent
// re-encryption, lazy migration of a pre-existing plaintext row, and
// fail-loud behavior when the secrets key is missing or has changed. These
// exercise encryptStorageAddition/decryptStorageAddition directly (this
// file is `package op`, unlike storage_secret_test.go's `package op_test`,
// specifically so it can reach the unexported functions).

// TestEncryptDecryptStorageAdditionRoundTrip pins the basic contract: a
// secret field seals to an enc:v1:-prefixed value and opens back to the
// original plaintext.
func TestEncryptDecryptStorageAdditionRoundTrip(t *testing.T) {
	plain := `{"remote_path":"/x","password":"round-trip-secret","salt":"s"}`
	sealed, err := encryptStorageAddition("Crypt", plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if strings.Contains(sealed, "round-trip-secret") {
		t.Fatalf("sealed Addition must not contain the plaintext secret: %s", sealed)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(sealed), &m); err != nil {
		t.Fatal(err)
	}
	pw, _ := m["password"].(string)
	if !strings.HasPrefix(pw, "enc:v1:") {
		t.Fatalf("password must carry the enc:v1: prefix, got %q", pw)
	}

	opened, err := decryptStorageAddition("Crypt", sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var m2 map[string]any
	if err := json.Unmarshal([]byte(opened), &m2); err != nil {
		t.Fatal(err)
	}
	if m2["password"] != "round-trip-secret" {
		t.Fatalf("round trip mismatch: got %v", m2["password"])
	}
	if m2["remote_path"] != "/x" {
		t.Fatalf("non-secret field must survive the round trip, got %v", m2["remote_path"])
	}
}

// TestEncryptStorageAdditionIdempotentOnAlreadyEncryptedValue pins the
// idempotent-prefix-check: re-encrypting an already-sealed value is a
// no-op, never double-wrapping it.
func TestEncryptStorageAdditionIdempotentOnAlreadyEncryptedValue(t *testing.T) {
	plain := `{"remote_path":"/x","password":"idempotent-secret","salt":"s"}`
	once, err := encryptStorageAddition("Crypt", plain)
	if err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	twice, err := encryptStorageAddition("Crypt", once)
	if err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	if once != twice {
		t.Fatalf("re-encrypting an already-encrypted value must be a no-op:\nfirst:  %s\nsecond: %s", once, twice)
	}
	back, err := decryptStorageAddition("Crypt", twice)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(back), &m); err != nil {
		t.Fatal(err)
	}
	if m["password"] != "idempotent-secret" {
		t.Fatalf("round trip after double-encrypt-attempt mismatch: %v", m["password"])
	}
}

// TestDecryptStorageAdditionPassesThroughPlaintextRow pins lazy migration's
// read side: a pre-G4 plaintext row (no enc:v1: prefix at all) must read
// fine as-is — it is not eagerly rewritten just by being decrypted, only
// upgraded the next time encryptStorageAddition runs on it (see
// TestLazyMigrationPlaintextRowBecomesEncryptedOnLoad below for the full
// read-then-save cycle).
func TestDecryptStorageAdditionPassesThroughPlaintextRow(t *testing.T) {
	plain := `{"remote_path":"/x","password":"legacy-plaintext-secret","salt":"s"}`
	out, err := decryptStorageAddition("Crypt", plain)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if out != plain {
		t.Fatalf("a plaintext row must pass through decryptStorageAddition unchanged, got %s", out)
	}
}

// TestLazyMigrationPlaintextRowBecomesEncryptedOnLoad pins G4 §2's explicit
// "lazy migration" requirement end to end: a plaintext row written directly
// to the DB (bypassing CreateStorage's own encrypt step — simulating a row
// that predates this feature) gets automatically upgraded to encrypted the
// very first time it is loaded, because initStorage always ends in
// MustSaveDriverStorage regardless of whether anything else changed.
func TestLazyMigrationPlaintextRowBecomesEncryptedOnLoad(t *testing.T) {
	const legacySecret = "legacy-plaintext-secret-for-migration-test"
	storage := model.Storage{
		MountPath: "/lazy-migration-test",
		Driver:    "CryptoRoundTripTestDriver",
		Addition:  `{"remote_path":"/x","password":"` + legacySecret + `"}`,
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("db.CreateStorage (plaintext, bypassing op.CreateStorage's encrypt step): %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteStorageById(storage.ID) })

	preLoad, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preLoad.Addition, legacySecret) {
		t.Fatalf("sanity: row must actually be plaintext before load, got %s", preLoad.Addition)
	}

	if err := LoadStorage(context.Background(), storage); err != nil {
		t.Fatalf("LoadStorage: %v", err)
	}
	t.Cleanup(func() {
		if d, err := GetStorageByMountPath(storage.MountPath); err == nil {
			_ = d.Drop(context.Background())
		}
	})

	postLoad, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(postLoad.Addition, legacySecret) {
		t.Fatalf("REGRESSION: plaintext row must be upgraded to encrypted after being loaded, got %s", postLoad.Addition)
	}
	if !strings.Contains(postLoad.Addition, "enc:v1:") {
		t.Fatalf("expected an enc:v1:-prefixed value after lazy migration, got %s", postLoad.Addition)
	}

	// Sanity: the driver itself is still live and usable after the
	// migration (drivers/crypt/driver_test.go separately proves a real
	// Crypt mount actually encrypts/decrypts files correctly through this
	// exact CreateStorage/LoadStorage → initStorage → decrypt path).
	if _, err := GetStorageByMountPath(storage.MountPath); err != nil {
		t.Fatalf("get live driver after migration: %v", err)
	}
}

// TestLoadStorageWithWrongKeyDoesNotDestroyCiphertext pins the [CRITICAL C1]
// red-team finding: initStorage used to call MustSaveDriverStorage
// UNCONDITIONALLY, even when decryptStorageAddition/Unmarshal had just
// failed and therefore never populated storageDriver.GetAddition() (still
// its zero value). MustSaveDriverStorage derives the persisted Addition
// column from THAT typed struct, so it would marshal the zero value,
// "encrypt" it, and overwrite the row's real enc:v1: ciphertext with a
// blank blob — permanently destroying the only copy of the credential.
// This is exactly the scenario G4 §4 exists to make recoverable (secrets
// key lost, or a backup restored without IncludeSecretsKey, or restored
// onto a different key entirely): LoadStorages calls this at boot for
// every row, so a single wrong key would have silently zeroed out every
// stored credential in the database — a recoverable operational mistake
// turned into unrecoverable data loss the instant the server started.
//
// This test seeds a REAL ciphertext row, swaps in a wrong key (simulating
// exactly that scenario), calls LoadStorage, and asserts the DB row's
// Addition is untouched byte-for-byte and its Status now reports the
// failure — proving the row survives to be retried once the right key is
// available again, instead of being silently wiped.
func TestLoadStorageWithWrongKeyDoesNotDestroyCiphertext(t *testing.T) {
	const originalSecret = "must-survive-a-wrong-key-load"
	plain := `{"remote_path":"/x","password":"` + originalSecret + `"}`
	sealed, err := encryptStorageAddition("CryptoRoundTripTestDriver", plain)
	if err != nil {
		t.Fatalf("seed encrypt: %v", err)
	}
	if !strings.Contains(sealed, "enc:v1:") {
		t.Fatalf("sanity: seeded row must actually be encrypted, got %s", sealed)
	}

	storage := model.Storage{
		MountPath: "/wrong-key-load-test",
		Driver:    "CryptoRoundTripTestDriver",
		Addition:  sealed,
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("db.CreateStorage: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteStorageById(storage.ID) })

	// Swap in a wrong key, simulating secrets key loss / a mismatched
	// restore — the exact trigger scenario.
	keyPath := filepath.Join(extension.PluginsDir(), crypto.SecretsKeyFile)
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
	t.Cleanup(func() {
		if err := os.WriteFile(keyPath, original, 0o600); err != nil {
			t.Fatalf("restore original secrets key: %v", err)
		}
	})

	loadErr := LoadStorage(context.Background(), storage)
	if loadErr == nil {
		t.Fatal("LoadStorage under the wrong key must fail loud, got nil error")
	}
	t.Cleanup(func() {
		if d, err := GetStorageByMountPath(storage.MountPath); err == nil {
			_ = d.Drop(context.Background())
		}
	})

	postLoad, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if postLoad.Addition != sealed {
		t.Fatalf("REGRESSION (C1): DB row's Addition must survive a decrypt failure byte-for-byte unchanged,\n got:  %s\n want: %s", postLoad.Addition, sealed)
	}
	if postLoad.Status == "" || postLoad.Status == WORK {
		t.Fatalf("expected Status to record the decrypt failure, got %q", postLoad.Status)
	}

	// Sanity: the original ciphertext, once the right key is back, still
	// decrypts to the real value — proving nothing was lost, only the load
	// attempt failed.
	os.WriteFile(keyPath, original, 0o600)
	recovered, err := decryptStorageAddition("CryptoRoundTripTestDriver", postLoad.Addition)
	if err != nil {
		t.Fatalf("decrypt after restoring the correct key: %v", err)
	}
	if !strings.Contains(recovered, originalSecret) {
		t.Fatalf("recovered Addition does not contain the original secret: %s", recovered)
	}
}

// TestDecryptStorageAdditionFailsLoudOnWrongKey pins G4 §5 (fail-loud): if
// the secrets key changes (rotation, disk loss + regeneration, a wrong key
// restored from an unrelated backup) after a secret was sealed under the
// old one, decrypting it must return a clear error — never a panic, never
// an empty/garbage credential silently handed to the driver.
func TestDecryptStorageAdditionFailsLoudOnWrongKey(t *testing.T) {
	sealed, err := encryptStorageAddition("Crypt", `{"remote_path":"/x","password":"key-mismatch-secret","salt":"s"}`)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
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
	t.Cleanup(func() {
		if err := os.WriteFile(keyPath, original, 0o600); err != nil {
			t.Fatalf("restore original secrets key: %v", err)
		}
	})

	_, err = decryptStorageAddition("Crypt", sealed)
	if err == nil {
		t.Fatal("decrypt under the wrong key must fail loud, got nil error")
	}
	if !strings.Contains(err.Error(), "secrets key") {
		t.Fatalf("error should mention the secrets key (so an operator/admin knows what's wrong), got: %v", err)
	}
}

// TestDecryptStorageAdditionFailsLoudOnMissingKeyFile pins the same
// fail-loud requirement for a deleted (not just wrong) key file — a wrong-
// length or absent-then-regenerated key file must not silently regenerate
// under the covers and pretend nothing happened; LoadOrCreateKey would
// simply mint a brand-new key that cannot open what was sealed under the
// old one, so this is really the same failure mode as
// TestDecryptStorageAdditionFailsLoudOnWrongKey, pinned via the other route
// (delete + let a fresh key be created) for completeness.
func TestDecryptStorageAdditionFailsLoudOnMissingKeyFile(t *testing.T) {
	sealed, err := encryptStorageAddition("Crypt", `{"remote_path":"/x","password":"missing-key-secret","salt":"s"}`)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	mgr, err := extension.DefaultManager()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(mgr.Dir(), crypto.SecretsKeyFile)
	original, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read original secrets key: %v", err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(keyPath, original, 0o600); err != nil {
			t.Fatalf("restore original secrets key: %v", err)
		}
	})
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key file: %v", err)
	}

	_, err = decryptStorageAddition("Crypt", sealed)
	if err == nil {
		t.Fatal("decrypt after the key file disappeared (and a fresh one gets minted) must fail loud, got nil error")
	}
}

// TestRejectReservedPrefixAdditionCatchesCollision pins Minor 10: a
// caller-submitted secret value that literally starts with crypto.EncPrefixV1
// ("enc:v1:") would otherwise sail through encryptStorageAddition's
// "already encrypted, skip" check unencrypted — this is the guard that must
// catch it before that ever happens.
func TestRejectReservedPrefixAdditionCatchesCollision(t *testing.T) {
	if err := rejectReservedPrefixAddition("Crypt", `{"remote_path":"/x","password":"enc:v1:not-really-ciphertext"}`); err == nil {
		t.Fatal("a fresh password value colliding with the reserved enc:v1: prefix must be rejected")
	}
	if err := rejectReservedPrefixAddition("Crypt", `{"remote_path":"/x","password":"a-normal-password"}`); err != nil {
		t.Fatalf("a normal password value must not be rejected: %v", err)
	}
}

// TestCreateStorageRejectsPasswordCollidingWithReservedPrefix is the
// process-level counterpart: CreateStorage must reject the whole call (and
// create no row at all) rather than silently persisting the collision
// unencrypted.
func TestCreateStorageRejectsPasswordCollidingWithReservedPrefix(t *testing.T) {
	storage := model.Storage{
		MountPath: "/minor10-create-test",
		Driver:    "CryptoRoundTripTestDriver",
		Addition:  `{"remote_path":"/x","password":"enc:v1:not-really-ciphertext"}`,
	}
	if _, err := CreateStorage(context.Background(), storage); err == nil {
		t.Fatal("CreateStorage must reject a password colliding with the reserved enc:v1: prefix")
	}
	if _, err := db.GetStorageByMountPath(storage.MountPath); err == nil {
		t.Fatal("REGRESSION (Minor 10): a rejected CreateStorage must not have created a row")
	}
}

// TestUpdateStorageRejectsPasswordCollidingWithReservedPrefix confirms
// UpdateStorage checks the caller-submitted value BEFORE
// MergeStorageAdditionSecrets folds the old (legitimately-already-encrypted)
// value in — checking after the merge would make the two cases
// indistinguishable.
func TestUpdateStorageRejectsPasswordCollidingWithReservedPrefix(t *testing.T) {
	const originalSecret = "genuine-secret-before-update-attempt"
	storage := model.Storage{
		MountPath: "/minor10-update-test",
		Driver:    "CryptoRoundTripTestDriver",
		Addition:  `{"remote_path":"/x","password":"` + originalSecret + `"}`,
	}
	id, err := CreateStorage(context.Background(), storage)
	if err != nil {
		t.Fatalf("CreateStorage: %v", err)
	}
	t.Cleanup(func() {
		if d, err := GetStorageByMountPath(storage.MountPath); err == nil {
			_ = d.Drop(context.Background())
		}
		_ = db.DeleteStorageById(id)
	})

	update := model.Storage{
		ID:        id,
		MountPath: storage.MountPath,
		Driver:    storage.Driver,
		Addition:  `{"remote_path":"/x","password":"enc:v1:not-really-ciphertext"}`,
	}
	if err := UpdateStorage(context.Background(), update); err == nil {
		t.Fatal("UpdateStorage must reject a password colliding with the reserved enc:v1: prefix")
	}

	postUpdate, err := db.GetStorageById(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(postUpdate.Addition, "not-really-ciphertext") {
		t.Fatalf("REGRESSION (Minor 10): rejected UpdateStorage must not have written the colliding value at all, got %s", postUpdate.Addition)
	}
	recovered, err := decryptStorageAddition(storage.Driver, postUpdate.Addition)
	if err != nil {
		t.Fatalf("decrypt after rejected update: %v", err)
	}
	if !strings.Contains(recovered, originalSecret) {
		t.Fatalf("original secret must survive a rejected update untouched, got %s", recovered)
	}
}
