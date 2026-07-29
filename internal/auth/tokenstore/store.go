// Package tokenstore holds the persistent record of every issued access/
// refresh token's revocation state. It is the fail-closed backstop behind
// JWT verification: a structurally valid signature is not enough — the jti
// must also still be known, unrevoked and unexpired here.
package tokenstore

import "errors"

// Record is the store's view of one issued token, independent of
// internal/model's gorm tags.
type Record struct {
	JTI       string
	UserID    uint
	SessionID string
	Kind      string
	ExpiresAt int64
}

// Sentinel errors returned by Get, distinguishing why a jti was rejected.
// Callers that only need "valid or not" can treat any of them the same way
// (reject); callers that need to tell replay (ErrTokenRevoked) apart from a
// merely unknown/expired token — refresh-token replay detection is the
// motivating case — can use errors.Is.
var (
	ErrTokenNotFound = errors.New("token not found")
	ErrTokenRevoked  = errors.New("token revoked")
	ErrTokenExpired  = errors.New("token expired")
)

// Store is the persistence boundary for token revocation state.
type Store interface {
	Save(r Record) error
	// Get returns an error if jti is unknown, revoked, or expired — callers
	// must treat any error as "reject this token", never as "not yet
	// decided". For those three cases the error is one of ErrTokenNotFound,
	// ErrTokenRevoked or ErrTokenExpired (see errors.Is); a genuine
	// underlying-store failure returns a different, wrapped error instead of
	// being folded into ErrTokenNotFound, so callers must not assume the
	// error is always one of the three sentinels.
	Get(jti string) (*Record, error)
	Revoke(jti string) error
	// RevokeIfActive atomically revokes jti only if it is currently
	// unrevoked, returning whether this call won the race. It exists so
	// callers consuming a single-use token (refresh-token rotation) can
	// detect concurrent replay: if two callers present the same jti at
	// once, only one gets true.
	RevokeIfActive(jti string) (bool, error)
	RevokeBySession(sessionID string) error
	RevokeByUser(userID uint) error
	// GC deletes expired/revoked rows and reports how many were removed.
	GC() (removed int, err error)
}

// instance is a single-writer singleton: Init is expected to run exactly
// once, during bootstrap, before any goroutine calls Get. It is not guarded
// for concurrent Init calls after startup.
var instance Store

// Init sets the package-level singleton. Called once during bootstrap.
func Init(s Store) {
	instance = s
}

// Get returns the package singleton. Callers that need fail-closed behavior
// before Init has run are responsible for checking for nil (see Task 2).
func Get() Store {
	return instance
}
