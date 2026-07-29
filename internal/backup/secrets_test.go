package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/backup"
	"github.com/alist-org/alist/v3/internal/crypto"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/extension"
)

// tamperManifestHasSecretsKey rewrites archiveBytes' manifest.json to set
// has_secrets_key=true, leaving every other tar member (including the real
// manifest's other fields — encrypted stays false, since this only exercises
// an UNENCRYPTED archive) byte-for-byte unchanged. Used to simulate a
// corrupted/hand-edited manifest, a shape Write() itself never produces.
func tamperManifestHasSecretsKey(t *testing.T, archiveBytes []byte) []byte {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gzr)
	type member struct {
		hdr  *tar.Header
		body []byte
	}
	var members []member
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body: %v", err)
		}
		if hdr.Name == "manifest.json" {
			var m backup.Manifest
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("unmarshal manifest: %v", err)
			}
			m.HasSecretsKey = true
			body, err = json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal tampered manifest: %v", err)
			}
			hdr.Size = int64(len(body))
		}
		members = append(members, member{hdr: hdr, body: body})
	}

	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	for _, m := range members {
		if err := tw.WriteHeader(m.hdr); err != nil {
			t.Fatalf("write header %s: %v", m.hdr.Name, err)
		}
		if _, err := tw.Write(m.body); err != nil {
			t.Fatalf("write body %s: %v", m.hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// G4 §4: the shared secrets key (storage Addition / plugin config_form
// encryption, both resolved via extension.PluginsDir()/crypto.SecretsKeyFile)
// can optionally travel inside a backup archive, so a restore onto a
// different machine (or a wiped data dir) can still decrypt every
// enc:v1:-prefixed value the restored Storages/plugin state carries.

// TestIncludeSecretsKeyWithoutPasswordRejected pins Options.validate(): the
// key is only ever written into the archive's AES-256-GCM-encrypted section,
// so requesting it without a password (which would mean no such section
// exists) must fail loudly at export time, not silently drop the request or
// write the key in plaintext somewhere.
func TestIncludeSecretsKeyWithoutPasswordRejected(t *testing.T) {
	boot(t)
	var buf bytes.Buffer
	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	_, err = backup.Write(&buf, arc, backup.Options{IncludeSecretsKey: true})
	if err == nil {
		t.Fatal("expected IncludeSecretsKey without Password to be rejected")
	}
}

// TestIncludeSecretsKeyRejectsWeakPassword pins Minor 7: carrying the actual
// secrets key inside the archive raises the stakes of brute-forcing the
// archive's password well above a normal keyless export (cracking it then
// hands the attacker the SAME key that protects every enc:v1: value
// elsewhere, not just this archive's own contents) — Options.validate()
// must reject a short password specifically when IncludeSecretsKey is set,
// while continuing to accept the identical short password for a plain
// encrypted export that doesn't carry the key.
func TestIncludeSecretsKeyRejectsWeakPassword(t *testing.T) {
	boot(t)
	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if _, err := backup.Write(&bytes.Buffer{}, arc, backup.Options{Password: "short1", IncludeSecretsKey: true}); err == nil {
		t.Fatal("expected a short password to be rejected when IncludeSecretsKey is set")
	}
	// The identical short password without IncludeSecretsKey is unaffected —
	// this floor only applies to the higher-stakes case.
	if _, err := backup.Write(&bytes.Buffer{}, arc, backup.Options{Password: "short1"}); err != nil {
		t.Fatalf("a short password without IncludeSecretsKey must still be accepted: %v", err)
	}
	if _, err := backup.Write(&bytes.Buffer{}, arc, backup.Options{Password: "long-enough-password", IncludeSecretsKey: true}); err != nil {
		t.Fatalf("a long-enough password with IncludeSecretsKey must be accepted: %v", err)
	}
}

// TestBackupSecretsKeyRoundTripAcrossMachines simulates the exact scenario
// G4 §4 exists for: export (with password) from one "machine" (data dir),
// restore into a DIFFERENT one whose secrets key file does not exist yet (or
// holds an unrelated key) — the restored key file must end up holding
// EXACTLY the source machine's key, so a value already sealed under it (here
// simulated directly via crypto.Seal, standing in for a storage Addition
// field or plugin config value encrypted before the backup was taken)
// decrypts correctly after restore, instead of permanently failing under a
// mismatched key.
func TestBackupSecretsKeyRoundTripAcrossMachines(t *testing.T) {
	boot(t) // "source machine": flags.DataDir -> a fresh temp dir

	sourceKey, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		t.Fatalf("resolve source key: %v", err)
	}
	const secretPlaintext = "s3cr3t-storage-credential"
	sealed, err := crypto.Seal(sourceKey, []byte(secretPlaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var buf bytes.Buffer
	manifest, err := backup.Write(&buf, arc, backup.Options{Password: "hunter2-long-enough", IncludeSecretsKey: true})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !manifest.HasSecretsKey {
		t.Fatal("manifest.has_secrets_key not set when IncludeSecretsKey was requested")
	}

	// "Different machine": point flags.DataDir at a brand-new temp dir with
	// its OWN, different secrets key already present, simulating a fresh
	// install rather than an empty one — the restore must clobber this with
	// the archive's key, not merge/ignore it.
	targetDir := t.TempDir()
	oldDataDir := flags.DataDir
	flags.DataDir = targetDir
	t.Cleanup(func() { flags.DataDir = oldDataDir })
	targetKeyBefore, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		t.Fatalf("seed target key: %v", err)
	}
	if bytes.Equal(targetKeyBefore, sourceKey) {
		t.Fatal("test setup bug: target key coincidentally matches source key")
	}

	restored, err := backup.Read(&buf, "hunter2-long-enough")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if restored.SecretsKey == nil {
		t.Fatal("Read did not decode SecretsKey from an archive with has_secrets_key=true")
	}
	if !bytes.Equal(restored.SecretsKey, sourceKey) {
		t.Fatal("decoded SecretsKey does not match the source machine's key")
	}

	if _, err := backup.Apply(restored, backup.StrategyMerge, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The target's key file must now hold the SOURCE key, atomically and
	// with the same 0600 mode LoadOrCreateKey/WriteKeyFile always use.
	keyPath := filepath.Join(extension.PluginsDir(), crypto.SecretsKeyFile)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat restored key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("restored key file mode = %o, want 0600", perm)
	}
	targetKeyAfter, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		t.Fatalf("read restored key: %v", err)
	}
	if !bytes.Equal(targetKeyAfter, sourceKey) {
		t.Fatal("restore did not overwrite the target's key file with the source's key")
	}

	// Fail-loud sanity check via the OLD (target) key: the archive's sealed
	// value must NOT open under it (proves this isn't a vacuously-equal-key
	// test setup) ...
	if _, err := crypto.Open(targetKeyBefore, sealed); err == nil {
		t.Fatal("test setup bug: sealed value opens under the pre-restore target key")
	}
	// ... and must open under whatever key now lives on disk post-restore —
	// exactly the property a real storage/plugin secret depends on to
	// survive a cross-machine restore.
	opened, err := crypto.Open(targetKeyAfter, sealed)
	if err != nil {
		t.Fatalf("sealed value does not open under the restored key: %v", err)
	}
	if string(opened) != secretPlaintext {
		t.Fatalf("opened = %q, want %q", opened, secretPlaintext)
	}
}

// TestRestoreWithoutSecretsKeyLeavesExistingKeyAlone confirms a restore from
// an archive that did NOT carry a key (Options{} / IncludeSecretsKey=false,
// the default and the common case) never touches whatever secrets key file
// is already on disk — G4 §4 is opt-in, and its absence must be a true
// no-op on the key, not e.g. an accidental key deletion or an all-zero
// overwrite.
func TestRestoreWithoutSecretsKeyLeavesExistingKeyAlone(t *testing.T) {
	boot(t)
	existingKey, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}

	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var buf bytes.Buffer
	manifest, err := backup.Write(&buf, arc, backup.Options{})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if manifest.HasSecretsKey {
		t.Fatal("manifest.has_secrets_key set without IncludeSecretsKey")
	}

	restored, err := backup.Read(&buf, "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if restored.SecretsKey != nil {
		t.Fatal("Read populated SecretsKey from an archive that never carried one")
	}
	if _, err := backup.Apply(restored, backup.StrategyMerge, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	keyAfter, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		t.Fatalf("read key after restore: %v", err)
	}
	if !bytes.Equal(keyAfter, existingKey) {
		t.Fatal("restore without a carried key changed the existing key file")
	}
}

// TestUnencryptedArchiveHasSecretsKeyManuallySetIsRejected pins Read's
// defense against a hand-edited/corrupt manifest claiming has_secrets_key on
// an archive that Export itself would never produce that way (unencrypted).
func TestUnencryptedArchiveHasSecretsKeyManuallySetIsRejected(t *testing.T) {
	boot(t)
	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var buf bytes.Buffer
	if _, err := backup.Write(&buf, arc, backup.Options{}); err != nil {
		t.Fatalf("write: %v", err)
	}
	tampered := tamperManifestHasSecretsKey(t, buf.Bytes())
	if _, err := backup.Read(bytes.NewReader(tampered), ""); err == nil {
		t.Fatal("expected has_secrets_key on an unencrypted archive to be rejected")
	}
}

// TestPreRestoreSnapshotSecretsKeyPreserved pins I2: Apply's pre-restore
// safety-net snapshot is exported via Options{} (unencrypted, no key), so it
// never carries a key of its own — if the incoming restore ALSO replaces
// the live secrets key file (archive.SecretsKey != nil), the snapshot's own
// Storages/plugin state (still encrypted under whatever key was live AT
// SNAPSHOT TIME) would become permanently undecryptable the moment that key
// file is overwritten, defeating the "complete rollback point" the snapshot
// exists for. Confirms the pre-restore key gets copied to
// Dir()/pre-restore-<ts>.secrets.key with the SAME ts as the tar.gz
// snapshot, holding exactly the key that was live before the restore (not
// the incoming one).
func TestPreRestoreSnapshotSecretsKeyPreserved(t *testing.T) {
	boot(t)

	// Build an archive under WHATEVER key is live right now (call it "key
	// A") — Write always carries the currently-installed key via
	// resolveSecretsKey, so there's no other way to make it carry a key
	// distinct from "whatever's on disk when Write runs".
	arc, err := backup.Collect(db.GetDb())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var buf bytes.Buffer
	if _, err := backup.Write(&buf, arc, backup.Options{Password: "hunter2-long-enough", IncludeSecretsKey: true}); err != nil {
		t.Fatalf("write: %v", err)
	}
	restored, err := backup.Read(&buf, "hunter2-long-enough")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Now simulate "this machine has since moved on to its own key B" (e.g.
	// key rotation, or simply a different instance's key) — the key that is
	// live RIGHT BEFORE Apply runs is the one that must be preserved,
	// regardless of what the archive carries.
	preRestoreKey := make([]byte, crypto.KeySize)
	if _, err := rand.Read(preRestoreKey); err != nil {
		t.Fatal(err)
	}
	if err := crypto.WriteKeyFile(extension.PluginsDir(), crypto.SecretsKeyFile, preRestoreKey); err != nil {
		t.Fatalf("seed pre-restore key: %v", err)
	}
	if bytes.Equal(restored.SecretsKey, preRestoreKey) {
		t.Fatal("test setup bug: archive key coincidentally matches the pre-restore key")
	}

	if _, err := backup.Apply(restored, backup.StrategyMerge, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	entries, err := os.ReadDir(backup.Dir())
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	var snapTar, snapKey string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "pre-restore-") && strings.HasSuffix(name, ".tar.gz") {
			snapTar = name
		}
		if strings.HasPrefix(name, "pre-restore-") && strings.HasSuffix(name, ".secrets.key") {
			snapKey = name
		}
	}
	if snapTar == "" {
		t.Fatal("pre-restore tar.gz snapshot not found")
	}
	if snapKey == "" {
		t.Fatal("pre-restore secrets key copy not found")
	}
	tarTS := strings.TrimSuffix(strings.TrimPrefix(snapTar, "pre-restore-"), ".tar.gz")
	keyTS := strings.TrimSuffix(strings.TrimPrefix(snapKey, "pre-restore-"), ".secrets.key")
	if tarTS != keyTS {
		t.Fatalf("snapshot tar (%s) and key copy (%s) timestamps do not match", snapTar, snapKey)
	}

	preservedKey, err := os.ReadFile(filepath.Join(backup.Dir(), snapKey))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(preservedKey, preRestoreKey) {
		t.Fatal("preserved key does not match the key that was live before the restore")
	}
	if info, err := os.Stat(filepath.Join(backup.Dir(), snapKey)); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("preserved key file mode = %o, want 0600", perm)
	}

	// And the live key on disk now is the INCOMING one, not the preserved one.
	liveKey, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(liveKey, restored.SecretsKey) {
		t.Fatal("live key after restore does not match the archive's carried key")
	}
}
