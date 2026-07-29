package passkey

import (
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// BeginRegistration starts a registration ceremony for an already-authenticated
// user (adding one more passkey). It requires a resident/discoverable key so the
// credential can later be used for passwordless login, and excludes the user's
// existing credentials so the same authenticator is not enrolled twice.
//
// The returned *protocol.CredentialCreation is sent to the browser
// (navigator.credentials.create); the *webauthn.SessionData must be stashed
// server-side (ChallengeStore) and handed back to FinishRegistration.
func BeginRegistration(w *webauthn.WebAuthn, user webauthn.User) (*protocol.CredentialCreation, *webauthn.SessionData, error) {
	return w.BeginRegistration(
		user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(user.WebAuthnCredentials()).CredentialDescriptors()),
	)
}

// FinishRegistration verifies the authenticator's attestation response against
// the stored session and produces a model.Passkey ready to be appended to the
// user's credential list (persistence is the caller's job). name is the
// user-chosen label; now stamps created/last-used.
func FinishRegistration(w *webauthn.WebAuthn, user webauthn.User, session webauthn.SessionData, responseJSON []byte, name string, now time.Time) (*model.Passkey, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(responseJSON)
	if err != nil {
		return nil, err
	}
	cred, err := w.CreateCredential(user, session, parsed)
	if err != nil {
		return nil, err
	}
	pk := model.NewPasskeyFromCredential(cred, name, now)
	return &pk, nil
}
