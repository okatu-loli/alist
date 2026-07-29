package bootstrap

import "github.com/alist-org/alist/v3/internal/authn/totp"

// InitTOTP registers the built-in TOTP second factor as a trusted in-process
// AuthNMethod (Phase C: internal TOTP second factor), mirroring InitPasskey.
// It only records the method in the extension registry; the actual
// setup/step-up ceremonies run later, per request, reading the process-wide
// totp.Default service — so this can run at boot regardless of storage/plugin
// readiness.
func InitTOTP() {
	totp.RegisterBuiltin()
}
