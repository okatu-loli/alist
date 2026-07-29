package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeFakeDownload creates a small file under dir and returns a download
// func that "produces" it while counting how many times it actually ran --
// the precise, deterministic way to prove localize's caching/dedup actually
// avoids repeat downloads (review fix 2).
func writeFakeDownload(t *testing.T, dir, name string, content []byte, counter *int32) func() (string, error) {
	t.Helper()
	return func() (string, error) {
		atomic.AddInt32(counter, 1)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return "", err
		}
		return p, nil
	}
}

// TestLocalizeCacheReusesEntryForSameKey pins the core caching behavior: a
// second localize call with the identical key must NOT invoke download again,
// and must hand back the exact same path.
func TestLocalizeCacheReusesEntryForSameKey(t *testing.T) {
	c := newLocalizeCache()
	dir := t.TempDir()
	var downloads int32
	key := localizeCacheKey{path: "/x/a.fx", size: 5, modified: 1}

	p1, rel1, err := c.localize(key, writeFakeDownload(t, dir, "a", []byte("hello"), &downloads))
	if err != nil {
		t.Fatalf("localize #1: %v", err)
	}
	p2, rel2, err := c.localize(key, writeFakeDownload(t, dir, "a", []byte("hello"), &downloads))
	if err != nil {
		t.Fatalf("localize #2: %v", err)
	}
	if p1 != p2 {
		t.Fatalf("paths differ across a same-key repeat call: %q vs %q", p1, p2)
	}
	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Fatalf("download ran %d times, want exactly 1 (same key must be cached)", got)
	}
	rel1()
	rel2()
}

// TestLocalizeCacheConcurrentSameKeySingleflights pins the singleflight
// dedup: many goroutines racing a cache MISS for the identical key must
// collapse into exactly one download, all receiving the same path.
func TestLocalizeCacheConcurrentSameKeySingleflights(t *testing.T) {
	c := newLocalizeCache()
	dir := t.TempDir()
	var downloads int32
	key := localizeCacheKey{path: "/x/racy.fx", size: 3, modified: 7}

	const n = 20
	paths := make([]string, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			p, rel, err := c.localize(key, func() (string, error) {
				atomic.AddInt32(&downloads, 1)
				// A tiny sleep widens the race window so concurrent callers
				// actually overlap the in-flight download instead of running
				// serially by accident.
				time.Sleep(5 * time.Millisecond)
				out := filepath.Join(dir, "racy")
				if err := os.WriteFile(out, []byte("hi"), 0o644); err != nil {
					return "", err
				}
				return out, nil
			})
			if err != nil {
				t.Errorf("localize goroutine %d: %v", i, err)
				return
			}
			paths[i] = p
			releases[i] = rel
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Fatalf("download ran %d times across %d concurrent same-key callers, want exactly 1", got, n)
	}
	for i, p := range paths {
		if p != paths[0] {
			t.Fatalf("goroutine %d got path %q, want %q (all concurrent callers must share one download)", i, p, paths[0])
		}
	}
	for _, rel := range releases {
		if rel != nil {
			rel()
		}
	}
}

// TestLocalizeCacheEvictsExpiredEntryAndRedownloads pins the TTL half of
// review fix 2: once an unreferenced entry's TTL has elapsed, the next
// localize call (any key, triggering a sweep) removes its temp file, and a
// following call for that SAME key downloads again rather than reusing a
// stale path.
func TestLocalizeCacheEvictsExpiredEntryAndRedownloads(t *testing.T) {
	c := newLocalizeCache()
	dir := t.TempDir()
	var downloads int32
	key := localizeCacheKey{path: "/x/b.fx", size: 5, modified: 2}

	p1, rel1, err := c.localize(key, writeFakeDownload(t, dir, "b", []byte("first"), &downloads))
	if err != nil {
		t.Fatalf("localize #1: %v", err)
	}
	rel1() // drop to refs==0 so it becomes evictable once expired

	// Force this entry to look already-expired.
	c.mu.Lock()
	c.entries[key].expires = time.Now().Add(-time.Hour)
	c.mu.Unlock()

	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("temp file should still exist before any sweep runs: %v", err)
	}

	// Trigger a sweep via a DIFFERENT key -- sweepLocked runs at the start of
	// every localize call, for any key.
	otherKey := localizeCacheKey{path: "/x/other.fx", size: 1, modified: 99}
	var otherDownloads int32
	_, relOther, err := c.localize(otherKey, writeFakeDownload(t, dir, "other", []byte("x"), &otherDownloads))
	if err != nil {
		t.Fatalf("localize (sweep trigger): %v", err)
	}
	defer relOther()

	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatalf("expired unreferenced temp file should have been removed by the sweep, stat err = %v", err)
	}

	// Same key again: must download fresh (not reuse the now-gone path).
	p2, rel2, err := c.localize(key, writeFakeDownload(t, dir, "b2", []byte("second"), &downloads))
	if err != nil {
		t.Fatalf("localize #2 (after eviction): %v", err)
	}
	defer rel2()
	if got := atomic.LoadInt32(&downloads); got != 2 {
		t.Fatalf("download ran %d times across the eviction+redownload sequence, want exactly 2", got)
	}
	if p2 == p1 {
		t.Fatal("re-download after eviction must not reuse the removed path")
	}
}

// TestLocalizeCacheReferencedEntrySurvivesExpiryUntilReleased pins the
// refcount guard: an entry still referenced (Extract's stream not yet
// closed) must NOT be evicted even past its TTL, and becomes evictable only
// once every reference is released.
func TestLocalizeCacheReferencedEntrySurvivesExpiryUntilReleased(t *testing.T) {
	c := newLocalizeCache()
	dir := t.TempDir()
	var downloads int32
	key := localizeCacheKey{path: "/x/c.fx", size: 4, modified: 3}

	p1, rel1, err := c.localize(key, writeFakeDownload(t, dir, "c", []byte("data"), &downloads))
	if err != nil {
		t.Fatalf("localize: %v", err)
	}
	// Held reference (rel1 not yet called) + already-expired.
	c.mu.Lock()
	c.entries[key].expires = time.Now().Add(-time.Hour)
	c.mu.Unlock()

	// Sweep via a different key: must NOT touch the still-referenced entry.
	otherKey := localizeCacheKey{path: "/x/other2.fx", size: 1, modified: 5}
	var otherDownloads int32
	_, relOther, err := c.localize(otherKey, writeFakeDownload(t, dir, "other2", []byte("x"), &otherDownloads))
	if err != nil {
		t.Fatalf("localize (sweep trigger): %v", err)
	}
	defer relOther()

	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("still-referenced, expired entry must survive a sweep: %v", err)
	}

	rel1() // now refs==0
	// Trigger one more sweep; now it should go.
	_, relOther2, err := c.localize(otherKey, writeFakeDownload(t, dir, "other2", []byte("x"), &otherDownloads))
	if err != nil {
		t.Fatalf("localize (second sweep trigger): %v", err)
	}
	defer relOther2()
	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatalf("expired entry should be swept once its last reference is released, stat err = %v", err)
	}
}

// TestLocalizeCacheEvictsLeastRecentlyUsedOverBudget pins the size-budget
// half of review fix 2: once the cache's total exceeds
// localizeCacheMaxTotalBytes, unreferenced entries are evicted
// least-recently-used first until back under budget.
func TestLocalizeCacheEvictsLeastRecentlyUsedOverBudget(t *testing.T) {
	c := newLocalizeCache()
	dir := t.TempDir()
	oldBudget := localizeCacheMaxTotalBytes
	localizeCacheMaxTotalBytes = 10 // bytes -- tiny, deliberately
	t.Cleanup(func() { localizeCacheMaxTotalBytes = oldBudget })

	mk := func(name string, n int, mod int64) (localizeCacheKey, string) {
		key := localizeCacheKey{path: "/x/" + name, size: int64(n), modified: mod}
		var d int32
		p, rel, err := c.localize(key, writeFakeDownload(t, dir, name, []byte(fmt.Sprintf("%0*d", n, 0)), &d))
		if err != nil {
			t.Fatalf("localize %s: %v", name, err)
		}
		rel() // refs==0 immediately, so it's evictable
		return key, p
	}

	// Each entry is 6 bytes; budget is 10, so at most one fits comfortably --
	// inserting a second pushes the cache over budget, and going over budget
	// is corrected lazily by the NEXT localize call's leading sweep (any
	// key) -- see localize's doc comment -- so a third, throwaway call is
	// what actually triggers evicting the first (LRU) entry here.
	keyA, pathA := mk("lru-a", 6, 1)
	time.Sleep(5 * time.Millisecond) // ensure a distinct, later lastUsed for B
	_, pathB := mk("lru-b", 6, 2)
	mk("lru-trigger", 1, 3) // forces the lazy over-budget sweep to run

	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Fatalf("least-recently-used entry A should have been evicted once over budget, stat err = %v", err)
	}
	if _, err := os.Stat(pathB); err != nil {
		t.Fatalf("most-recently-inserted entry B should survive: %v", err)
	}
	c.mu.Lock()
	_, stillCached := c.entries[keyA]
	c.mu.Unlock()
	if stillCached {
		t.Fatal("evicted entry A must also be removed from the cache map")
	}
}
