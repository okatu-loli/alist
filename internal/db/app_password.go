package db

import (
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// CreateAppPassword inserts a new app-password row (UserID/Hash/Salt already
// set by op).
func CreateAppPassword(ap *model.AppPassword) error {
	return errors.WithStack(db.Create(ap).Error)
}

// GetAppPasswordByID loads an app password by primary key — the
// owner-ownership check happens in the op layer (op.DeleteAppPassword).
// A missing id maps to errs.AppPasswordNotFound.
func GetAppPasswordByID(id uint) (*model.AppPassword, error) {
	var ap model.AppPassword
	err := db.First(&ap, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.AppPasswordNotFound
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &ap, nil
}

// ListAppPasswordsByUser returns every app password belonging to userID,
// newest first.
func ListAppPasswordsByUser(userID uint) ([]model.AppPassword, error) {
	var aps []model.AppPassword
	if err := db.Where("user_id = ?", userID).Order("id DESC").Find(&aps).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return aps, nil
}

// CountAppPasswordsByUser returns how many app passwords userID currently
// owns — the per-user cap check reads this before minting a new one.
func CountAppPasswordsByUser(userID uint) (int64, error) {
	var count int64
	if err := db.Model(&model.AppPassword{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		return 0, errors.WithStack(err)
	}
	return count, nil
}

// DeleteAppPasswordByID removes an app password (revokes it) by id.
func DeleteAppPasswordByID(id uint) error {
	return errors.WithStack(db.Delete(&model.AppPassword{}, id).Error)
}

// UpdateAppPasswordLastUsed stamps LastUsed on a successful verify. Callers
// treat this as best-effort — a failure here must never fail the verify
// itself (see op.VerifyAppPassword).
func UpdateAppPasswordLastUsed(id uint, t time.Time) error {
	return errors.WithStack(db.Model(&model.AppPassword{}).Where("id = ?", id).Update("last_used", t).Error)
}
