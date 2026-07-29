package sign

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
)

// validateWiredPurpose rejects purposes whose signer isn't wired up yet.
// Only "download" is wired in Step 6; archive/share keys can exist in the
// table (future steps) but rotating/GC-ing them here would be a no-op at
// best, since Reload only ever rebuilds the download signer.
func validateWiredPurpose(purpose string) error {
	if purpose != model.SignPurposeDownload {
		return fmt.Errorf("signing key rotation is not wired for purpose %q yet (only %q is wired in this release)", purpose, model.SignPurposeDownload)
	}
	return nil
}

// generateKeyMaterial mirrors db.EnsureDownloadSigningKey's random-generation
// approach (hex 16-byte KID, base64 32-byte secret) so rotated keys are
// indistinguishable in shape from the seeded one.
func generateKeyMaterial() (kid string, secret string, err error) {
	kidBytes := make([]byte, 16)
	if _, err = rand.Read(kidBytes); err != nil {
		return "", "", err
	}
	secretBytes := make([]byte, 32)
	if _, err = rand.Read(secretBytes); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(kidBytes), base64.StdEncoding.EncodeToString(secretBytes), nil
}

// Rotate deactivates the current active key for purpose and installs a new
// random active key in its place, in a single transaction: a mid-failure
// must not leave the purpose with zero active keys (deactivate committed,
// create failed) or two active keys (the reverse). The old key is left in
// place (grace) so signatures already handed out keep verifying until an
// admin GCs it.
//
// After the transaction commits, Reload() rebuilds the in-memory signer.
// Reload's error is propagated: the db already shows the new key as active
// at that point, so a failed reload here would otherwise leave Sign/Verify
// silently using the stale signer while callers believe rotation succeeded.
func Rotate(purpose string) (string, error) {
	if err := validateWiredPurpose(purpose); err != nil {
		return "", errors.WithStack(err)
	}
	kid, secret, err := generateKeyMaterial()
	if err != nil {
		return "", errors.WithStack(err)
	}
	newKey := &model.SigningKey{
		Purpose:   purpose,
		KID:       kid,
		Secret:    secret,
		Active:    true,
		CreatedAt: time.Now().Unix(),
	}
	if err := db.RotateSigningKeyTx(purpose, newKey); err != nil {
		return "", err
	}
	if err := Reload(); err != nil {
		return "", errors.Wrapf(err, "rotate: key %q committed to db but signer reload failed to build", newKey.KID)
	}
	return newKey.KID, nil
}

// GC deletes every non-active key of purpose, ending their grace window, then
// reloads the signer. Deletions run in one transaction so a mid-failure
// leaves either all or none of the targeted keys removed.
func GC(purpose string) (int, error) {
	if err := validateWiredPurpose(purpose); err != nil {
		return 0, errors.WithStack(err)
	}
	keys, err := db.GetSigningKeys(purpose)
	if err != nil {
		return 0, err
	}
	var toDelete []string
	for _, k := range keys {
		if !k.Active {
			toDelete = append(toDelete, k.KID)
		}
	}
	if len(toDelete) == 0 {
		return 0, nil
	}
	if err := db.DeleteSigningKeysTx(toDelete); err != nil {
		return 0, err
	}
	if err := Reload(); err != nil {
		return 0, errors.Wrap(err, "gc: keys deleted but signer reload failed to build")
	}
	return len(toDelete), nil
}

// ListKeys returns every key (active and grace) for purpose, newest first
// not guaranteed — callers needing order should sort on CreatedAt. It
// deliberately skips validateWiredPurpose: listing is a harmless read, not a
// mutation, so unwired purposes are still readable.
func ListKeys(purpose string) ([]model.SigningKey, error) {
	return db.GetSigningKeys(purpose)
}
