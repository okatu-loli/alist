package utils

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// tokenSaltBytes is the salt size used by HashToken.
const tokenSaltBytes = 16

// HashToken returns a salted, irreversible digest of token suitable for
// storing the admin API token at rest (see conf.TokenPlaintext).
// The encoding is "<saltHex>$<digestHex>" where digestHex is
// sha256(salt || token); a fresh random salt is drawn from crypto/rand on
// every call, so hashing the same token twice yields different stored
// values.
//
// A salted fast hash (sha256), not a slow KDF (bcrypt/scrypt/argon2), is
// used deliberately: middlewares.Auth verifies the admin token on every
// authenticated HTTP request, so the check must stay cheap (microseconds).
// The token itself is a high-entropy random string
// (pkg/utils/random.Token), not a human password, so brute-forcing it is
// infeasible regardless of hash speed; the salt still defeats precomputed
// rainbow tables, and sha256 is one-way, so the stored value cannot be
// reversed to recover the token.
func HashToken(token string) (string, error) {
	salt := make([]byte, tokenSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodeTokenHash(salt, token), nil
}

func encodeTokenHash(salt []byte, token string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(token))
	digest := h.Sum(nil)
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(digest)
}

// VerifyToken reports whether provided hashes to the same salted digest as
// stored, a "<saltHex>$<digestHex>" value previously produced by HashToken.
// The comparison is constant-time. A malformed stored value (wrong shape,
// non-hex parts) never matches — it simply returns false — so callers can
// use this as a plain boolean gate without a separate error check.
func VerifyToken(stored, provided string) bool {
	saltHex, digestHex, ok := strings.Cut(stored, "$")
	if !ok || saltHex == "" || digestHex == "" {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	if _, err := hex.DecodeString(digestHex); err != nil {
		return false
	}
	candidate := encodeTokenHash(salt, provided)
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(stored)) == 1
}
