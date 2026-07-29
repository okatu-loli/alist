package migration

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the signing_keys table introduced by this migration.
// No explicit TableName() (see 0002_rbac.go): the identifier itself is named
// so default pluralization lands on "signing_keys". Tags must byte-match
// model.SigningKey's — the drift guard AutoMigrates the live model and diffs
// the resulting schema against this frozen snapshot.
type signingKey struct {
	ID        uint   `gorm:"primaryKey"`
	Purpose   string `gorm:"index:idx_signing_key_purpose"`
	KID       string `gorm:"column:kid;uniqueIndex;size:32"`
	Secret    string
	Active    bool
	CreatedAt int64
}

func init() {
	register(&gormigrate.Migration{
		ID: "0003_signing_keys",
		Migrate: func(tx *gorm.DB) error {
			if err := tx.AutoMigrate(&signingKey{}); err != nil {
				return err
			}
			return seedDownloadSigningKey(tx)
		},
		Rollback: func(tx *gorm.DB) error {
			return tx.Migrator().DropTable(&signingKey{})
		},
	})
}

// seedDownloadSigningKey ensures a "download" purpose key exists. Idempotent:
// skipped if an active download key is already present.
func seedDownloadSigningKey(tx *gorm.DB) error {
	var cnt int64
	if err := tx.Model(&signingKey{}).Where("purpose = ? AND active = ?", "download", true).Count(&cnt).Error; err != nil {
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
	k := signingKey{
		Purpose:   "download",
		KID:       hex.EncodeToString(kidBytes),
		Secret:    base64.StdEncoding.EncodeToString(secretBytes),
		Active:    true,
		CreatedAt: time.Now().Unix(),
	}
	return tx.Create(&k).Error
}
