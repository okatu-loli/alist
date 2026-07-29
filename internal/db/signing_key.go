package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

func GetSigningKeys(purpose string) ([]model.SigningKey, error) {
	var ks []model.SigningKey
	err := db.Where("purpose = ?", purpose).Find(&ks).Error
	return ks, errors.WithStack(err)
}

func GetActiveSigningKey(purpose string) (*model.SigningKey, error) {
	var k model.SigningKey
	if err := db.Where("purpose = ? AND active = ?", purpose, true).First(&k).Error; err != nil {
		return nil, errors.Wrapf(err, "no active signing key for %s", purpose)
	}
	return &k, nil
}

func CreateSigningKey(k *model.SigningKey) error {
	return errors.WithStack(db.Create(k).Error)
}

func DeactivateSigningKeys(purpose string) error {
	return errors.WithStack(db.Model(&model.SigningKey{}).
		Where("purpose = ? AND active = ?", purpose, true).Update("active", false).Error)
}

func DeleteSigningKey(kid string) error {
	return errors.WithStack(db.Where("kid = ?", kid).Delete(&model.SigningKey{}).Error)
}

// RotateSigningKeyTx deactivates every active key of newKey.Purpose and
// creates newKey in its place, atomically: a mid-failure must not leave the
// purpose with zero active keys (deactivate committed, create failed) or two
// active keys (the reverse). newKey must already carry Purpose/KID/Secret/
// Active=true/CreatedAt set by the caller.
func RotateSigningKeyTx(purpose string, newKey *model.SigningKey) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.SigningKey{}).
			Where("purpose = ? AND active = ?", purpose, true).
			Update("active", false).Error; err != nil {
			return err
		}
		return tx.Create(newKey).Error
	}))
}

// DeleteSigningKeysTx removes the given kids atomically (no-op if empty).
func DeleteSigningKeysTx(kids []string) error {
	if len(kids) == 0 {
		return nil
	}
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		for _, kid := range kids {
			if err := tx.Where("kid = ?", kid).Delete(&model.SigningKey{}).Error; err != nil {
				return err
			}
		}
		return nil
	}))
}
