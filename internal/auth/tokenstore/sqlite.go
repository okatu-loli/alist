package tokenstore

import (
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// SQLiteStore is the default Store, backed by the app's main database
// (sqlite or otherwise — the name reflects the original target, not a hard
// dependency; it goes through internal/db like every other table).
type SQLiteStore struct{}

func NewSQLiteStore() *SQLiteStore {
	return &SQLiteStore{}
}

func (s *SQLiteStore) Save(r Record) error {
	return db.SaveToken(&model.Token{
		JTI:       r.JTI,
		UserID:    r.UserID,
		SessionID: r.SessionID,
		Kind:      r.Kind,
		ExpiresAt: r.ExpiresAt,
		CreatedAt: time.Now().Unix(),
	})
}

// Get is fail-closed: not-found, revoked and expired all return an error so
// callers cannot mistake any of them for a valid token. The error is one of
// the ErrToken* sentinels so callers needing to distinguish why (replay
// detection needs ErrTokenRevoked specifically) can use errors.Is.
func (s *SQLiteStore) Get(jti string) (*Record, error) {
	t, err := db.GetToken(jti)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTokenNotFound
		}
		// A real DB failure, distinct from "jti never existed" — still
		// non-nil so the caller stays fail-closed, but distinguishable in
		// logs and not eligible for the ErrTokenRevoked replay branch.
		return nil, errors.Wrapf(err, "tokenstore: get %s", jti)
	}
	if t.Revoked {
		return nil, ErrTokenRevoked
	}
	if t.ExpiresAt < time.Now().Unix() {
		return nil, ErrTokenExpired
	}
	return &Record{
		JTI:       t.JTI,
		UserID:    t.UserID,
		SessionID: t.SessionID,
		Kind:      t.Kind,
		ExpiresAt: t.ExpiresAt,
	}, nil
}

func (s *SQLiteStore) Revoke(jti string) error {
	return db.RevokeToken(jti)
}

func (s *SQLiteStore) RevokeIfActive(jti string) (bool, error) {
	return db.RevokeTokenIfActive(jti)
}

func (s *SQLiteStore) RevokeBySession(sessionID string) error {
	return db.RevokeTokensBySession(sessionID)
}

func (s *SQLiteStore) RevokeByUser(userID uint) error {
	return db.RevokeTokensByUser(userID)
}

func (s *SQLiteStore) GC() (int, error) {
	return db.GCTokens()
}
