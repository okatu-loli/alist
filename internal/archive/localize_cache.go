package archive

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// localizeCacheTTL / localizeCacheMaxTotalBytes are vars (not consts) so
// tests can shrink them without needing a multi-GiB fixture or a real
// 10-minute wait.
var (
	// localizeCacheTTL bounds how long an out-of-process (gRPC) ArchiveTool
	// dispatch's localized temp file is kept around after its last use before
	// it becomes eligible for cleanup -- browsing a large archive is Meta, then
	// several List calls, then an Extract, each its own HTTP request; without
	// this a caller re-opening the same archive a few seconds later would
	// re-download it whole all over again.
	localizeCacheTTL = 10 * time.Minute
	// localizeCacheMaxTotalBytes bounds the sum of every currently-cached
	// localized file's size; once exceeded, the least-recently-used
	// unreferenced entries are evicted (their temp files removed) until back
	// under budget.
	localizeCacheMaxTotalBytes int64 = 8 << 30 // 8 GiB
)

// ErrArchiveLocalizeTooLarge is returned when an out-of-process (gRPC)
// ArchiveTool would need to localize an archive larger than
// maxArchiveLocalizeBytes to a local temp file -- refused up front, before a
// single byte is downloaded, so a single browse request against an
// oversized remote archive can't fill the local disk.
var ErrArchiveLocalizeTooLarge = errors.New("archive: source file is too large to localize for an out-of-process archive tool")

// maxArchiveLocalizeBytes bounds how large a remote archive's OWN file size
// (not its declared uncompressed contents -- that's maxZipTotalUncompressedSize)
// may be before resolveArchiveSource refuses to localize it for a gRPC tool
// at all. Var (not const) so tests can lower it without a multi-GiB fixture.
var maxArchiveLocalizeBytes int64 = 4 << 30 // 4 GiB

// localizeCacheKey identifies one archive's content for localization-cache
// purposes: the virtual path (already mount-qualified, so it uniquely
// identifies the storage+path pair without a separate storage id field),
// its declared size, and its modtime -- the same "source signature" a caller
// re-opening the SAME unchanged file will reproduce byte-for-byte, and a
// caller opening the file AFTER it changed (new size and/or modtime) will
// not, so a stale cached download is never served for changed content.
type localizeCacheKey struct {
	path     string
	size     int64
	modified int64 // ModTime().UnixNano()
}

// localizeCacheEntry is one cached localized-to-temp-file result.
type localizeCacheEntry struct {
	tmpPath  string
	size     int64 // the localized file's own size, for the total-bytes budget
	expires  time.Time
	lastUsed time.Time
	// refs counts callers currently holding this entry's tmpPath (bumped by
	// localize, dropped by the release func it returns) -- an entry with
	// refs>0 is NEVER evicted (by TTL or by the size budget), so a slow
	// Extract still streaming from it can't have its temp file yanked out
	// from under it.
	refs int
}

// localizeCache is the process-wide cache of out-of-process ArchiveTool
// dispatch's localized temp files (Phase E wave 2 review fix 2). Cleanup is
// LAZY: sweepLocked runs at the start of every localize call (for ANY key),
// so an expired/over-budget entry is reclaimed the next time archive
// browsing touches the cache at all, without a dedicated background
// goroutine to manage the lifecycle of.
type localizeCache struct {
	mu      sync.Mutex
	entries map[localizeCacheKey]*localizeCacheEntry
	total   int64
	group   singleflight.Group
}

func newLocalizeCache() *localizeCache {
	return &localizeCache{entries: map[localizeCacheKey]*localizeCacheEntry{}}
}

// defaultLocalizeCache is the process-wide instance resolveArchiveSource uses.
var defaultLocalizeCache = newLocalizeCache()

// localize returns a cached, already-localized temp file's path for key,
// calling download() to actually produce one only on a true cache miss --
// concurrent callers racing the SAME miss join a single in-flight download
// via singleflight rather than each downloading their own copy. The returned
// release must be called exactly once when the caller is done with the temp
// file (immediately after Meta/List read what they need from it, or deferred
// until an Extract's returned stream is closed) -- it never deletes the file
// itself; only a later sweepLocked (triggered by ANY subsequent localize
// call, possibly for a different key) does that, once refs has dropped back
// to zero.
func (c *localizeCache) localize(key localizeCacheKey, download func() (string, error)) (path string, release func(), err error) {
	c.mu.Lock()
	c.sweepLocked()
	if e, ok := c.entries[key]; ok {
		e.refs++
		e.lastUsed = time.Now()
		e.expires = time.Now().Add(localizeCacheTTL)
		c.mu.Unlock()
		return e.tmpPath, c.releaseFunc(key), nil
	}
	c.mu.Unlock()

	groupKey := fmt.Sprintf("%s|%d|%d", key.path, key.size, key.modified)
	_, err, _ = c.group.Do(groupKey, func() (any, error) {
		p, derr := download()
		if derr != nil {
			return nil, derr
		}
		sz := int64(0)
		if info, statErr := os.Stat(p); statErr == nil {
			sz = info.Size()
		}
		c.mu.Lock()
		// Deliberately NOT followed by a sweepLocked() here: the entry just
		// inserted has refs==0 (not yet bumped -- every caller of this
		// localize call, winner included, bumps it AFTER Do returns below),
		// so sweeping immediately could evict the very entry this call just
		// produced before its own caller ever gets to reference it. Going
		// over localizeCacheMaxTotalBytes from this insert is instead
		// corrected lazily by the NEXT localize call's leading sweepLocked
		// (for any key) -- consistent with the whole cache's "lazy cleanup"
		// design (see the package doc comment on localizeCache).
		c.entries[key] = &localizeCacheEntry{
			tmpPath:  p,
			size:     sz,
			expires:  time.Now().Add(localizeCacheTTL),
			lastUsed: time.Now(),
		}
		c.total += sz
		c.mu.Unlock()
		return nil, nil
	})
	if err != nil {
		return "", func() {}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		// Vanishingly unlikely (evicted by a concurrent sweep between the
		// downloader's insert and this lookup): treat as a fresh miss rather
		// than handing back a path that might already be gone.
		return "", func() {}, errors.New("archive: localized temp file was evicted before use")
	}
	e.refs++
	e.lastUsed = time.Now()
	return e.tmpPath, c.releaseFunc(key), nil
}

// releaseFunc returns a once-only release callback for key: it drops one
// reference, never deleting the file itself (see localize's doc comment).
func (c *localizeCache) releaseFunc(key localizeCacheKey) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if e, ok := c.entries[key]; ok && e.refs > 0 {
				e.refs--
			}
		})
	}
}

// sweepLocked removes every expired, unreferenced entry, then -- if still
// over localizeCacheMaxTotalBytes -- evicts unreferenced entries in
// least-recently-used order until back under budget or nothing evictable
// remains. An entry with refs>0 is never touched by either pass (see
// localizeCacheEntry's doc comment). Caller must hold c.mu.
func (c *localizeCache) sweepLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if e.refs == 0 && now.After(e.expires) {
			c.total -= e.size
			_ = os.Remove(e.tmpPath)
			delete(c.entries, k)
		}
	}
	if c.total <= localizeCacheMaxTotalBytes {
		return
	}
	type candidate struct {
		key localizeCacheKey
		e   *localizeCacheEntry
	}
	var candidates []candidate
	for k, e := range c.entries {
		if e.refs == 0 {
			candidates = append(candidates, candidate{k, e})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].e.lastUsed.Before(candidates[j].e.lastUsed) })
	for _, cd := range candidates {
		if c.total <= localizeCacheMaxTotalBytes {
			break
		}
		c.total -= cd.e.size
		_ = os.Remove(cd.e.tmpPath)
		delete(c.entries, cd.key)
	}
}
