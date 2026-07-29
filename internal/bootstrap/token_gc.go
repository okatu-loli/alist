package bootstrap

import (
	"time"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	log "github.com/sirupsen/logrus"
)

// tokenGCInterval is how often revoked/expired rows are purged from the
// tokens table. The table is small and the query is a simple indexed
// delete, so an hourly sweep comfortably keeps it from growing unbounded
// without adding meaningful load.
const tokenGCInterval = time.Hour

// StartTokenGC launches a background goroutine that periodically purges
// revoked/expired rows from the tokens table (see tokenstore.Store.GC).
// Without this, GCTokens/Store.GC had no caller at all: the tokens table
// grew without bound, since revoked rows were kept forever.
//
// Note: GC deletes revoked rows, which erases replay evidence. A refresh
// token replayed after its row has been garbage-collected returns
// ErrTokenNotFound instead of ErrTokenRevoked and so skips the session
// revoke that replay detection normally triggers. This is an accepted
// trade-off — the session's other tokens still age out on their own
// expiry — not an oversight.
func StartTokenGC() {
	go func() {
		ticker := time.NewTicker(tokenGCInterval)
		defer ticker.Stop()
		for range ticker.C {
			runTokenGC()
		}
	}()
}

// runTokenGC performs a single GC pass. Split out from StartTokenGC's loop
// so it can be exercised directly by tests without waiting on the ticker.
func runTokenGC() {
	store := tokenstore.Get()
	if store == nil {
		return
	}
	removed, err := store.GC()
	if err != nil {
		log.Errorf("token gc failed: %+v", err)
		return
	}
	log.Debugf("token gc removed %d rows", removed)
}
