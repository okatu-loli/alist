package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func GetSession(userID uint, deviceKey string) (*model.Session, error) {
	s := model.Session{UserID: userID, DeviceKey: deviceKey}
	if err := db.Select("user_id, device_key, last_active, status, user_agent, ip").Where(&s).First(&s).Error; err != nil {
		return nil, errors.Wrap(err, "failed find session")
	}
	return &s, nil
}

func CreateSession(s *model.Session) error {
	return errors.WithStack(db.Create(s).Error)
}

func UpsertSession(s *model.Session) error {
	return errors.WithStack(db.Clauses(clause.OnConflict{UpdateAll: true}).Create(s).Error)
}

func DeleteSession(userID uint, deviceKey string) error {
	return errors.WithStack(db.Where("user_id = ? AND device_key = ?", userID, deviceKey).Delete(&model.Session{}).Error)
}

func CountActiveSessionsByUser(userID uint) (int64, error) {
	var count int64
	err := db.Model(&model.Session{}).
		Where("user_id = ? AND status = ?", userID, model.SessionActive).
		Count(&count).Error
	return count, errors.WithStack(err)
}

func DeleteSessionsBefore(ts int64) error {
	return errors.WithStack(db.Where("last_active < ?", ts).Delete(&model.Session{}).Error)
}

// GetOldestActiveSession returns the oldest active session for the specified user.
func GetOldestActiveSession(userID uint) (*model.Session, error) {
	var s model.Session
	if err := db.Where("user_id = ? AND status = ?", userID, model.SessionActive).
		Order("last_active ASC").First(&s).Error; err != nil {
		return nil, errors.Wrap(err, "failed get oldest active session")
	}
	return &s, nil
}

func UpdateSessionLastActive(userID uint, deviceKey string, lastActive int64) error {
	return errors.WithStack(db.Model(&model.Session{}).Where("user_id = ? AND device_key = ?", userID, deviceKey).Update("last_active", lastActive).Error)
}

func ListSessionsByUser(userID uint) ([]model.Session, error) {
	var sessions []model.Session
	err := db.Select("user_id, device_key, last_active, status, user_agent, ip").Where("user_id = ? AND status = ?", userID, model.SessionActive).Find(&sessions).Error
	return sessions, errors.WithStack(err)
}

func ListSessions() ([]model.Session, error) {
	var sessions []model.Session
	err := db.Select("user_id, device_key, last_active, status, user_agent, ip").Where("status = ?", model.SessionActive).Find(&sessions).Error
	return sessions, errors.WithStack(err)
}

func MarkInactive(sessionID string) error {
	return errors.WithStack(db.Model(&model.Session{}).Where("device_key = ?", sessionID).Update("status", model.SessionInactive).Error)
}

// MarkInactiveAndRevokeTokens flips a session to inactive and revokes every
// token issued for it in a single transaction, so a crash or error between
// the two writes can never leave "session inactive but its tokens still
// live" — the failure mode MarkInactive's two independent calls used to
// allow. It writes the tokens table directly rather than going through the
// tokenstore.Store interface: on this skeleton tokens live in this same
// gorm db (see tokenstore.SQLiteStore.RevokeBySession, which does the exact
// same update), so bypassing the interface costs nothing and buys
// atomicity. If a non-db-backed Store (e.g. redis) is ever wired in, this
// function stops being equivalent to "mark + store.RevokeBySession" and the
// caller needs to revisit how the two are kept in sync.
func MarkInactiveAndRevokeTokens(sessionID string) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Session{}).Where("device_key = ?", sessionID).
			Update("status", model.SessionInactive).Error; err != nil {
			return err
		}
		return tx.Model(&model.Token{}).Where("session_id = ?", sessionID).
			Update("revoked", true).Error
	}))
}
