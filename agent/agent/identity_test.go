package agent

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/AlistGo/alist-agent/proto"
)

// TestLoadOrCreateIdentityGeneratesValidKeypair pins that a fresh directory
// gets a real ed25519 keypair of the right sizes.
func TestLoadOrCreateIdentityGeneratesValidKeypair(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	if len(id.PubKey) != ed25519.PublicKeySize {
		t.Fatalf("pubkey size = %d, want %d", len(id.PubKey), ed25519.PublicKeySize)
	}
	if len(id.PrivKey) != ed25519.PrivateKeySize {
		t.Fatalf("privkey size = %d, want %d", len(id.PrivKey), ed25519.PrivateKeySize)
	}
	// The generated keypair must actually be a matched ed25519 pair: signing
	// with PrivKey and verifying with PubKey must succeed.
	sig := id.Sign([]byte("probe"))
	if !ed25519.Verify(id.PubKey, []byte("probe"), sig) {
		t.Fatal("generated keypair does not verify its own signature")
	}
}

// TestLoadOrCreateIdentityPersistsAtRestrictivePermissions pins that the
// identity file lands on disk at 0600 (owner read/write only) — private
// key material must never be group/world-readable.
func TestLoadOrCreateIdentityPersistsAtRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix file mode bits don't apply on windows")
	}
	dir := t.TempDir()
	if _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	path := filepath.Join(dir, identityFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat identity file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("identity file perm = %o, want 0600", perm)
	}
}

// TestLoadOrCreateIdentityReusesExistingFile pins the stable-identity
// contract: calling LoadOrCreateIdentity a second time against the same dir
// must return the SAME keypair (and thus the same AgentID), not mint a new
// one — an agent's identity must survive process restarts.
func TestLoadOrCreateIdentityReusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateIdentity: %v", err)
	}
	second, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateIdentity: %v", err)
	}
	if string(first.PubKey) != string(second.PubKey) {
		t.Fatal("second load minted a different public key")
	}
	if string(first.PrivKey) != string(second.PrivKey) {
		t.Fatal("second load minted a different private key")
	}
	if first.AgentID() != second.AgentID() {
		t.Fatalf("AgentID not stable across reload: %q vs %q", first.AgentID(), second.AgentID())
	}
}

// TestLoadOrCreateIdentityMalformedFileErrors pins that a corrupt identity
// file is a hard error, never silently papered over with a freshly
// generated (and therefore DIFFERENT, breaking the stable-AgentID contract)
// keypair.
func TestLoadOrCreateIdentityMalformedFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, identityFileName)
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Fatal("malformed identity file must error, not silently regenerate")
	}
}

// TestIdentityAgentIDMatchesFingerprint pins that Identity.AgentID is not a
// second, independent derivation — it must equal proto.FingerprintPubKey(PubKey)
// exactly, since the server derives an agent's AgentID the same way from
// the pubkey it receives at pairing.
func TestIdentityAgentIDMatchesFingerprint(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.AgentID() != proto.FingerprintPubKey(id.PubKey) {
		t.Fatalf("AgentID() = %q, want proto.FingerprintPubKey(PubKey) = %q", id.AgentID(), proto.FingerprintPubKey(id.PubKey))
	}
}
