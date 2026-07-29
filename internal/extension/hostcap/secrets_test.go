package hostcap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// newSecretsBackend builds an encrypting DiskKVBackend rooted at
// <dir>/secrets, keyed by a freshly loaded/created secrets key at dir. It also
// returns the plaintext DiskKVBackend over the same subdir so a test can peek
// at (or tamper with) what actually landed on disk.
func newSecretsBackend(t *testing.T, dir string) (KVBackend, *DiskKVBackend) {
	t.Helper()
	key, err := LoadOrCreateSecretsKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateSecretsKey: %v", err)
	}
	disk := NewDiskKVBackend(filepath.Join(dir, "secrets"))
	enc, err := NewEncryptingKVBackend(disk, key)
	if err != nil {
		t.Fatalf("NewEncryptingKVBackend: %v", err)
	}
	return enc, disk
}

// TestSecretsRoundTripEncryptsOnDisk: Set then Get returns the original
// plaintext, but the bytes actually persisted to disk are ciphertext, not the
// plaintext (AES-256-GCM: nonce(12) + sealed(len(plaintext)) + tag(16)).
func TestSecretsRoundTripEncryptsOnDisk(t *testing.T) {
	dir := t.TempDir()
	enc, disk := newSecretsBackend(t, dir)
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{SecretsBackend: enc})
	ctx := context.Background()

	const plaintext = "super-secret-oauth-token"
	if err := h.Store().Secrets().Set(ctx, "token", []byte(plaintext)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := h.Store().Secrets().Get(ctx, "token")
	if err != nil || !ok || string(v) != plaintext {
		t.Fatalf("Get = %q,%v,%v; want %q,true,nil", v, ok, err, plaintext)
	}

	// Peek at the raw ciphertext through a plaintext DiskKVBackend over the
	// same subdir/file — it must not equal (or contain) the plaintext, and its
	// length must match the nonce+ciphertext+tag construction exactly.
	raw, ok, err := disk.Get("p1", "token")
	if err != nil || !ok {
		t.Fatalf("disk.Get raw = %v,%v,%v", raw, ok, err)
	}
	if string(raw) == plaintext {
		t.Fatal("secret stored in plaintext on disk")
	}
	if bytes.Contains(raw, []byte(plaintext)) {
		t.Fatal("on-disk ciphertext contains the plaintext bytes")
	}
	const nonceSize, tagSize = 12, 16
	if len(raw) != nonceSize+len(plaintext)+tagSize {
		t.Fatalf("on-disk ciphertext length = %d, want %d (nonce+plaintext+tag)", len(raw), nonceSize+len(plaintext)+tagSize)
	}
}

// TestSecretsDisjointFromPlaintextKV: a value written through Secrets() is
// invisible via KV().Get on the same key, and vice versa — the two are backed
// by disjoint storage extents, not a naming convention.
func TestSecretsDisjointFromPlaintextKV(t *testing.T) {
	dir := t.TempDir()
	enc, _ := newSecretsBackend(t, dir)
	kv := NewDiskKVBackend(dir) // plaintext leg, disjoint subdir from "secrets"
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{KVBackend: kv, SecretsBackend: enc})
	ctx := context.Background()

	if err := h.Store().Secrets().Set(ctx, "shared-key", []byte("secret-value")); err != nil {
		t.Fatalf("Secrets Set: %v", err)
	}
	if _, ok, err := h.Store().KV().Get(ctx, "shared-key"); err != nil || ok {
		t.Fatalf("KV().Get on a secrets-only key: ok=%v err=%v, want ok=false", ok, err)
	}

	if err := h.Store().KV().Set(ctx, "other-key", []byte("plain-value")); err != nil {
		t.Fatalf("KV Set: %v", err)
	}
	if _, ok, err := h.Store().Secrets().Get(ctx, "other-key"); err != nil || ok {
		t.Fatalf("Secrets().Get on a KV-only key: ok=%v err=%v, want ok=false", ok, err)
	}
}

// TestSecretsPerPluginIsolation: two plugin ids sharing one Secrets backend
// cannot read each other's secrets (same mechanism as plaintext KV).
func TestSecretsPerPluginIsolation(t *testing.T) {
	dir := t.TempDir()
	enc, _ := newSecretsBackend(t, dir)
	hA := New(Grant{PluginID: "plug.a", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{SecretsBackend: enc})
	hB := New(Grant{PluginID: "plug.b", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{SecretsBackend: enc})
	ctx := context.Background()

	if err := hA.Store().Secrets().Set(ctx, "k", []byte("A-secret")); err != nil {
		t.Fatalf("A Set: %v", err)
	}
	if _, ok, err := hB.Store().Secrets().Get(ctx, "k"); err != nil || ok {
		t.Fatalf("B Get A's key: ok=%v err=%v, want ok=false (isolation)", ok, err)
	}
	if v, ok, err := hA.Store().Secrets().Get(ctx, "k"); err != nil || !ok || string(v) != "A-secret" {
		t.Fatalf("A Get its own key = %q,%v,%v; want A-secret,true,nil", v, ok, err)
	}
}

// TestSecretsGrantGate: a plugin without CapStore is denied on every Secrets
// method — the same gate as the plaintext KV (design decision: Secrets reuses
// CapStore rather than a new capability word).
func TestSecretsGrantGate(t *testing.T) {
	dir := t.TempDir()
	enc, _ := newSecretsBackend(t, dir)
	h := New(Grant{PluginID: "p.nostore"}, Options{SecretsBackend: enc}) // no CapStore
	ctx := context.Background()

	if _, _, err := h.Store().Secrets().Get(ctx, "k"); err != ext.ErrPermissionDenied {
		t.Fatalf("Get without CapStore = %v, want ErrPermissionDenied", err)
	}
	if err := h.Store().Secrets().Set(ctx, "k", []byte("v")); err != ext.ErrPermissionDenied {
		t.Fatalf("Set without CapStore = %v, want ErrPermissionDenied", err)
	}
	if err := h.Store().Secrets().Delete(ctx, "k"); err != ext.ErrPermissionDenied {
		t.Fatalf("Delete without CapStore = %v, want ErrPermissionDenied", err)
	}
	if _, err := h.Store().Secrets().List(ctx, ""); err != ext.ErrPermissionDenied {
		t.Fatalf("List without CapStore = %v, want ErrPermissionDenied", err)
	}
}

// TestSecretsTamperFailsClosed: corrupting one byte of the stored ciphertext
// makes Get fail (GCM authentication fails), never return garbage/partial
// plaintext.
func TestSecretsTamperFailsClosed(t *testing.T) {
	dir := t.TempDir()
	enc, disk := newSecretsBackend(t, dir)
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{SecretsBackend: enc})

	if err := h.Store().Secrets().Set(ctx, "k", []byte("secret-data")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, ok, err := disk.Get("p1", "k")
	if err != nil || !ok {
		t.Fatalf("disk.Get = %v,%v,%v", raw, ok, err)
	}
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-1] ^= 0xFF // flip a tag byte
	if err := disk.Set("p1", "k", tampered); err != nil {
		t.Fatalf("disk.Set tampered: %v", err)
	}

	v, ok, err := h.Store().Secrets().Get(ctx, "k")
	if err == nil {
		t.Fatalf("tampered ciphertext decrypted without error: v=%q ok=%v", v, ok)
	}
	if v != nil {
		t.Fatalf("failed decrypt must not return partial plaintext, got %q", v)
	}
}

// TestSecretsListReturnsKeysNotValues: List enumerates key names only — the
// []string return type structurally cannot carry decrypted values, but this
// also pins that the keys themselves come back correctly.
func TestSecretsListReturnsKeysNotValues(t *testing.T) {
	dir := t.TempDir()
	enc, _ := newSecretsBackend(t, dir)
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{SecretsBackend: enc})
	ctx := context.Background()

	if err := h.Store().Secrets().Set(ctx, "a", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := h.Store().Secrets().Set(ctx, "b", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	keys, err := h.Store().Secrets().List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"a", "b"}) {
		t.Fatalf("List = %v, want [a b]", keys)
	}
}

// TestLoadOrCreateSecretsKeyPersistsAndReuses: the key file is created 0600 on
// first use and reused (not regenerated) by a second call over the same dir —
// mirroring a process restart reading back the same key.
func TestLoadOrCreateSecretsKeyPersistsAndReuses(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateSecretsKey(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateSecretsKey: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("key length = %d, want 32", len(k1))
	}

	keyPath := filepath.Join(dir, ".secrets.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %v, want -rw-------", perm)
	}

	k2, err := LoadOrCreateSecretsKey(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateSecretsKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("key regenerated on second load over the same dir, want reused/persisted")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover tmp file after atomic write: %s", e.Name())
		}
	}
}

// TestLoadOrCreateSecretsKeyRejectsWrongLength: a corrupt/foreign key file
// (wrong length) is a hard error, not silently accepted or regenerated —
// silently regenerating would make every existing secret permanently
// undecryptable without ever telling the operator why.
func TestLoadOrCreateSecretsKeyRejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".secrets.key"), []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSecretsKey(dir); err == nil {
		t.Fatal("expected error for wrong-length key file, got nil")
	}
}

// TestNewEncryptingKVBackendRejectsWrongKeySize guards the constructor itself
// (defence in depth beyond the key-file loader's own length check).
func TestNewEncryptingKVBackendRejectsWrongKeySize(t *testing.T) {
	if _, err := NewEncryptingKVBackend(NewMemKVBackend(), []byte("short")); err == nil {
		t.Fatal("expected error for a non-32-byte key")
	}
}
