// Package crypto holds the small set of symmetric encryption-at-rest
// primitives every "secret needs to survive a restart, encrypted" call site
// in this codebase needs: AES-256-GCM seal/open, plus a key file that is
// generated once, persisted atomically (0600), and reused across restarts.
//
// This is an extraction (G4, secret 一等公民 step 2), not a new design:
// internal/extension/hostcap/secrets.go implemented exactly this — sealing
// Host.Store().Secrets() values with AES-256-GCM under a key persisted at
// LoadOrCreateSecretsKey — and has been running in production since W6b.
// hostcap's Seal/Open/key-file logic now calls straight through to this
// package (see secrets.go); hostcap's own exported names, error strings, and
// on-disk format (nonce(12)+ciphertext+tag(16), key file mode 0600, atomic
// temp+rename+reread) are unchanged, so its existing test suite is the
// regression gate for this extraction — it must stay green untouched.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// KeySize is the required symmetric key length for Seal/Open (AES-256).
const KeySize = 32

// SecretsKeyFile is the filename the process-wide shared secrets key is
// persisted under (see LoadOrCreateKey) — used by every caller that wants
// THE one secrets key rather than an independent, purpose-specific one:
// internal/extension/hostcap's plugin Store().Secrets() KV, storage
// Addition field encryption (internal/op), and plugin config_form
// encryption (internal/extension) all resolve to the identical key file via
// extension.Manager.SecretsKey (G4 §1: extracted so all three share one key
// and therefore one backup/restore story, §4). A caller that genuinely
// wants an independent key unrelated to this shared one should pass its own
// filename to LoadOrCreateKey instead of this constant.
const SecretsKeyFile = ".secrets.key"

// ErrOpenFailed is returned by Open when the sealed blob cannot be
// authenticated/decrypted: too short to contain a nonce, truncated,
// tampered, or sealed under a different key. Callers must fail closed —
// Open never returns a partial or garbage plaintext, only this sentinel.
var ErrOpenFailed = errors.New("crypto: value failed to decrypt (corrupt, tampered, or wrong key)")

// Seal encrypts plaintext with AES-256-GCM under key (must be exactly
// KeySize bytes) and returns nonce||ciphertext||tag: a fresh random 12-byte
// nonce is generated per call and prepended to the output (GCM's standard
// construction), so no nonce is ever reused for a given key.
func Seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a Seal'd blob under key, returning ErrOpenFailed (never a
// partial/garbage plaintext) if it's too short or fails authentication.
func Open(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, ErrOpenFailed
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrOpenFailed
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// LoadOrCreateKey loads a KeySize-byte key persisted at <dir>/<filename>,
// generating and atomically persisting (temp file + chmod 0600 + rename) a
// fresh random one on first use. Subsequent calls over the same dir and
// filename return the same key, so anything encrypted under it survives a
// process restart. The key is never logged. A wrong-length existing file is
// a hard error, never silently regenerated (that would make everything
// already sealed under the old key permanently undecryptable with no
// indication why).
func LoadOrCreateKey(dir, filename string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filename)

	if data, err := os.ReadFile(path); err == nil {
		if len(data) != KeySize {
			return nil, fmt.Errorf("crypto: key file %s has invalid length %d, want %d", path, len(data), KeySize)
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := atomicWriteFile0600(dir, filename, key); err != nil {
		return nil, err
	}

	// Re-read from disk rather than trusting the in-memory key: if another
	// process/goroutine raced us and won the rename, this makes every caller
	// converge on whichever key actually landed, instead of two callers each
	// believing a different key is "the" key.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != KeySize {
		return nil, fmt.Errorf("crypto: key file %s has invalid length %d, want %d", path, len(data), KeySize)
	}
	return data, nil
}

// WriteKeyFile atomically persists key (must be exactly KeySize bytes) at
// <dir>/<filename> with mode 0600, OVERWRITING any existing file there —
// unlike LoadOrCreateKey, which never touches a key that already exists.
// Used to write a secrets key back onto disk verbatim on restore (G4 §4: a
// backup archive that carried the key, e.g. after moving to a different
// machine, or recovering a wiped data dir on the same one).
func WriteKeyFile(dir, filename string, key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("crypto: key must be %d bytes, got %d", KeySize, len(key))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicWriteFile0600(dir, filename, key)
}

// atomicWriteFile0600 writes data to <dir>/<filename> via temp file + chmod
// 0600 + rename, so a concurrent reader never observes a partially-written
// or wrong-permission key file. dir must already exist.
//
// [I4, G4 red team] Also fsyncs the temp file's data before renaming, and
// fsyncs the directory after: without the first, a crash right after Rename
// can leave the renamed file present but still holding whatever was in the
// page cache (for a key file, that can mean "zero bytes" or leftover
// garbage) — nothing about Rename alone forces the preceding Write to disk.
// Without the second, the rename itself (the directory entry pointing the
// name at the new inode) is not guaranteed durable either. For a key file
// specifically, either gap can produce "ciphertext already committed
// elsewhere (e.g. the DB, fsynced by the DB engine itself), key file lost
// or empty" — an unrecoverable pairing a plain rename-without-fsync does not
// protect against.
func atomicWriteFile0600(dir, filename string, data []byte) error {
	path := filepath.Join(dir, filename)
	tmp, err := os.CreateTemp(dir, filename+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("crypto: fsync temp key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("crypto: fsync key file directory: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so a preceding create/rename inside it is
// durable across a crash, not just visible to other processes in the
// meantime. Windows does not support fsyncing a directory handle the same
// way POSIX does (opening a directory for Sync is not a supported
// operation there) — the data-level fsync in atomicWriteFile0600 above is
// the part that actually protects the key's bytes, so this is best-effort
// on that platform rather than a hard failure.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}
