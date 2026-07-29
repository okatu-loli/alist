package bootstrap

import "github.com/alist-org/alist/v3/internal/authn/passkey"

// InitPasskey registers the built-in passkey login as a trusted in-process
// AuthNMethod so passwordless login is available (and, being trusted, usable by
// admins). It only records the method in the extension registry; the credential
// ceremonies run later, per request, reading settings live — so this can run at
// boot regardless of storage/plugin readiness.
func InitPasskey() {
	passkey.RegisterBuiltin()
}
