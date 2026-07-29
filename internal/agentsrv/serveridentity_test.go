package agentsrv

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// TestLoadOrCreateServerIdentityGeneratesValidKeypair pins that a fresh dir
// gets a real ed25519 keypair whose PubKeyBase64 decodes to a right-sized key
// that verifies a signature the private half produced.
func TestLoadOrCreateServerIdentityGeneratesValidKeypair(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateServerIdentity(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateServerIdentity: %v", err)
	}
	pub, err := base64.StdEncoding.DecodeString(id.PubKeyBase64())
	if err != nil {
		t.Fatalf("PubKeyBase64 is not valid base64: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("pubkey size = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	// The generated keypair must actually be a matched pair.
	sig := ed25519.Sign(id.priv, []byte("probe"))
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte("probe"), sig) {
		t.Fatal("generated server keypair does not verify its own signature")
	}
}

// TestLoadOrCreateServerIdentityPersistsAtRestrictivePermissions pins that
// the server identity file lands at 0600 — it holds a private key.
func TestLoadOrCreateServerIdentityPersistsAtRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix file mode bits don't apply on windows")
	}
	dir := t.TempDir()
	if _, err := LoadOrCreateServerIdentity(dir); err != nil {
		t.Fatalf("LoadOrCreateServerIdentity: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, serverIdentityFileName))
	if err != nil {
		t.Fatalf("stat server identity file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("server identity file perm = %o, want 0600", perm)
	}
}

// TestLoadOrCreateServerIdentityReusesExistingFile pins the stable-identity
// contract: a second load returns the SAME public key, never a fresh one — an
// agent that pinned the pubkey must keep verifying across server restarts.
func TestLoadOrCreateServerIdentityReusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateServerIdentity(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateServerIdentity: %v", err)
	}
	second, err := LoadOrCreateServerIdentity(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateServerIdentity: %v", err)
	}
	if first.PubKeyBase64() != second.PubKeyBase64() {
		t.Fatal("second load minted a different public key")
	}
	if string(first.priv) != string(second.priv) {
		t.Fatal("second load minted a different private key")
	}
}

// TestLoadOrCreateServerIdentityMalformedFileFailsClosed pins that a corrupt
// file is a hard error — never silently regenerated, which would rotate the
// server's identity and break every agent that pinned the old public key.
func TestLoadOrCreateServerIdentityMalformedFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, serverIdentityFileName)
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateServerIdentity(dir); err == nil {
		t.Fatal("malformed server identity file must error, not silently regenerate")
	}
	// The corrupt bytes must be left untouched (no silent overwrite).
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "not json" {
		t.Fatalf("corrupt file was overwritten (should fail closed and leave it): %q", string(b))
	}
}

// TestLoadOrCreateServerIdentityWrongLengthKeyFailsClosed pins that a
// well-formed JSON file whose base64 key is the wrong length is rejected too.
func TestLoadOrCreateServerIdentityWrongLengthKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, serverIdentityFileName)
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	body := `{"public_key":"` + short + `","private_key":"` + short + `"}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateServerIdentity(dir); err == nil {
		t.Fatal("wrong-length key must fail closed, not regenerate")
	}
}

// TestLoadOrCreateServerIdentityConcurrentCreateConverges pins that many
// goroutines racing to create the identity in one fresh dir all end up with
// the SAME public key (a single persisted identity, not one-per-goroutine).
func TestLoadOrCreateServerIdentityConcurrentCreateConverges(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	pubs := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, err := LoadOrCreateServerIdentity(dir)
			if err != nil {
				errs[i] = err
				return
			}
			pubs[i] = id.PubKeyBase64()
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if pubs[i] != pubs[0] {
			t.Fatalf("concurrent create diverged: pubs[%d]=%q != pubs[0]=%q", i, pubs[i], pubs[0])
		}
	}
	// And the on-disk file must match what everyone returned.
	reloaded, err := LoadOrCreateServerIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PubKeyBase64() != pubs[0] {
		t.Fatalf("reloaded pubkey %q != converged %q", reloaded.PubKeyBase64(), pubs[0])
	}
}

// TestServerIdentityDISeam pins the injection seam the pairing handler reads
// through: SetServerIdentity installs it, CurrentServerIdentity returns it,
// and a nil clears back to "none wired" (the pairing handler's empty-field
// path).
func TestServerIdentityDISeam(t *testing.T) {
	prev := CurrentServerIdentity()
	t.Cleanup(func() { SetServerIdentity(prev) })

	SetServerIdentity(nil)
	if CurrentServerIdentity() != nil {
		t.Fatal("CurrentServerIdentity must be nil after SetServerIdentity(nil)")
	}
	id, err := LoadOrCreateServerIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	SetServerIdentity(id)
	got := CurrentServerIdentity()
	if got == nil || got.PubKeyBase64() != id.PubKeyBase64() {
		t.Fatalf("CurrentServerIdentity did not return the installed identity")
	}
}
