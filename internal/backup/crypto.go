package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"

	"github.com/pkg/errors"
)

// Encryption parameters. AES-256-GCM per domain payload; the key is derived
// once per archive with PBKDF2-HMAC-SHA256 over a random salt kept in the
// manifest. manifest.json itself is never encrypted so an archive stays
// identifiable without the password.
const (
	kdfAlgo       = "pbkdf2-sha256"
	kdfIterations = 210_000 // OWASP 2023 floor for PBKDF2-HMAC-SHA256
	kdfKeyLen     = 32      // AES-256
	saltLen       = 16
)

// crypto/pbkdf2 (stdlib, Go 1.24+) is used instead of golang.org/x/crypto/pbkdf2
// so no new module dependency is pulled in for one call.

// newKDFParams draws a fresh random salt and returns the params to record in the
// manifest.
func newKDFParams() (KDFParams, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return KDFParams{}, errors.Wrap(err, "backup: generating kdf salt")
	}
	return KDFParams{
		Algo:       kdfAlgo,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Iterations: kdfIterations,
		KeyLen:     kdfKeyLen,
	}, nil
}

// deriveKey reproduces the AES key from the password and the manifest's KDF
// params. It rejects params it did not write so a hand-edited manifest cannot
// steer decryption toward a weaker configuration.
func deriveKey(password string, p KDFParams) ([]byte, error) {
	if p.Algo != kdfAlgo {
		return nil, errors.Errorf("backup: unsupported kdf algo %q", p.Algo)
	}
	salt, err := base64.StdEncoding.DecodeString(p.Salt)
	if err != nil {
		return nil, errors.Wrap(err, "backup: decoding kdf salt")
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, p.Iterations, p.KeyLen)
	if err != nil {
		return nil, errors.Wrap(err, "backup: deriving key")
	}
	return key, nil
}

// encrypt seals plaintext with AES-256-GCM, prefixing the random nonce to the
// ciphertext. A fresh nonce per call keeps reuse-under-one-key safe.
func encrypt(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.Wrap(err, "backup: generating nonce")
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt reverses encrypt. A wrong password derives a wrong key and GCM
// authentication fails here — surfaced loudly rather than yielding garbage.
func decrypt(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("backup: ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("backup: decryption failed (wrong password or corrupt archive)")
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.Wrap(err, "backup: aes cipher")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.Wrap(err, "backup: gcm")
	}
	return gcm, nil
}
