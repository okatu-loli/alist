package crypto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// EncPrefixV1 marks a JSON object field's value as AES-256-GCM-sealed
// (Seal) under a shared key, base64-standard-encoded after the prefix. See
// EncryptJSONFields/DecryptJSONFields — the version tag lets a future
// algorithm/format change (v2) coexist with old rows during a rollout, and
// lets a value with NO such prefix be recognized as still-plaintext (a
// pre-encryption row) rather than mistaken for ciphertext.
const EncPrefixV1 = "enc:v1:"

// RejectReservedPrefix returns an error if any field named in fields within
// obj holds a plaintext string value that already starts with EncPrefixV1.
//
// [Minor 10, G4 red team] EncryptJSONFields treats ANY string with this
// prefix as already-sealed ciphertext and skips it — that is deliberate and
// safe for a value round-tripped from storage (the "leave unchanged" case:
// a blank field in an update gets the OLD, already-encrypted value merged
// back in verbatim before EncryptJSONFields ever runs). But a FRESH value a
// user directly submits — a password/token they typed — that happens to
// collide with this exact prefix would, by that same skip rule, be stored
// completely unencrypted, indistinguishable from real ciphertext to every
// later reader, forever.
//
// Callers accepting fresh secret input from a user must call this on
// exactly the caller-submitted value, BEFORE any merge-with-old-value step
// (which would otherwise make an already-real, legitimately-prefixed
// ciphertext value indistinguishable from the case this rejects) and before
// encryption.
func RejectReservedPrefix(obj json.RawMessage, fields map[string]bool) error {
	if len(fields) == 0 || len(obj) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(obj, &m); err != nil {
		return nil
	}
	for name := range fields {
		v, ok := m[name]
		if !ok {
			continue
		}
		if s, isString := v.(string); isString && strings.HasPrefix(s, EncPrefixV1) {
			return fmt.Errorf("crypto: field %q must not start with the reserved prefix %q", name, EncPrefixV1)
		}
	}
	return nil
}

// EncryptJSONFields seals every field named in fields within obj (a JSON
// object) that isn't already sealed — idempotent: a value already carrying
// EncPrefixV1 is left alone, so calling this again on an already-encrypted
// obj is a no-op for those fields — replacing it with
// "EncPrefixV1<base64>". A field absent from obj, or whose value isn't a
// non-empty string, is left untouched (an empty string or a non-string
// value is not this function's problem — see the field-type-confusion note
// on the internal/op and internal/extension redaction functions this
// mirrors; encryption only ever acts on genuinely-present string secrets).
//
// getKey is called AT MOST ONCE, and only if at least one field actually
// needs sealing — a caller whose secret fields are all blank/absent never
// pays for (or fails on) resolving a key it doesn't need.
//
// Returns obj UNCHANGED (not an error) if obj isn't a JSON object or fields
// is empty — a malformed/legacy value must not make an otherwise-unrelated
// save fail outright. Sealing itself failing (getKey erroring, or the AEAD
// erroring) IS reported as an error: silently persisting plaintext when
// encryption was supposed to happen defeats the entire point of calling
// this function.
func EncryptJSONFields(getKey func() ([]byte, error), obj json.RawMessage, fields map[string]bool) (json.RawMessage, error) {
	if len(fields) == 0 || len(obj) == 0 {
		return obj, nil
	}
	var m map[string]any
	if err := json.Unmarshal(obj, &m); err != nil {
		return obj, nil
	}
	var key []byte
	changed := false
	for name := range fields {
		v, ok := m[name]
		if !ok {
			continue
		}
		s, isString := v.(string)
		if !isString || s == "" || strings.HasPrefix(s, EncPrefixV1) {
			continue
		}
		if key == nil {
			k, err := getKey()
			if err != nil {
				return nil, err
			}
			key = k
		}
		sealed, err := Seal(key, []byte(s))
		if err != nil {
			return nil, fmt.Errorf("crypto: encrypt field %q: %w", name, err)
		}
		m[name] = EncPrefixV1 + base64.StdEncoding.EncodeToString(sealed)
		changed = true
	}
	if !changed {
		return obj, nil
	}
	// [Minor 9, G4 red team] changed is only ever true here because at least
	// one field was JUST sealed above — a Marshal failure at this point must
	// NOT fall back to returning obj unchanged: obj is the ORIGINAL,
	// UNENCRYPTED payload, and doing so would silently hand the caller back
	// plaintext with a nil error, exactly as if encryption had succeeded.
	// That defeats the entire point of calling this function with no
	// indication anything went wrong. (In practice m only ever holds values
	// encoding/json itself just decoded — string/float64/bool/nil/map/slice
	// — so this marshal essentially cannot fail, but "essentially never"
	// is not the same guarantee as "the failure path is safe".)
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("crypto: marshal sealed object: %w", err)
	}
	return b, nil
}

// DecryptJSONFields reverses EncryptJSONFields: every field named in fields
// whose value carries EncPrefixV1 is opened back to plaintext. A value with
// no such prefix passes through unchanged (lazy migration: a pre-encryption
// row, or a non-secret field, reads fine as-is — the next EncryptJSONFields
// call upgrades it).
//
// getKey is called at most once, and only if at least one field actually
// needs opening.
//
// Returns obj unchanged (not an error) if it isn't a JSON object or fields
// is empty. Returns a real error (never a partial/garbage result) if a
// prefixed value fails to decrypt (getKey erroring, malformed base64, or
// AEAD authentication failure) — the caller must treat this as fatal to
// whatever operation is reading the value, never swallow it into an empty
// string or garbage plaintext.
func DecryptJSONFields(getKey func() ([]byte, error), obj json.RawMessage, fields map[string]bool) (json.RawMessage, error) {
	if len(fields) == 0 || len(obj) == 0 {
		return obj, nil
	}
	var m map[string]any
	if err := json.Unmarshal(obj, &m); err != nil {
		return obj, nil
	}
	var key []byte
	changed := false
	for name := range fields {
		v, ok := m[name]
		if !ok {
			continue
		}
		s, isString := v.(string)
		if !isString || !strings.HasPrefix(s, EncPrefixV1) {
			continue
		}
		if key == nil {
			k, err := getKey()
			if err != nil {
				return nil, err
			}
			key = k
		}
		sealed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, EncPrefixV1))
		if err != nil {
			return nil, fmt.Errorf("crypto: decrypt field %q: malformed ciphertext: %w", name, err)
		}
		pt, err := Open(key, sealed)
		if err != nil {
			return nil, fmt.Errorf("crypto: decrypt field %q: %w", name, err)
		}
		m[name] = string(pt)
		changed = true
	}
	if !changed {
		return obj, nil
	}
	// Same reasoning as EncryptJSONFields's identical guard (Minor 9): changed
	// is only true because a field was just opened above, so falling back to
	// obj here would silently hand back STILL-ENCRYPTED ciphertext dressed up
	// as a successful decrypt (nil error) — a different failure shape than
	// the plaintext-leak Encrypt side risks, but the same "wrong data,
	// reported as success" defect.
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("crypto: marshal opened object: %w", err)
	}
	return b, nil
}
