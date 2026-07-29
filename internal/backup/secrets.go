package backup

import (
	"os"
	"path/filepath"

	"github.com/alist-org/alist/v3/internal/crypto"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/pkg/errors"
)

// secretsKeyTarName is the tar member the secrets key travels under when
// Options.IncludeSecretsKey is set — deliberately NOT domain+".json" shaped
// (see importDomainOrder) so it can never collide with, or be mistaken for,
// one of the DB-row domains: it isn't one, it's raw key material.
const secretsKeyTarName = "secrets.key"

// resolveSecretsKey reads the SAME shared secrets key file that
// internal/op's storage Addition encryption and internal/extension's plugin
// config_form encryption resolve to (extension.PluginsDir()/crypto.SecretsKeyFile
// — see internal/op/storage_crypto.go's storageSecretsKey, which this
// mirrors for the same reason: only the pure path helper is needed here, not
// the full plugin Manager singleton).
func resolveSecretsKey() ([]byte, error) {
	key, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		return nil, errors.Wrap(err, "backup: resolving secrets key")
	}
	return key, nil
}

// restoreSecretsKey writes key back to the shared secrets key file (0600,
// atomic), so every storage Addition / plugin config value already
// encrypted under it (elsewhere in this archive, or already present in the
// target instance's DB from before this restore) keeps decrypting
// correctly. Called by Apply only when the archive actually carried a key
// (Archive.SecretsKey != nil).
func restoreSecretsKey(key []byte) error {
	if err := crypto.WriteKeyFile(extension.PluginsDir(), crypto.SecretsKeyFile, key); err != nil {
		return errors.Wrap(err, "backup: writing restored secrets key")
	}
	return nil
}

// preserveSecretsKeyForSnapshot copies the CURRENT on-disk secrets key to
// Dir()/pre-restore-<ts>.secrets.key (0600) — must run BEFORE
// restoreSecretsKey overwrites the live key file with an archive-carried
// one. ts is the exact same timestamp Apply's pre-restore-<ts>.tar.gz
// snapshot uses, so the two files pair up.
//
// [I2, G4 red team] Without this, that snapshot — taken via Options{}, which
// never carries a key — is unusable as the "complete rollback point" its own
// doc comment promises: its Storages/plugin state are still encrypted under
// whatever key was live AT SNAPSHOT TIME, and once that key file is replaced
// by the restore that follows, every enc:v1: value in the snapshot becomes
// permanently undecryptable — restoring FROM the safety net would itself
// need a key nothing preserved.
//
// A missing key file (nothing has ever been encrypted on this instance, so
// LoadOrCreateKey was never called) is not an error: there is nothing to
// preserve, and one will be minted fresh on first use same as always.
func preserveSecretsKeyForSnapshot(ts string) error {
	current, err := os.ReadFile(filepath.Join(extension.PluginsDir(), crypto.SecretsKeyFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "backup: reading current secrets key")
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return errors.Wrap(err, "backup: creating backup dir")
	}
	if err := crypto.WriteKeyFile(Dir(), "pre-restore-"+ts+".secrets.key", current); err != nil {
		return errors.Wrap(err, "backup: writing pre-restore secrets key copy")
	}
	return nil
}
