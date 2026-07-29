package extension

import (
	"crypto/ed25519"
	"encoding/hex"
)

// CoreAPIVersion is the host-declared core plugin API version (9.3). It tracks
// the ext module's v0.x line: a plugin declares a semver constraint on it in
// Manifest.CoreAPI (e.g. "^0.1.0") and an incompatible constraint is rejected
// at install time. Bump this when the ext contract makes a breaking change.
const CoreAPIVersion = "0.1.0"

// officialPublicKeyHex is the embedded trust root: the ed25519 public key whose
// signature marks a plugin package as official (9.3 gating — only packages
// verifying against this key may install and run via gRPC). The matching
// private key is NOT in the repo; official packages are signed out-of-band with
// `alist plugin sign` (cmd/plugin_sign.go).
//
// Additional trust roots come from admin-imported plugin sources (sources.go):
// a package signed by an imported source's key is 🟡 source-verified.
const officialPublicKeyHex = "1bac9b2899f66a6a0c9214bf98eefb10fba1f705ad01d1ec51d4eaac646dd6ca"

// officialPublicKey decodes the embedded trust root once at init. A malformed
// constant is a build-time authoring error, so it panics rather than degrading
// to "nothing verifies".
var officialPublicKey = mustDecodeKey(officialPublicKeyHex)

func mustDecodeKey(h string) ed25519.PublicKey {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != ed25519.PublicKeySize {
		panic("extension: malformed embedded official public key")
	}
	return ed25519.PublicKey(b)
}

// OfficialVerifier returns a Verifier whose only trust root is the embedded
// official key. This is the gate for 9.3: install/enable reject any package that
// does not verify against it.
func OfficialVerifier() *Verifier {
	return &Verifier{keys: []ed25519.PublicKey{officialPublicKey}}
}

// OfficialPublicKeyHex exposes the embedded official trust root (hex) for
// display surfaces (e.g. the plugin-sources admin list).
func OfficialPublicKeyHex() string { return officialPublicKeyHex }
