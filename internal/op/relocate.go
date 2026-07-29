package op

import (
	"sync"
	"time"
)

// relocateRetryAttempts/relocateRetryBaseDelay bound the backoff applied to a
// relocation that fails transiently (e.g. SQLITE_LOCKED from a reader holding
// the table in a cache=shared sqlite setup: busy_timeout does not cover that
// case, so the caller must retry). 5 attempts with a 10ms doubling backoff caps
// worst-case added latency at ~150ms.
const (
	relocateRetryAttempts  = 5
	relocateRetryBaseDelay = 10 * time.Millisecond
)

// relocateMutexes serializes relocations per-storage to avoid database lock
// contention when multiple renames/moves happen concurrently on the same storage.
var (
	relocateMutexes   = make(map[uint]*sync.Mutex)
	relocateMutexesMu sync.Mutex
)

func getRelocateMutex(storageID uint) *sync.Mutex {
	relocateMutexesMu.Lock()
	defer relocateMutexesMu.Unlock()
	if m, ok := relocateMutexes[storageID]; ok {
		return m
	}
	m := &sync.Mutex{}
	relocateMutexes[storageID] = m
	return m
}

// withRelocateRetry runs fn, retrying on failure with an exponential backoff.
// It absorbs the transient database lock contention that a concurrent
// relocate/read workload can produce; the per-storage mutex above only
// serializes relocations against each other, not against concurrent readers,
// so this retry is still needed on top of it.
func withRelocateRetry(fn func() error) error {
	var err error
	delay := relocateRetryBaseDelay
	for attempt := 0; attempt < relocateRetryAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt < relocateRetryAttempts-1 {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return err
}
