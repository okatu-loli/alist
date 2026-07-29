package db

import (
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
)

// CreateRecoveryCodes inserts a fresh batch of recovery-code rows (UserID/
// Hash/Salt already set by op). Used once per confirmed TOTP enrollment /
// regeneration.
func CreateRecoveryCodes(codes []model.RecoveryCode) error {
	if len(codes) == 0 {
		return nil
	}
	return errors.WithStack(db.Create(&codes).Error)
}

// ListRecoveryCodesByUser returns every recovery-code row (used and unused)
// belonging to userID.
func ListRecoveryCodesByUser(userID uint) ([]model.RecoveryCode, error) {
	var codes []model.RecoveryCode
	if err := db.Where("user_id = ?", userID).Order("id ASC").Find(&codes).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return codes, nil
}

// MarkRecoveryCodeUsedIfUnused atomically stamps UsedAt on row id, but ONLY if
// it is still unused (the WHERE clause and the write happen as one statement,
// so two concurrent callers racing to consume the SAME row can never both
// succeed). It reports whether THIS call is the one that flipped it — a
// caller that loses the race gets ok=false, exactly as if the code had
// already been used by someone else (red-team I7: the read-then-write gap in
// the old unconditional update let two concurrent requests both "succeed"
// against the same still-unused row).
func MarkRecoveryCodeUsedIfUnused(id uint, t time.Time) (ok bool, err error) {
	res := db.Model(&model.RecoveryCode{}).Where("id = ? AND used_at IS NULL", id).Update("used_at", t)
	if res.Error != nil {
		return false, errors.WithStack(res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DeleteRecoveryCodesByUser removes every recovery-code row for userID (unbind
// / regenerate both start from a clean slate).
func DeleteRecoveryCodesByUser(userID uint) error {
	return errors.WithStack(db.Where("user_id = ?", userID).Delete(&model.RecoveryCode{}).Error)
}
