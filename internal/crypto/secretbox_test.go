package crypto

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := LoadOrCreateKey(t.TempDir(), "test.key")
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := testKey(t)
	const plaintext = "super-secret-value"
	ct, err := Seal(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, []byte(plaintext)) {
		t.Fatal("ciphertext contains the plaintext bytes")
	}
	const nonceSize, tagSize = 12, 16
	if len(ct) != nonceSize+len(plaintext)+tagSize {
		t.Fatalf("ciphertext length = %d, want %d (nonce+plaintext+tag)", len(ct), nonceSize+len(plaintext)+tagSize)
	}
	pt, err := Open(key, ct)
	if err != nil || string(pt) != plaintext {
		t.Fatalf("Open = %q,%v; want %q,nil", pt, err, plaintext)
	}
}

func TestOpenTamperedFailsClosed(t *testing.T) {
	key := testKey(t)
	ct, err := Seal(key, []byte("secret-data"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xFF
	pt, err := Open(key, tampered)
	if err == nil {
		t.Fatalf("tampered ciphertext decrypted without error: %q", pt)
	}
	if pt != nil {
		t.Fatalf("failed decrypt must not return partial plaintext, got %q", pt)
	}
	if err != ErrOpenFailed {
		t.Fatalf("error = %v, want ErrOpenFailed", err)
	}
}

func TestOpenWrongKeyFailsClosed(t *testing.T) {
	key1 := testKey(t)
	key2 := testKey(t)
	ct, err := Seal(key1, []byte("secret-data"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(key2, ct); err != ErrOpenFailed {
		t.Fatalf("Open under the wrong key = %v, want ErrOpenFailed", err)
	}
}

func TestOpenTruncatedFailsClosed(t *testing.T) {
	key := testKey(t)
	if _, err := Open(key, []byte("short")); err != ErrOpenFailed {
		t.Fatalf("Open on a too-short blob = %v, want ErrOpenFailed", err)
	}
}

func TestSealRejectsWrongKeySize(t *testing.T) {
	if _, err := Seal([]byte("too-short"), []byte("data")); err == nil {
		t.Fatal("expected error for a non-32-byte key")
	}
}

func TestLoadOrCreateKeyPersistsAndReuses(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateKey(dir, "k.key")
	if err != nil {
		t.Fatalf("first LoadOrCreateKey: %v", err)
	}
	if len(k1) != KeySize {
		t.Fatalf("key length = %d, want %d", len(k1), KeySize)
	}

	keyPath := filepath.Join(dir, "k.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %v, want -rw-------", perm)
	}

	k2, err := LoadOrCreateKey(dir, "k.key")
	if err != nil {
		t.Fatalf("second LoadOrCreateKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("key regenerated on second load over the same dir/filename, want reused")
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

func TestLoadOrCreateKeyRejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k.key"), []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir, "k.key"); err == nil {
		t.Fatal("expected error for wrong-length key file, got nil")
	}
}

// TestLoadOrCreateKeyDistinctFilenamesAreIndependent pins the reason this
// package takes a filename (not a hardcoded one, unlike hostcap's original
// single-purpose secrets.key): storage/plugin-config encryption (G4 §2/§3)
// and hostcap's Secrets KV (G4 §1) need independent key files under the same
// dir without colliding.
func TestLoadOrCreateKeyDistinctFilenamesAreIndependent(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateKey(dir, "a.key")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateKey(dir, "b.key")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("two distinct filenames under the same dir must not yield the same key by coincidence in this test (astronomically unlikely unless something is wrong)")
	}
}
