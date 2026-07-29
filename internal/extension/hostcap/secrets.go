package hostcap

import (
	"errors"
	"fmt"

	"github.com/alist-org/alist/v3/internal/crypto"
)

// This file implements Host.Store().Secrets() (W6b, design docs
// v4-plugin-extensions-design.md §3): an encrypted-at-rest sibling of the
// plaintext KV, for a plugin holding OAuth tokens or API secrets it should not
// persist in the clear.
//
// Gate: Secrets reuses CapStore (the "store" host_api) rather than introducing
// a new manifest permission word — a plugin that can read/write its own
// namespaced KV is already trusted with arbitrary plugin-private data, and the
// admin-facing consent surface would gain a near-duplicate "can this plugin
// keep secrets" checkbox next to "can this plugin keep data" with no
// meaningfully different blast radius (both are already fully isolated per
// plugin id). A distinct CapSecrets would be justified if Secrets ever grew a
// capability KV lacks (e.g. host-mediated OAuth flows the plugin itself never
// sees the token for) — F0 has no such asymmetry yet.
//
// Isolation: KV() and Secrets() are backed by two DISJOINT KVBackend instances
// (see plugin_host_bridge.go's wiring — a plaintext DiskKVBackend rooted at the
// plugin data dir, a second DiskKVBackend rooted at a distinct "secrets"
// subdirectory, wrapped by encryptingKVBackend). A key written through one is
// structurally unreachable from the other, independent of what string the key
// is — not a naming convention (a "secret:" prefix) a caller could collide
// with by accident.
//
// The actual AES-256-GCM seal/open and key-file persistence live in
// internal/crypto (G4: extracted so internal/op's storage Addition secrets
// and internal/extension's plugin config secrets can reuse the identical,
// already-production primitive instead of a second implementation). This
// file is now a thin wrapper: encryptingKVBackend/NewEncryptingKVBackend/
// LoadOrCreateSecretsKey keep their exact pre-extraction names, signatures,
// error strings, and on-disk format — every test in secrets_test.go is the
// zero-behavior-change gate for this refactor.

// encryptingKVBackend wraps a KVBackend, sealing every value with AES-256-GCM
// before it reaches inner and opening it on read. Keys and the per-plugin
// namespacing are untouched (inner already enforces invariant 3); only values
// are encrypted.
type encryptingKVBackend struct {
	inner KVBackend
	key   []byte
}

var _ KVBackend = (*encryptingKVBackend)(nil)

// NewEncryptingKVBackend wraps inner so every value is sealed with AES-256-GCM
// under key (must be exactly 32 bytes) before hitting inner, and opened on
// read. inner should not be shared with a plaintext KVBackend the plugin can
// also address (see the isolation note above) — use a disjoint backend/subdir.
func NewEncryptingKVBackend(inner KVBackend, key []byte) (KVBackend, error) {
	if len(key) != crypto.KeySize {
		return nil, fmt.Errorf("hostcap: secrets key must be %d bytes, got %d", crypto.KeySize, len(key))
	}
	return &encryptingKVBackend{inner: inner, key: key}, nil
}

// errSecretDecryptFailed is returned by Get when the stored ciphertext cannot
// be authenticated/decrypted (truncated, tampered, or sealed under a
// different key) — fail-closed: never return partial or garbage plaintext.
var errSecretDecryptFailed = errors.New("hostcap: secret value failed to decrypt (corrupt or tampered)")

func (b *encryptingKVBackend) Get(pluginID, key string) ([]byte, bool, error) {
	ct, ok, err := b.inner.Get(pluginID, key)
	if err != nil || !ok {
		return nil, ok, err
	}
	pt, err := crypto.Open(b.key, ct)
	if err != nil {
		return nil, false, errSecretDecryptFailed
	}
	return pt, true, nil
}

func (b *encryptingKVBackend) Set(pluginID, key string, value []byte) error {
	ct, err := crypto.Seal(b.key, value)
	if err != nil {
		return err
	}
	return b.inner.Set(pluginID, key, ct)
}

func (b *encryptingKVBackend) Delete(pluginID, key string) error {
	return b.inner.Delete(pluginID, key)
}

// List returns the keys under prefix — values are encrypted, but keys never
// are, so this passes straight through to inner.
func (b *encryptingKVBackend) List(pluginID, prefix string) ([]string, error) {
	return b.inner.List(pluginID, prefix)
}

// LoadOrCreateSecretsKey loads the AES-256 key persisted at
// <dir>/.secrets.key (crypto.SecretsKeyFile — the SAME file
// extension.Manager.SecretsKey resolves for storage/plugin-config
// encryption, G4 §1: one shared key, one backup/restore story), generating
// and atomically persisting a fresh random one (mode 0600) on first use.
// Subsequent calls over the same dir return the same key, so encrypted
// secrets survive a process restart. The key is never logged.
func LoadOrCreateSecretsKey(dir string) ([]byte, error) {
	return crypto.LoadOrCreateKey(dir, crypto.SecretsKeyFile)
}
