package model

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

func TestParsePasskeys_EmptyString(t *testing.T) {
	pks, err := ParsePasskeys("")
	if err != nil {
		t.Fatalf("empty string should not error: %v", err)
	}
	if pks == nil {
		t.Fatal("empty string should yield a non-nil slice")
	}
	if len(pks) != 0 {
		t.Fatalf("want 0 passkeys, got %d", len(pks))
	}
}

func TestParsePasskeys_Garbage(t *testing.T) {
	if _, err := ParsePasskeys("not json"); err == nil {
		t.Fatal("garbage should error")
	}
}

func TestMarshalPasskeys_EmptyRoundTrips(t *testing.T) {
	for _, in := range [][]Passkey{nil, {}} {
		s, err := MarshalPasskeys(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if s != "" {
			t.Fatalf("empty slice must marshal to empty string, got %q", s)
		}
	}
}

func TestPasskeys_RoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second).UTC()
	in := []Passkey{
		{
			ID:             base64.RawURLEncoding.EncodeToString([]byte("cred-1")),
			PublicKey:      []byte{1, 2, 3, 4},
			SignCount:      7,
			AAGUID:         uuid.New().String(),
			Name:           "MacBook",
			CreatedAt:      now,
			LastUsedAt:     now,
			Transports:     []string{"internal", "hybrid"},
			BackupEligible: true,
			BackupState:    true,
		},
	}
	s, err := MarshalPasskeys(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := ParsePasskeys(s)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1, got %d", len(out))
	}
	if !passkeyEqual(out[0], in[0]) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in[0], out[0])
	}
}

func passkeyEqual(a, b Passkey) bool {
	if a.ID != b.ID || a.SignCount != b.SignCount || a.AAGUID != b.AAGUID ||
		a.Name != b.Name || a.BackupEligible != b.BackupEligible || a.BackupState != b.BackupState {
		return false
	}
	if !a.CreatedAt.Equal(b.CreatedAt) || !a.LastUsedAt.Equal(b.LastUsedAt) {
		return false
	}
	if len(a.PublicKey) != len(b.PublicKey) || len(a.Transports) != len(b.Transports) {
		return false
	}
	for i := range a.PublicKey {
		if a.PublicKey[i] != b.PublicKey[i] {
			return false
		}
	}
	for i := range a.Transports {
		if a.Transports[i] != b.Transports[i] {
			return false
		}
	}
	return true
}

func TestCredentialConversion(t *testing.T) {
	aaguid := uuid.New()
	cred := &webauthn.Credential{
		ID:        []byte("credential-id-bytes"),
		PublicKey: []byte{9, 8, 7},
		Transport: []protocol.AuthenticatorTransport{"internal"},
		Flags:     webauthn.CredentialFlags{BackupEligible: true, BackupState: false},
		Authenticator: webauthn.Authenticator{
			AAGUID:    aaguid[:],
			SignCount: 42,
		},
	}
	now := time.Now()
	pk := NewPasskeyFromCredential(cred, "Yubikey", now)
	if pk.ID != base64.RawURLEncoding.EncodeToString(cred.ID) {
		t.Fatalf("id mismatch: %q", pk.ID)
	}
	if pk.SignCount != 42 {
		t.Fatalf("signcount mismatch: %d", pk.SignCount)
	}
	if pk.AAGUID != aaguid.String() {
		t.Fatalf("aaguid mismatch: %q vs %q", pk.AAGUID, aaguid.String())
	}
	if !pk.BackupEligible || pk.BackupState {
		t.Fatalf("flags mismatch: %+v", pk)
	}

	back, err := pk.ToCredential()
	if err != nil {
		t.Fatalf("ToCredential: %v", err)
	}
	if string(back.ID) != string(cred.ID) {
		t.Fatalf("credential id round-trip: %q", back.ID)
	}
	if back.Authenticator.SignCount != 42 {
		t.Fatalf("signcount round-trip: %d", back.Authenticator.SignCount)
	}
	if string(back.Authenticator.AAGUID) != string(aaguid[:]) {
		t.Fatal("aaguid round-trip mismatch")
	}
	if !back.Flags.BackupEligible {
		t.Fatal("backup eligible flag lost")
	}
}

func TestWebAuthnUser(t *testing.T) {
	u := &User{ID: 123456789, Username: "alice"}
	if u.WebAuthnName() != "alice" || u.WebAuthnDisplayName() != "alice" {
		t.Fatal("name/displayname should be username")
	}
	handle := u.WebAuthnID()
	if len(handle) != 8 {
		t.Fatalf("handle should be 8 bytes, got %d", len(handle))
	}
	id, ok := UserIDFromWebAuthnHandle(handle)
	if !ok || id != 123456789 {
		t.Fatalf("handle round-trip failed: id=%d ok=%v", id, ok)
	}
	if _, ok := UserIDFromWebAuthnHandle([]byte{1, 2, 3}); ok {
		t.Fatal("short handle should fail")
	}
}

func TestWebAuthnCredentials_CorruptAuthn(t *testing.T) {
	u := &User{ID: 1, Username: "bob", Authn: "{{not json"}
	if creds := u.WebAuthnCredentials(); len(creds) != 0 {
		t.Fatalf("corrupt authn should yield no credentials, got %d", len(creds))
	}
}

func TestWebAuthnCredentials_FromStored(t *testing.T) {
	pk := Passkey{
		ID:         base64.RawURLEncoding.EncodeToString([]byte("abc")),
		PublicKey:  []byte{1},
		AAGUID:     uuid.New().String(),
		Transports: []string{"internal"},
	}
	s, err := MarshalPasskeys([]Passkey{pk})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	u := &User{ID: 1, Username: "carol", Authn: s}
	creds := u.WebAuthnCredentials()
	if len(creds) != 1 {
		t.Fatalf("want 1 credential, got %d", len(creds))
	}
	if string(creds[0].ID) != "abc" {
		t.Fatalf("credential id mismatch: %q", creds[0].ID)
	}
}
