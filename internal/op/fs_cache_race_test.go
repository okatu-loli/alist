package op

// package op (not op_test): createTestStorage (relocate_hook_test.go) is
// unexported, and these tests need direct access to the unexported
// listCache — both to seed it (the Local driver's Config().NoCache is true,
// so a real List() call never populates listCache for it) and to observe
// the Del triggered by ClearCache.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestCacheRMWNoLostUpdateUnderConcurrency pins the actual invariant
// listCacheMu exists for: delCacheObj's Get -> clone -> filter -> Set cycle
// is a classic read-modify-write, and go-cache's own per-shard locking only
// makes each individual Get/Set call race-free — it does nothing to stop two
// overlapping RMW cycles from both reading the same snapshot and one Set
// clobbering the other's deletion (a lost update). That failure mode is
// invisible to `go test -race` (no single memory access is unsynchronized),
// so the oracle here is the final cache content, not the race detector.
//
// n goroutines each delete one distinct object from an n-object seeded
// slice via delCacheObj. If listCacheMu serializes the RMW cycles, every
// deletion lands and the final slice is empty. If it doesn't, concurrent
// cycles race on the same base snapshot and some deletions are lost.
func TestCacheRMWNoLostUpdateUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	// Every delCacheObj call re-Sets the entry via cacheSet, which keys the
	// TTL off storage.GetStorage().CacheExpiration minutes. createTestStorage
	// leaves that at its zero value, which go-cache treats as "expire
	// immediately" (WithEx(0) sets expire = now) -- the very first concurrent
	// delCacheObj call would then evict the entry and every subsequent Get
	// would report ok=false regardless of listCacheMu. Create the storage
	// directly here with a real CacheExpiration so the only thing under test
	// is the RMW lost-update behavior, not cache eviction.
	sid, err := CreateStorage(context.Background(), model.Storage{
		MountPath:       "/rmw",
		Driver:          "Local",
		Addition:        `{"root_folder_path":"` + dir + `"}`,
		CacheExpiration: 1,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = DeleteStorageById(context.Background(), sid) })
	storage, err := GetStorageByMountPath("/rmw")
	if err != nil {
		t.Fatal(err)
	}

	const n = 200
	path := "/x"
	key := Key(storage, path)

	objs := make([]model.Obj, 0, n)
	for i := 0; i < n; i++ {
		objs = append(objs, &model.Object{Name: fmt.Sprintf("obj%d", i)})
	}
	listCache.Set(key, objs, cache.WithEx[[]model.Obj](time.Minute))

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			delCacheObj(storage, path, &model.Object{Name: fmt.Sprintf("obj%d", i)})
		}(i)
	}
	wg.Wait()

	final, ok := listCache.Get(key)
	if !ok {
		t.Fatal("expected cache entry to still exist after concurrent deletes")
	}
	if len(final) != 0 {
		t.Fatalf("lost update: expected all %d objects deleted, %d remain (listCacheMu should serialize delCacheObj's get-modify-set cycle)", n, len(final))
	}
}

func TestMoveDirClearsStaleSubtreeCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "sub", "child.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	createTestStorage(t, "/ms", dir)
	storage, err := GetStorageByMountPath("/ms")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Warm the cache for the subtree that is about to move. The Local
	// driver's Config().NoCache is true, so a real List() call never writes
	// through to listCache — seed it directly to simulate a warm cache
	// state and exercise ClearCache's Del, independent of driver caching.
	key := Key(storage, "/src/sub")
	listCache.Set(key, []model.Obj{&model.Object{Name: "child.txt"}}, cache.WithEx[[]model.Obj](time.Minute))
	if _, ok := listCache.Get(key); !ok {
		t.Fatal("expected warm cache entry before move")
	}

	if err := Move(ctx, storage, "/src/sub", "/dst"); err != nil {
		t.Fatalf("move: %v", err)
	}

	// the recursive ClearCache triggered by Move must have dropped the stale
	// subtree entry; Del is not observable from outside the op package, so
	// this check needs package op.
	if _, ok := listCache.Get(key); ok {
		t.Fatal("stale subtree cache entry for /src/sub survived the move")
	}

	// the old path no longer exists on disk, so a cache-miss List must fail
	// rather than silently resurrect stale entries
	if _, err := List(ctx, storage, "/src/sub", model.ListArgs{}); err == nil {
		t.Fatal("expected List(/src/sub) to fail after move: old path should not resolve")
	}
}
