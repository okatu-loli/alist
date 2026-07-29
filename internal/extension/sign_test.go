package extension

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// writePackageDir writes a minimal plugin package (manifest + dummy binary) into
// a fresh dir and returns it. Not yet signed.
func writePackageDir(t *testing.T, m ext.Manifest) string {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("#!/bin/false\ndummy-binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestVerifyGood(t *testing.T) {
	pub, priv := genKey(t)
	dir := writePackageDir(t, validManifest())
	if err := SignDir(dir, priv); err != nil {
		t.Fatalf("SignDir: %v", err)
	}
	if err := NewVerifier(pub).Verify(dir); err != nil {
		t.Fatalf("Verify good package: %v", err)
	}
}

func TestVerifyUnsigned(t *testing.T) {
	pub, _ := genKey(t)
	dir := writePackageDir(t, validManifest())
	err := NewVerifier(pub).Verify(dir)
	if err == nil {
		t.Fatal("expected error verifying unsigned package")
	}
	if _, ok := err.(*SignatureError); !ok {
		t.Fatalf("expected *SignatureError, got %T", err)
	}
}

func TestVerifyWrongKey(t *testing.T) {
	_, priv := genKey(t)
	otherPub, _ := genKey(t)
	dir := writePackageDir(t, validManifest())
	if err := SignDir(dir, priv); err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier(otherPub).Verify(dir); err == nil {
		t.Fatal("expected error: signed by untrusted key")
	}
}

func TestVerifyTamperedBinary(t *testing.T) {
	pub, priv := genKey(t)
	dir := writePackageDir(t, validManifest())
	if err := SignDir(dir, priv); err != nil {
		t.Fatal(err)
	}
	// Alter the binary after signing: checksum no longer matches.
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := NewVerifier(pub).Verify(dir)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if _, ok := err.(*SignatureError); !ok {
		t.Fatalf("expected *SignatureError, got %T", err)
	}
}

func TestVerifyTamperedManifest(t *testing.T) {
	pub, priv := genKey(t)
	dir := writePackageDir(t, validManifest())
	if err := SignDir(dir, priv); err != nil {
		t.Fatal(err)
	}
	// Swap the manifest for a different one; its checksum won't match.
	m := validManifest()
	m.ID = "com.evil.injected"
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier(pub).Verify(dir); err == nil {
		t.Fatal("expected error: tampered manifest")
	}
}

func TestVerifyExtraFile(t *testing.T) {
	pub, priv := genKey(t)
	dir := writePackageDir(t, validManifest())
	if err := SignDir(dir, priv); err != nil {
		t.Fatal(err)
	}
	// Injected unlisted file: ComputeChecksums picks it up, actual != recorded.
	if err := os.WriteFile(filepath.Join(dir, "extra.so"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier(pub).Verify(dir); err == nil {
		t.Fatal("expected error: unlisted extra file")
	}
}
