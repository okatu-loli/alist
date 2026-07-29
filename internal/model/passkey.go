package model

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// Passkey is one registered WebAuthn credential belonging to a user. A user may
// register several (one per device). The set is serialized as a JSON array into
// the User.Authn column; see ParsePasskeys/MarshalPasskeys.
type Passkey struct {
	// ID is base64url(RawURLEncoding) of the raw credential ID — the stable
	// handle a discoverable-login assertion carries back to identify the key.
	ID string `json:"id"`
	// PublicKey is the COSE-encoded credential public key.
	PublicKey []byte `json:"public_key"`
	// SignCount is the authenticator's signature counter as last observed. It
	// must only ever move forward; a stall or rollback signals a clone.
	SignCount uint32 `json:"sign_count"`
	// AAGUID identifies the authenticator model, formatted as a UUID string
	// (empty when the authenticator reported no AAGUID).
	AAGUID string `json:"aaguid"`
	// Name is a user-chosen label for the passkey ("MacBook Touch ID").
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	// Transports the authenticator advertised ("internal", "usb", "hybrid"...).
	Transports     []string `json:"transports"`
	BackupEligible bool     `json:"backup_eligible"`
	BackupState    bool     `json:"backup_state"`
}

// ParsePasskeys decodes the User.Authn column into a slice of Passkey. An empty
// (or whitespace-only) string yields an empty, non-nil slice — a user with no
// registered passkeys is the common case, not an error.
func ParsePasskeys(authn string) ([]Passkey, error) {
	if authn == "" {
		return []Passkey{}, nil
	}
	var pks []Passkey
	if err := json.Unmarshal([]byte(authn), &pks); err != nil {
		return nil, err
	}
	if pks == nil {
		pks = []Passkey{}
	}
	return pks, nil
}

// MarshalPasskeys encodes a slice of Passkey for storage in User.Authn. A nil or
// empty slice serializes to "" so the column round-trips back to an empty list.
func MarshalPasskeys(pks []Passkey) (string, error) {
	if len(pks) == 0 {
		return "", nil
	}
	b, err := json.Marshal(pks)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// NewPasskeyFromCredential builds a Passkey from a freshly verified go-webauthn
// Credential (the output of a registration ceremony). name is the user-chosen
// label; now stamps both created and last-used.
func NewPasskeyFromCredential(cred *webauthn.Credential, name string, now time.Time) Passkey {
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	return Passkey{
		ID:             base64.RawURLEncoding.EncodeToString(cred.ID),
		PublicKey:      cred.PublicKey,
		SignCount:      cred.Authenticator.SignCount,
		AAGUID:         encodeAAGUID(cred.Authenticator.AAGUID),
		Name:           name,
		CreatedAt:      now,
		LastUsedAt:     now,
		Transports:     transports,
		BackupEligible: cred.Flags.BackupEligible,
		BackupState:    cred.Flags.BackupState,
	}
}

// ToCredential converts a stored Passkey back into a go-webauthn Credential so it
// can participate in a login ceremony. It restores the fields validateLogin
// inspects: ID, public key, sign counter, AAGUID and the backup flags.
func (p Passkey) ToCredential() (webauthn.Credential, error) {
	id, err := base64.RawURLEncoding.DecodeString(p.ID)
	if err != nil {
		return webauthn.Credential{}, err
	}
	aaguid, err := decodeAAGUID(p.AAGUID)
	if err != nil {
		return webauthn.Credential{}, err
	}
	transports := make([]protocol.AuthenticatorTransport, 0, len(p.Transports))
	for _, t := range p.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID:        id,
		PublicKey: p.PublicKey,
		Transport: transports,
		Flags: webauthn.CredentialFlags{
			BackupEligible: p.BackupEligible,
			BackupState:    p.BackupState,
		},
		Authenticator: webauthn.Authenticator{
			AAGUID:    aaguid,
			SignCount: p.SignCount,
		},
	}, nil
}

func encodeAAGUID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if id, err := uuid.FromBytes(raw); err == nil {
		return id.String()
	}
	// Non-16-byte AAGUID is out of spec; fall back to base64url so it round-trips.
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAAGUID(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if id, err := uuid.Parse(s); err == nil {
		b := id
		return b[:], nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// --- webauthn.User implementation ---------------------------------------------
//
// model.User implements webauthn.User so it can drive go-webauthn's Begin/Finish
// ceremonies directly. The user handle is the 8-byte big-endian user ID: stable,
// opaque, and reversible (UserIDFromWebAuthnHandle) for discoverable login.

// WebAuthnID returns the opaque, stable user handle: the primary key encoded as
// 8 big-endian bytes.
func (u *User) WebAuthnID() []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(u.ID))
	return b
}

// WebAuthnName returns the human-palatable account name.
func (u *User) WebAuthnName() string { return u.Username }

// WebAuthnDisplayName returns the display name; alist has no separate display
// name, so the username is reused.
func (u *User) WebAuthnDisplayName() string { return u.Username }

// WebAuthnCredentials maps the user's stored passkeys to go-webauthn
// Credentials. A corrupt Authn column yields no credentials rather than an
// error — the interface has no error channel, and a login simply finds no match.
func (u *User) WebAuthnCredentials() []webauthn.Credential {
	pks, err := ParsePasskeys(u.Authn)
	if err != nil {
		return nil
	}
	creds := make([]webauthn.Credential, 0, len(pks))
	for _, pk := range pks {
		c, err := pk.ToCredential()
		if err != nil {
			continue
		}
		creds = append(creds, c)
	}
	return creds
}

// UserIDFromWebAuthnHandle reverses WebAuthnID: it decodes an 8-byte user handle
// carried by a discoverable-login assertion back into a user primary key.
func UserIDFromWebAuthnHandle(handle []byte) (uint, bool) {
	if len(handle) != 8 {
		return 0, false
	}
	return uint(binary.BigEndian.Uint64(handle)), true
}
