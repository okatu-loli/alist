package session

import (
	"github.com/alist-org/alist/v3/internal/db"
	log "github.com/sirupsen/logrus"
)

// MarkInactive marks the session with the given ID as inactive and revokes
// every access/refresh token issued for it, in one transaction, so a kicked
// session dies immediately in tokenstore-backed validation
// (server/common.ParseToken) instead of merely being caught the next time
// HandleSession runs — and so a failure between "mark" and "revoke" can
// never leave the session inactive with its tokens still live.
//
// This is the single entrypoint every kick path (evict handlers, evict_oldest
// device policy) must go through — calling db.MarkInactive directly would
// flip the session's status without killing its already-issued tokens.
//
// It goes straight through db.MarkInactiveAndRevokeTokens instead of
// tokenstore.Get(): tokens live in the same gorm db as sessions (see
// tokenstore.SQLiteStore.RevokeBySession, which performs the identical
// update), so this call no longer depends on tokenstore.Init having run —
// it works even in the early-bootstrap window that used to force the
// mark-only, no-revoke degradation this function had before. If a future
// non-db Store (e.g. redis) is wired in, db.MarkInactiveAndRevokeTokens stops
// being equivalent to "mark + store.RevokeBySession" and this needs
// revisiting (see that function's doc comment).
func MarkInactive(sessionID string) error {
	if err := db.MarkInactiveAndRevokeTokens(sessionID); err != nil {
		log.Errorf("session %s atomic mark-inactive-and-revoke failed: %+v", sessionID, err)
		return err
	}
	return nil
}
