package op

import "github.com/alist-org/alist/v3/internal/db"

func init() {
	// See db.RegisterOnHandleChanged: every op-layer read cache below is only
	// valid for whichever db handle was open when it was populated, so it must
	// be dropped the instant db.Init repoints the process at a (possibly
	// different) handle. This is the production-correct home for the reset —
	// not just a test convenience — because db.Init is the actual choke point
	// where "the data this process reads from changed", and that happens
	// outside of tests too: `alist migrate v4`'s initTarget() calls
	// bootstrap.InitDB() -> db.Init() a second time in-process when it opens
	// the freshly created target, so any in-process sequence that migrates and
	// then boots (data.InitData()) against the result would otherwise keep
	// reading roles/users/settings cached from whatever database was open
	// before.
	db.RegisterOnHandleChanged(resetProcessCaches)
}

// resetProcessCaches empties every op-layer in-memory read cache.
func resetProcessCaches() {
	listCache.Clear()
	linkCache.Clear()
	InvalidateAllNodePathCache()
	invalidateMetaIndex()
	rolePermCache.Clear()
	roleCache.Clear()
	settingCache.Clear()
	settingGroupCache.Clear()
	userCache.Clear()
}

// ResetCachesForTest empties the same caches on demand. db.Init's automatic
// reset (see init() above) only fires when the db handle itself changes; a
// test that instead mutates rows directly in the database — bypassing the op
// layer, without swapping the handle (e.g. forcing a column back to its
// pre-self-heal value to re-exercise initRoles' self-heal path) — still needs
// to bust the cache by hand, since nothing else would tell it the underlying
// data moved.
func ResetCachesForTest() {
	resetProcessCaches()
}
