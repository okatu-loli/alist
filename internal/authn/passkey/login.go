package passkey

import (
	"errors"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// ErrClonedAuthenticator is returned when the signature counter in an assertion
// did not advance past the stored value. go-webauthn flags this as a
// CloneWarning rather than failing the ceremony; this engine treats it as a hard
// rejection — a stalled or rolled-back counter means the private key may have
// been copied.
var ErrClonedAuthenticator = errors.New("passkey: authenticator sign counter did not advance (possible clone)")

// BeginDiscoverableLogin starts a passwordless (usernameless) login ceremony.
// allowCredentials is empty, so the browser lets the user pick any resident
// credential for this RP. The returned SessionData must be stashed server-side
// and handed back to FinishDiscoverableLogin.
func BeginDiscoverableLogin(w *webauthn.WebAuthn) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	return w.BeginDiscoverableLogin()
}

// FinishDiscoverableLogin verifies a discoverable-login assertion. handler maps
// the assertion's (rawID, userHandle) to the owning webauthn.User — the caller
// supplies it (in W2, a credentialID → user lookup over the user table).
//
// On success it returns the resolved user and the updated *webauthn.Credential
// whose Authenticator.SignCount the caller must persist back onto the stored
// passkey. The signature counter is enforced strictly: if it failed to advance
// (clone signal), the ceremony is rejected with ErrClonedAuthenticator even
// though the cryptographic assertion itself verified.
func FinishDiscoverableLogin(w *webauthn.WebAuthn, session webauthn.SessionData, responseJSON []byte, handler webauthn.DiscoverableUserHandler) (webauthn.User, *webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(responseJSON)
	if err != nil {
		return nil, nil, err
	}
	user, cred, err := w.ValidatePasskeyLogin(handler, session, parsed)
	if err != nil {
		return nil, nil, err
	}
	if cred.Authenticator.CloneWarning {
		return nil, nil, ErrClonedAuthenticator
	}
	return user, cred, nil
}
