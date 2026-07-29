package op_test

import (
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestCryptConfidentialTagActivated pins the "activate the dead tag" part of
// G3 §1: drivers/crypt/meta.go's Password/Salt fields have long carried a
// confidential:"true" struct tag that nothing ever read. getAdditionalItems
// (internal/extension/info.go) now sets driver.Item.Secret from it, so the
// built-in Crypt driver's Info must report both as Secret — and a plain
// field (remote_path) must not be.
func TestCryptConfidentialTagActivated(t *testing.T) {
	info, ok := op.GetDriverInfoMap()["Crypt"]
	if !ok {
		t.Fatal("Crypt driver not registered (blank import missing?)")
	}
	secret := map[string]bool{}
	for _, item := range info.Additional {
		secret[item.Name] = item.Secret
	}
	if !secret["password"] {
		t.Fatal("Crypt's password field must be Secret (confidential:\"true\" tag)")
	}
	if !secret["salt"] {
		t.Fatal("Crypt's salt field must be Secret (confidential:\"true\" tag)")
	}
	if secret["remote_path"] {
		t.Fatal("Crypt's remote_path field must NOT be Secret")
	}
}

// TestRedactStorageAdditionBlanksSecretAndMarksSet pins the read-back
// sentinel shape: a configured secret is blanked and marked "<field>_set":
// true; a non-secret field passes through unchanged.
func TestRedactStorageAdditionBlanksSecretAndMarksSet(t *testing.T) {
	storage := model.Storage{
		Driver:   "Crypt",
		Addition: `{"password":"s3cr3t-must-not-leak","salt":"s","remote_path":"/x"}`,
	}
	redacted := op.RedactStorageAddition(storage)
	if strings.Contains(redacted.Addition, "s3cr3t-must-not-leak") {
		t.Fatalf("secret value leaked into redacted Addition: %s", redacted.Addition)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(redacted.Addition), &m); err != nil {
		t.Fatal(err)
	}
	if m["password"] != "" {
		t.Fatalf("password must be blanked, got %v", m["password"])
	}
	if m["password_set"] != true {
		t.Fatalf("password_set must be true, got %v", m["password_set"])
	}
	if m["remote_path"] != "/x" {
		t.Fatalf("non-secret field must pass through unchanged, got %v", m["remote_path"])
	}
}

// TestRedactStorageAdditionLeavesNeverSetSecretFieldAbsent pins the "_set
// absent means never configured" half of the contract.
func TestRedactStorageAdditionLeavesNeverSetSecretFieldAbsent(t *testing.T) {
	storage := model.Storage{Driver: "Crypt", Addition: `{"password":"","remote_path":"/x"}`}
	redacted := op.RedactStorageAddition(storage)
	var m map[string]any
	if err := json.Unmarshal([]byte(redacted.Addition), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["password_set"]; ok {
		t.Fatalf("password_set must be absent when nothing was ever stored, got %v", m["password_set"])
	}
}

// TestRedactStorageAdditionUnknownDriverUnchanged pins the best-effort
// fallback: an unrecognized driver name leaves Addition byte-for-byte
// unchanged rather than erroring or guessing.
func TestRedactStorageAdditionUnknownDriverUnchanged(t *testing.T) {
	storage := model.Storage{Driver: "NoSuchDriver", Addition: `{"password":"x"}`}
	redacted := op.RedactStorageAddition(storage)
	if redacted.Addition != storage.Addition {
		t.Fatalf("unknown driver must leave Addition unchanged, got %s", redacted.Addition)
	}
}

// TestMergeStorageAdditionSecretsKeepsOldOnBlank pins the "blank means keep"
// merge rule mirroring UpdateSSOProvider's client_secret handling.
func TestMergeStorageAdditionSecretsKeepsOldOnBlank(t *testing.T) {
	old := `{"password":"original-secret","remote_path":"/old"}`
	incoming := `{"password":"","remote_path":"/new"}`
	merged := op.MergeStorageAdditionSecrets("Crypt", old, incoming)
	var m map[string]any
	if err := json.Unmarshal([]byte(merged), &m); err != nil {
		t.Fatal(err)
	}
	if m["password"] != "original-secret" {
		t.Fatalf("blank password on update must keep the stored one, got %v", m["password"])
	}
	if m["remote_path"] != "/new" {
		t.Fatalf("non-secret field must take the new value, got %v", m["remote_path"])
	}
}

// TestMergeStorageAdditionSecretsHonorsExplicitNewValue confirms the merge
// only protects blank resubmissions, never a deliberate new value.
func TestMergeStorageAdditionSecretsHonorsExplicitNewValue(t *testing.T) {
	old := `{"password":"original-secret"}`
	incoming := `{"password":"deliberately-changed"}`
	merged := op.MergeStorageAdditionSecrets("Crypt", old, incoming)
	var m map[string]any
	if err := json.Unmarshal([]byte(merged), &m); err != nil {
		t.Fatal(err)
	}
	if m["password"] != "deliberately-changed" {
		t.Fatalf("an explicitly-submitted non-blank password must be honored, got %v", m["password"])
	}
}

// TestRedactStorageAdditionRedactsTypeConfusedSecretValues pins review round
// 1's item 3: a secret field submitted as a JSON object, number, or array
// (not a plain string) must still be redacted — `v.(string)` failing must
// not be conflated with "genuinely empty" and let the value pass through in
// plaintext.
func TestRedactStorageAdditionRedactsTypeConfusedSecretValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"object", `{"password":{"nested":"s3cr3t-object-must-not-leak"},"remote_path":"/x"}`},
		{"number", `{"password":12345,"remote_path":"/x"}`},
		{"array", `{"password":["s3cr3t-array-must-not-leak"],"remote_path":"/x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := model.Storage{Driver: "Crypt", Addition: tc.json}
			redacted := op.RedactStorageAddition(storage)
			if strings.Contains(redacted.Addition, "s3cr3t") || strings.Contains(redacted.Addition, "12345") {
				t.Fatalf("REGRESSION: type-confused secret value leaked into redacted Addition: %s", redacted.Addition)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(redacted.Addition), &m); err != nil {
				t.Fatal(err)
			}
			if m["password"] != "" {
				t.Fatalf("password must be blanked regardless of its original type, got %v (%T)", m["password"], m["password"])
			}
			if m["password_set"] != true {
				t.Fatalf("password_set must be true, got %v", m["password_set"])
			}
			if m["remote_path"] != "/x" {
				t.Fatalf("non-secret field must pass through unchanged, got %v", m["remote_path"])
			}
		})
	}
}

// TestRedactStorageAdditionLeavesJSONNullSecretFieldAbsent confirms an
// explicit JSON null (as opposed to an empty string) is still treated as
// "nothing stored" — the "_set absent means never configured" contract
// applies to null too, not just "".
func TestRedactStorageAdditionLeavesJSONNullSecretFieldAbsent(t *testing.T) {
	storage := model.Storage{Driver: "Crypt", Addition: `{"password":null,"remote_path":"/x"}`}
	redacted := op.RedactStorageAddition(storage)
	var m map[string]any
	if err := json.Unmarshal([]byte(redacted.Addition), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["password_set"]; ok {
		t.Fatalf("password_set must be absent for a JSON null secret field, got %v", m["password_set"])
	}
}
