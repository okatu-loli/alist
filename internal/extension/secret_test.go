package extension

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactSecretConfigFieldsBlanksSecretAndMarksSet mirrors
// op.RedactStorageAddition's contract one config shape over: a configured
// secret field is blanked and marked "<field>_set": true; a non-secret
// field passes through unchanged.
func TestRedactSecretConfigFieldsBlanksSecretAndMarksSet(t *testing.T) {
	cfg := json.RawMessage(`{"api_token":"s3cr3t-must-not-leak","label":"prod"}`)
	out := RedactSecretConfigFields(cfg, map[string]bool{"api_token": true})
	if strings.Contains(string(out), "s3cr3t-must-not-leak") {
		t.Fatalf("secret value leaked into redacted config: %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["api_token"] != "" {
		t.Fatalf("api_token must be blanked, got %v", m["api_token"])
	}
	if m["api_token_set"] != true {
		t.Fatalf("api_token_set must be true, got %v", m["api_token_set"])
	}
	if m["label"] != "prod" {
		t.Fatalf("non-secret field must pass through unchanged, got %v", m["label"])
	}
}

// TestRedactSecretConfigFieldsLeavesNeverSetFieldAbsent pins the "_set
// absent means never configured" half of the contract.
func TestRedactSecretConfigFieldsLeavesNeverSetFieldAbsent(t *testing.T) {
	cfg := json.RawMessage(`{"api_token":"","label":"prod"}`)
	out := RedactSecretConfigFields(cfg, map[string]bool{"api_token": true})
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["api_token_set"]; ok {
		t.Fatalf("api_token_set must be absent when nothing was ever stored, got %v", m["api_token_set"])
	}
}

// TestRedactSecretConfigFieldsNoSecretFieldsUnchanged pins the best-effort
// no-op: nil/empty secretFields (no config_form, or none of its properties
// marked x-secret) leaves cfg byte-for-byte unchanged.
func TestRedactSecretConfigFieldsNoSecretFieldsUnchanged(t *testing.T) {
	cfg := json.RawMessage(`{"api_token":"x"}`)
	out := RedactSecretConfigFields(cfg, nil)
	if string(out) != string(cfg) {
		t.Fatalf("no secret fields must leave cfg unchanged, got %s", out)
	}
}

// TestMergeSecretConfigFieldsKeepsOldOnBlank pins the "blank means keep"
// merge rule mirroring MergeStorageAdditionSecrets/UpdateSSOProvider.
func TestMergeSecretConfigFieldsKeepsOldOnBlank(t *testing.T) {
	old := json.RawMessage(`{"api_token":"original-secret","label":"prod"}`)
	incoming := json.RawMessage(`{"api_token":"","label":"staging"}`)
	merged := MergeSecretConfigFields(old, incoming, map[string]bool{"api_token": true})
	var m map[string]any
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatal(err)
	}
	if m["api_token"] != "original-secret" {
		t.Fatalf("blank api_token on update must keep the stored one, got %v", m["api_token"])
	}
	if m["label"] != "staging" {
		t.Fatalf("non-secret field must take the new value, got %v", m["label"])
	}
}

// TestMergeSecretConfigFieldsHonorsExplicitNewValue confirms the merge only
// protects blank resubmissions, never a deliberate new value.
func TestMergeSecretConfigFieldsHonorsExplicitNewValue(t *testing.T) {
	old := json.RawMessage(`{"api_token":"original-secret"}`)
	incoming := json.RawMessage(`{"api_token":"deliberately-changed"}`)
	merged := MergeSecretConfigFields(old, incoming, map[string]bool{"api_token": true})
	var m map[string]any
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatal(err)
	}
	if m["api_token"] != "deliberately-changed" {
		t.Fatalf("an explicitly-submitted non-blank token must be honored, got %v", m["api_token"])
	}
}

// TestMergeSecretConfigFieldsEmptyOldCfgLeavesBlankAsIs pins the
// no-prior-value case: nothing was ever saved (oldCfg empty), so a blank
// incoming secret field has nothing to fall back to and stays blank.
func TestMergeSecretConfigFieldsEmptyOldCfgLeavesBlankAsIs(t *testing.T) {
	incoming := json.RawMessage(`{"api_token":""}`)
	merged := MergeSecretConfigFields(nil, incoming, map[string]bool{"api_token": true})
	var m map[string]any
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatal(err)
	}
	if m["api_token"] != "" {
		t.Fatalf("with no prior value to fall back to, api_token must stay blank, got %v", m["api_token"])
	}
}

// TestRedactSecretConfigFieldsRedactsTypeConfusedValues pins review round
// 1's item 3 one config shape over (see
// op.RedactStorageAddition's identical test): a secret field submitted as
// an object, number, or array must still be redacted.
func TestRedactSecretConfigFieldsRedactsTypeConfusedValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"object", `{"api_token":{"nested":"s3cr3t-object-must-not-leak"},"label":"prod"}`},
		{"number", `{"api_token":12345,"label":"prod"}`},
		{"array", `{"api_token":["s3cr3t-array-must-not-leak"],"label":"prod"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactSecretConfigFields(json.RawMessage(tc.json), map[string]bool{"api_token": true})
			if strings.Contains(string(out), "s3cr3t") || strings.Contains(string(out), "12345") {
				t.Fatalf("REGRESSION: type-confused secret value leaked into redacted config: %s", out)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			if m["api_token"] != "" {
				t.Fatalf("api_token must be blanked regardless of its original type, got %v (%T)", m["api_token"], m["api_token"])
			}
			if m["api_token_set"] != true {
				t.Fatalf("api_token_set must be true, got %v", m["api_token_set"])
			}
			if m["label"] != "prod" {
				t.Fatalf("non-secret field must pass through unchanged, got %v", m["label"])
			}
		})
	}
}

// TestRedactSecretConfigFieldsLeavesJSONNullFieldAbsent confirms an explicit
// JSON null is still treated as "nothing stored," same as an absent key.
func TestRedactSecretConfigFieldsLeavesJSONNullFieldAbsent(t *testing.T) {
	out := RedactSecretConfigFields(json.RawMessage(`{"api_token":null,"label":"prod"}`), map[string]bool{"api_token": true})
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["api_token_set"]; ok {
		t.Fatalf("api_token_set must be absent for a JSON null secret field, got %v", m["api_token_set"])
	}
}
