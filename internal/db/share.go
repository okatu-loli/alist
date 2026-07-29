package db

import (
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// CreateShare inserts a new share row (Token/NodeID/CreatorID already set by op).
func CreateShare(s *model.Share) error {
	return errors.WithStack(db.Create(s).Error)
}

// GetShareByToken looks a share up by its bearer token. A missing token maps to
// errs.ShareNotFound so the public layer answers with a single 404 regardless of
// the reason (enumeration defense).
func GetShareByToken(token string) (*model.Share, error) {
	var s model.Share
	err := db.Where("token = ?", token).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.ShareNotFound
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &s, nil
}

// GetShareByID loads a share by primary key — the creator-ownership check for
// delete happens in the op/handler layer.
func GetShareByID(id uint) (*model.Share, error) {
	var s model.Share
	if err := db.First(&s, id).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return &s, nil
}

// ListSharesByCreator returns every share a user created, newest first.
func ListSharesByCreator(creatorID uint) ([]model.Share, error) {
	var ss []model.Share
	if err := db.Where("creator_id = ?", creatorID).Order("id DESC").Find(&ss).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return ss, nil
}

// DeleteShare removes a share (revokes the link) by id.
func DeleteShare(id uint) error {
	return errors.WithStack(db.Delete(&model.Share{}, id).Error)
}
