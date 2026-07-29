package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// This file is the Wave 4 adversarial ("red team") regression suite for the
// file-search MVP. Each test targets one attack surface from the security
// section of the design spec and pins the invariant with a load-bearing
// assertion whose comment names the exact production line that, if removed,
// turns the test red. The head invariant under attack throughout is: a search
// result can NEVER contain a file the caller cannot read or one hidden by meta
// rules, and search visibility must equal browse visibility — no more, no less.

// mountTree writes files (rel path -> content, parent dirs auto-created) under a
// fresh temp dir and mounts it as a unique Local storage. Returns the mount.
// It reuses searchFixtureSeq (search_test.go) for collision-free mount paths on
// the package's shared in-memory DB.
func mountTree(t *testing.T, files map[string]string) string {
	t.Helper()
	n := atomic.AddInt64(&searchFixtureSeq, 1)
	mount := fmt.Sprintf("/rt%d", n)

	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
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

func allowRead(path string, recursive bool) model.Permission {
	return model.Permission{Path: path, Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: recursive}
}

func denyRead(path string, recursive bool) model.Permission {
	return model.Permission{Path: path, Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: recursive}
}

// userWith builds a scoped principal whose entire policy is the given rows,
// exercising the real op.FilterListing decision path without seeding roles.
func userWith(id uint, perms ...model.Permission) *model.User {
	return &model.User{ID: id, Username: fmt.Sprintf("rtuser%d", id), Scoped: true, ScopedPermissions: perms}
}

func names(rs []Result) map[string]bool {
	m := make(map[string]bool, len(rs))
	for _, r := range rs {
		m[r.Name] = true
	}
	return m
}

// TestSearchDeepUnreadableSubtreeNotEnqueued attacks surface #1 (deep unreadable
// subtree). The caller may read /pub recursively but a recursive DENY covers the
// deeper /pub/deep/vault. Searching a filename that lives ONLY under vault must
// return nothing, because op.FilterListing drops the denied vault directory from
// its parent's listing, so the walk never enqueues vault and its whole subtree
// is untouchable. A sanity hit at the readable /pub/deep level proves the walk
// really descended (the empty vault result is a true deny, not a dead traversal).
//
// Load-bearing: delete the `objs = op.FilterListing(...)` line in Search and the
// topsecret assertion leaks (vault gets enqueued, topsecret.txt surfaces).
func TestSearchDeepUnreadableSubtreeNotEnqueued(t *testing.T) {
	mount := mountTree(t, map[string]string{
		"pub/hello.txt":                  "1",
		"pub/deep/inner.txt":             "2",
		"pub/deep/vault/topsecret.txt":   "3",
		"pub/deep/vault/nested/more.txt": "4",
	})
	// Read all, then carve out a recursive deny over the deep vault subtree.
	user := userWith(71001, allowRead("/", true), denyRead(mount+"/pub/deep/vault", true))

	ctx := context.Background()
	// Sanity: the readable /pub/deep level IS reached by the walk.
	if hits, _, err := Search(ctx, user, Options{Parent: mount + "/pub", Keyword: "inner", MaxDepth: 20, Limit: 100}); err != nil || len(hits) != 1 {
		t.Fatalf("readable inner.txt must be found (proves walk descends): hits=%+v err=%v", hits, err)
	}

	// The vault directory name itself must not leak (denied read beats traverse).
	if hits, _, err := Search(ctx, user, Options{Parent: mount + "/pub", Keyword: "vault", MaxDepth: 20, Limit: 100}); err != nil || len(hits) != 0 {
		t.Fatalf("denied vault dir must not appear in results: hits=%+v err=%v", hits, err)
	}

	// The leak assertion: files that live only under the denied vault subtree.
	for _, kw := range []string{"topsecret", "more"} {
		hits, _, err := Search(ctx, user, Options{Parent: mount + "/pub", Keyword: kw, MaxDepth: 20, Limit: 100})
		if err != nil {
			t.Fatalf("Search(%q) error: %v", kw, err)
		}
		if len(hits) != 0 {
			t.Fatalf("Search(%q) leaked file(s) under the read-denied vault subtree: %+v", kw, hits)
		}
	}
}

// TestSearchHideMetaHonoursSeeHides attacks surface #1 (meta hide). A hide rule
// on the directory conceals *.secret from callers without see_hides. Searching
// "secret" must not surface the hidden file for such a caller, and MUST surface
// it for a caller who holds see_hides — the semantics are symmetric with browse.
//
// Load-bearing: delete the `!d.Can(ActSeeHides) && matchAnyHide(...)` guard in
// op.filterWithPatterns and the "without see_hides" case leaks passwords.secret.
func TestSearchHideMetaHonoursSeeHides(t *testing.T) {
	mount := mountTree(t, map[string]string{
		"h/visible.txt":      "v",
		"h/passwords.secret": "p",
	})
	// Hide rule on the /h directory: anything ending in .secret is concealed.
	m := &model.Meta{Path: mount + "/h", Hide: `\.secret$`, HSub: true}
	if err := op.CreateMeta(m); err != nil {
		t.Fatalf("create hide meta: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })

	ctx := context.Background()

	// Caller WITHOUT see_hides: the hidden .secret file must not appear.
	plain := userWith(71002, allowRead("/", true))
	hits, _, err := Search(ctx, plain, Options{Parent: mount + "/h", Keyword: "secret", MaxDepth: 20, Limit: 100})
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hidden passwords.secret leaked to a caller without see_hides: %+v", hits)
	}

	// Caller WITH see_hides: the same file must be searchable (symmetry).
	seer := userWith(71003, model.Permission{
		Path: "/", Actions: model.Actions{model.ActRead, model.ActSeeHides}, Effect: model.EffectAllow, Recursive: true,
	})
	hits, _, err = Search(ctx, seer, Options{Parent: mount + "/h", Keyword: "secret", MaxDepth: 20, Limit: 100})
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "passwords.secret" {
		t.Fatalf("see_hides caller must find passwords.secret: %+v", hits)
	}
}

// TestSearchTraverseOnlyEqualsBrowseVisibility is the key semantic assertion for
// surface #1 (traverse-only directories). The caller has an EXACT read grant on
// one deep file (gem.txt); every ancestor is traverse-only (readable only as a
// path to the grant), and gem.txt has an unreadable sibling (other.txt). The
// claim proven here: search visibility == browse visibility, because both are
// driven by the identical op.FilterListing decision at each directory's own real
// path. So:
//   - the readable gem.txt is found (traverse path works);
//   - the traverse-only ancestor dirs are surfaced when their name matches, which
//     is exactly what browsing (FilterListing) would show — not a leak;
//   - the unreadable sibling other.txt is NEVER surfaced;
//   - and the set search would collect at the leaf dir equals what FilterListing
//     (browse) returns there — asserted directly.
func TestSearchTraverseOnlyEqualsBrowseVisibility(t *testing.T) {
	mount := mountTree(t, map[string]string{
		"t/mid/leaf/gem.txt":   "g",
		"t/mid/leaf/other.txt": "o",
	})
	// Read ONLY the exact deep file. Ancestors become traverse-only.
	user := userWith(71004, allowRead(mount+"/t/mid/leaf/gem.txt", false))
	ctx := context.Background()

	// Browse ground truth at the leaf: FilterListing is the authority the browse
	// endpoint uses. It must show gem.txt and hide other.txt.
	leaf := mount + "/t/mid/leaf"
	objs, err := fs.List(ctx, leaf, &fs.ListArgs{NoLog: true})
	if err != nil {
		t.Fatalf("list leaf: %v", err)
	}
	browse := map[string]bool{}
	for _, o := range op.FilterListing(ctx, user, leaf, objs) {
		browse[o.GetName()] = true
	}
	if !browse["gem.txt"] || browse["other.txt"] {
		t.Fatalf("browse ground truth wrong: gem visible=%v other hidden=%v", browse["gem.txt"], !browse["other.txt"])
	}

	// Search from the top: it must find exactly what browse allows and nothing
	// more. Start from both the top and the leaf to prove per-level dirPath
	// filtering is used regardless of the search root (surface #1, last bullet).
	for _, root := range []string{mount + "/t", leaf} {
		if hits, _, err := Search(ctx, user, Options{Parent: root, Keyword: "gem", MaxDepth: 20, Limit: 100}); err != nil || len(hits) != 1 || hits[0].Name != "gem.txt" {
			t.Fatalf("from %s: readable gem.txt must be found via traverse: %+v err=%v", root, hits, err)
		}
		if hits, _, err := Search(ctx, user, Options{Parent: root, Keyword: "other", MaxDepth: 20, Limit: 100}); err != nil || len(hits) != 0 {
			t.Fatalf("from %s: unreadable sibling other.txt must never surface: %+v err=%v", root, hits, err)
		}
	}

	// Traverse-only ancestor dirs are visible exactly as browse shows them: a
	// keyword matching an ancestor name yields the dir (consistent, not a leak).
	if hits, _, err := Search(ctx, user, Options{Parent: mount + "/t", Keyword: "leaf", MaxDepth: 20, Limit: 100}); err != nil || len(hits) != 1 || !hits[0].IsDir {
		t.Fatalf("traverse-only leaf dir must surface just as browse lists it: %+v err=%v", hits, err)
	}
}

// TestSearchKeywordIsLiteralSubstring attacks surface #3 (ReDoS). The keyword is
// a plain case-insensitive substring test — never compiled as a regexp — so
// regex metacharacters are inert and a catastrophic pattern cannot backtrack.
// Proven two ways: a keyword containing regex quantifiers matches a filename
// literally (a regex would interpret "a+b" as a quantifier, not the literal
// three characters), and a classic catastrophic pattern against a long all-'a'
// name returns instantly with no match.
func TestSearchKeywordIsLiteralSubstring(t *testing.T) {
	longA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 40 'a'
	mount := mountTree(t, map[string]string{
		"lit/a+b.txt":           "x",
		"lit/plain.txt":         "y",
		"lit/" + longA + ".txt": "z",
	})
	user := userWith(71005, allowRead("/", true))
	ctx := context.Background()

	// Literal match: "a+b" is found in "a+b.txt" only because matching is
	// strings.Contains, not a regexp where '+' would be a quantifier.
	hits, _, err := Search(ctx, user, Options{Parent: mount + "/lit", Keyword: "a+b", MaxDepth: 20, Limit: 100})
	if err != nil || len(hits) != 1 || hits[0].Name != "a+b.txt" {
		t.Fatalf(`keyword "a+b" must literally match a+b.txt: %+v err=%v`, hits, err)
	}

	// Catastrophic-looking pattern: inert as a literal substring, returns fast.
	for _, kw := range []string{"(a+)+$", ".*.*.*.*", "(a|a)*$"} {
		start := time.Now()
		hits, _, err := Search(ctx, user, Options{Parent: mount + "/lit", Keyword: kw, MaxDepth: 20, Limit: 100})
		if err != nil {
			t.Fatalf("Search(%q) error: %v", kw, err)
		}
		if len(hits) != 0 {
			t.Fatalf("Search(%q) unexpectedly matched (keyword must be a literal substring): %+v", kw, hits)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("Search(%q) took %v — a linear substring scan must not backtrack", kw, d)
		}
	}
}

// TestSearchWideTreeBounded attacks surface #2 (traversal DoS, wide fan-out). A
// directory with many child dirs, walked under a tiny maxDirsVisited, must stop
// at the cap, report truncated=true, return only what it collected before the
// cap, and never panic or run unbounded.
func TestSearchWideTreeBounded(t *testing.T) {
	files := map[string]string{}
	const fanout = 40
	for i := 0; i < fanout; i++ {
		files[fmt.Sprintf("w/d%02d/hit.txt", i)] = "h"
	}
	mount := mountTree(t, files)
	user := userWith(71006, allowRead("/", true))

	prev := maxDirsVisited
	maxDirsVisited = 3 // root /w + a couple of child dirs, then the cap fires
	t.Cleanup(func() { maxDirsVisited = prev })

	results, truncated, err := Search(context.Background(), user, Options{Parent: mount + "/w", Keyword: "hit", MaxDepth: 20, Limit: 1000})
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if !truncated {
		t.Fatalf("wide tree past maxDirsVisited must report truncated=true")
	}
	if len(results) >= fanout {
		t.Fatalf("walk must have stopped at the visit cap, got %d hits (>= fanout %d)", len(results), fanout)
	}
}

// TestSearchDeepTreeMaxDepthBounded attacks surface #2 (traversal DoS, depth). A
// deep chain walked with a small MaxDepth must stop descending: a target below
// the depth bound is unreachable, while the same tree with a generous MaxDepth
// finds it. This pins that MaxDepth truly bounds descent.
func TestSearchDeepTreeMaxDepthBounded(t *testing.T) {
	mount := mountTree(t, map[string]string{
		"deep/l1/l2/l3/l4/target.txt": "t",
	})
	user := userWith(71007, allowRead("/", true))
	ctx := context.Background()

	// MaxDepth=2 below /deep reaches l1, l2 but not l3/l4/target.txt.
	if hits, _, err := Search(ctx, user, Options{Parent: mount + "/deep", Keyword: "target", MaxDepth: 2, Limit: 100}); err != nil || len(hits) != 0 {
		t.Fatalf("MaxDepth=2 must not reach the deeper target: %+v err=%v", hits, err)
	}
	// Generous depth finds it, proving the tree is otherwise walkable.
	if hits, _, err := Search(ctx, user, Options{Parent: mount + "/deep", Keyword: "target", MaxDepth: 20, Limit: 100}); err != nil || len(hits) != 1 {
		t.Fatalf("MaxDepth=20 must reach the target: %+v err=%v", hits, err)
	}
}

// TestSearchNoResultLeakAcrossManyMatches is a broad backstop: with a mix of
// readable and unreadable files that all match the keyword, every returned hit
// must be a readable one and none of the denied ones — regardless of ordering.
func TestSearchNoResultLeakAcrossManyMatches(t *testing.T) {
	mount := mountTree(t, map[string]string{
		"data/report_a.txt":      "1",
		"data/report_b.txt":      "2",
		"data/priv/report_x.txt": "3",
		"data/priv/report_y.txt": "4",
	})
	user := userWith(71008, allowRead(mount+"/data", true), denyRead(mount+"/data/priv", true))

	results, _, err := Search(context.Background(), user, Options{Parent: mount + "/data", Keyword: "report", MaxDepth: 20, Limit: 100})
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	got := names(results)
	if !got["report_a.txt"] || !got["report_b.txt"] {
		t.Fatalf("readable report_a/report_b must be found: %+v", results)
	}
	if got["report_x.txt"] || got["report_y.txt"] {
		t.Fatalf("read-denied report_x/report_y leaked: %+v", results)
	}
}
