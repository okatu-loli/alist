package db

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/alist-org/alist/v3/internal/model"
)

// EnsureDownloadSigningKey guarantees an active "download" purpose signing
// key exists. It is the LegacyAutoMigrate-path counterpart to migration
// 0003's seedDownloadSigningKey: intentionally duplicated rather than
// shared, because the migration package is pinned to a frozen local type
// (see 0003_signing_keys.go) while this operates on the live model.SigningKey
// — the same migration/runtime split already used for the meta anchor
// dual-write. Idempotent: a no-op once any active download key exists.
func EnsureDownloadSigningKey() error {
	var cnt int64
	if err := db.Model(&model.SigningKey{}).
		Where("purpose = ? AND active = ?", model.SignPurposeDownload, true).
		Count(&cnt).Error; err != nil {
		return err
	}
	if cnt > 0 {
		return nil
	}
	kidBytes := make([]byte, 16)
	if _, err := rand.Read(kidBytes); err != nil {
		return err
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return err
	}
	k := &model.SigningKey{
		Purpose:   model.SignPurposeDownload,
		KID:       hex.EncodeToString(kidBytes),
		Secret:    base64.StdEncoding.EncodeToString(secretBytes),
		Active:    true,
		CreatedAt: time.Now().Unix(),
	}
	return CreateSigningKey(k)
}
