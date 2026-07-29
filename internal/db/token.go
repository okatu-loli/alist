package db

import (
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
)

func SaveToken(t *model.Token) error { return errors.WithStack(db.Create(t).Error) }

func GetToken(jti string) (*model.Token, error) {
	var t model.Token
	if err := db.Where("jti = ?", jti).First(&t).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return &t, nil
}

func RevokeToken(jti string) error {
	return errors.WithStack(db.Model(&model.Token{}).Where("jti = ?", jti).Update("revoked", true).Error)
}

// RevokeTokenIfActive atomically revokes jti only if it is currently
// unrevoked, via a compare-and-swap UPDATE. Returns true if this call won
// the race and performed the revoke, false if jti was already revoked (or
// doesn't exist) — the caller can then tell "I consumed this token" apart
// from "someone else already did", which a plain Get-then-Revoke cannot do
// under concurrent presentation of the same token.
func RevokeTokenIfActive(jti string) (bool, error) {
	res := db.Model(&model.Token{}).Where("jti = ? AND revoked = ?", jti, false).Update("revoked", true)
	return res.RowsAffected == 1, errors.WithStack(res.Error)
}

func RevokeTokensBySession(sessionID string) error {
	return errors.WithStack(db.Model(&model.Token{}).Where("session_id = ?", sessionID).Update("revoked", true).Error)
}

func RevokeTokensByUser(userID uint) error {
	return errors.WithStack(db.Model(&model.Token{}).Where("user_id = ?", userID).Update("revoked", true).Error)
}

func GCTokens() (int, error) {
	res := db.Where("expires_at < ? OR revoked = ?", time.Now().Unix(), true).Delete(&model.Token{})
	return int(res.RowsAffected), errors.WithStack(res.Error)
}
