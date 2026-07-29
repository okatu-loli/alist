package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	// Import the Local driver directly (init side effect) so CreateStorage can
	// back the fixture without pulling the whole driver set.
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:search_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

var searchFixtureSeq int64

// setupSearchFixture mounts an isolated Local storage at a unique /srchN whose
// tree is:
//
//	/srchN/a/x.txt          ("xx")
//	/srchN/a/b/y.txt        ("hello")
//	/srchN/a/secret/z.txt   ("zzz")
//
// The b/secret split is what the RBAC leak test asserts against. Returns the
// mount path.
func setupSearchFixture(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&searchFixtureSeq, 1)
	mount := fmt.Sprintf("/srch%d", n)

	dir := t.TempDir()
	for _, d := range []string{"a/b", "a/secret"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writes := map[string]string{
		"a/x.txt":        "xx",
		"a/b/y.txt":      "hello",
		"a/secret/z.txt": "zzz",
	}
	for rel, content := range writes {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })
	return mount
}

// scopedReader builds a request-scoped principal whose entire authorization
// policy is a single recursive ActRead grant on scopePath. This exercises the
// real op.FilterListing decision path (UserPermissions -> ResolveEffectivePolicy)
// without seeding roles, and lets each test choose exactly how much of the tree
// the caller may read. There is no admin short-circuit in the policy engine, so
// a "sees everything" caller is just a reader scoped at "/".
func scopedReader(id uint, scopePath string) *model.User {
	return &model.User{
		ID:       id,
		Username: fmt.Sprintf("searchreader%d", id),
		Scoped:   true,
		ScopedPermissions: []model.Permission{{
			Path:      scopePath,
			Actions:   model.Actions{model.ActRead},
			Effect:    model.EffectAllow,
			Recursive: true,
		}},
	}
}

// TestSearchMatchesAndFilters: a caller who may read the whole tree finds the
// nested match, with the right path/IsDir/Size, and the match is
// case-insensitive.
func TestSearchMatchesAndFilters(t *testing.T) {
	mount := setupSearchFixture(t)
	user := scopedReader(70001, "/") // read everything

	for _, kw := range []string{"y", "Y"} {
		results, truncated, err := Search(context.Background(), user, Options{
			Parent: mount + "/a", Keyword: kw, MaxDepth: 20, Limit: 100,
		})
		if err != nil {
			t.Fatalf("Search(%q) error: %v", kw, err)
		}
		if truncated {
			t.Fatalf("Search(%q) unexpectedly truncated", kw)
		}
		if len(results) != 1 {
			t.Fatalf("Search(%q) = %+v, want exactly 1 hit", kw, results)
		}
		got := results[0]
		if got.Parent != mount+"/a/b" || got.Name != "y.txt" {
			t.Errorf("Search(%q) hit path = %s/%s, want %s/a/b/y.txt", kw, got.Parent, got.Name, mount)
		}
		if got.IsDir {
			t.Errorf("Search(%q) hit IsDir = true, want false", kw)
		}
		if got.Size != int64(len("hello")) {
			t.Errorf("Search(%q) hit Size = %d, want %d", kw, got.Size, len("hello"))
		}
	}
}

// TestSearchHidesUnreadable is the load-bearing leak regression. The caller may
// read only /a/b, not /a/secret. Searching "z" (which only /a/secret/z.txt
// satisfies) must return nothing, because op.FilterListing drops /a/secret
// before the walk ever descends into it. Deleting the op.FilterListing call in
// Search turns this test red — that call is the sole guard against leaking an
// unreadable file into results.
func TestSearchHidesUnreadable(t *testing.T) {
	mount := setupSearchFixture(t)
	// Scope covers ONLY /a/b, so /a/secret is neither readable nor traversable.
	user := scopedReader(70002, mount+"/a/b")

	// Sanity: the caller CAN see its own subtree (proves the walk works and the
	// empty "z" result is a real deny, not a broken traversal).
	if hits, _, err := Search(context.Background(), user, Options{
		Parent: mount + "/a", Keyword: "y", MaxDepth: 20, Limit: 100,
	}); err != nil || len(hits) != 1 || hits[0].Name != "y.txt" {
		t.Fatalf("readable-subtree search = %+v err=%v, want the single y.txt hit", hits, err)
	}

	// The leak assertion: z.txt lives under the unreadable /a/secret.
	results, _, err := Search(context.Background(), user, Options{
		Parent: mount + "/a", Keyword: "z", MaxDepth: 20, Limit: 100,
	})
	if err != nil {
		t.Fatalf("Search(z) error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(z) leaked unreadable file(s): %+v", results)
	}
}

// TestSearchBounds pins the safety limits: a tiny maxDirsVisited truncates
// without panicking while still returning what was collected before the cap,
// and an empty keyword is rejected.
func TestSearchBounds(t *testing.T) {
	mount := setupSearchFixture(t)
	user := scopedReader(70003, "/")

	t.Run("truncates-at-maxDirsVisited", func(t *testing.T) {
		prev := maxDirsVisited
		maxDirsVisited = 1 // only the root /a dir is listed; b and secret are not
		t.Cleanup(func() { maxDirsVisited = prev })

		// x.txt is a direct child of /a, so it is collected before the cap stops
		// the walk from descending into the subdirectories.
		results, truncated, err := Search(context.Background(), user, Options{
			Parent: mount + "/a", Keyword: "x", MaxDepth: 20, Limit: 100,
		})
		if err != nil {
			t.Fatalf("Search error: %v", err)
		}
		if !truncated {
			t.Errorf("truncated = false, want true when maxDirsVisited is exceeded")
		}
		if len(results) != 1 || results[0].Name != "x.txt" {
			t.Fatalf("results = %+v, want the single already-collected x.txt", results)
		}
	})

	t.Run("empty-keyword-rejected", func(t *testing.T) {
		for _, kw := range []string{"", "   "} {
			if _, _, err := Search(context.Background(), user, Options{
				Parent: mount + "/a", Keyword: kw,
			}); err == nil {
				t.Errorf("Search(keyword=%q) err = nil, want a rejection", kw)
			}
		}
	})

	t.Run("cancelled-context-truncates", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already done before the first loop iteration
		results, truncated, err := Search(ctx, user, Options{
			Parent: mount + "/a", Keyword: "y", MaxDepth: 20, Limit: 100,
		})
		if err != nil {
			t.Fatalf("Search error: %v", err)
		}
		if !truncated {
			t.Errorf("truncated = false, want true when context is cancelled")
		}
		if len(results) != 0 {
			t.Errorf("results = %+v, want none from a pre-cancelled context", results)
		}
	})
}
