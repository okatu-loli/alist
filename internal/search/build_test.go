package search

import (
	"context"
	"errors"
	"sort"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/search/database"
)

// --- fake tree helpers ---

func fdir(name string) model.Obj { return &model.Object{Name: name, IsFolder: true} }
func ffile(name string, size int64) model.Obj {
	return &model.Object{Name: name, Size: size, IsFolder: false}
}

// treeLister returns a lister over an in-memory {dir -> children} map, standing in
// for fs.List so the walk's emit/skip/depth core is exercised without storages.
func treeLister(tree map[string][]model.Obj) lister {
	return func(_ context.Context, dir string) ([]model.Obj, error) {
		return tree[dir], nil
	}
}

func sampleTree() map[string][]model.Obj {
	return map[string][]model.Obj{
		"/":    {fdir("a"), ffile("f1.txt", 10)},
		"/a":   {fdir("b"), ffile("f2.txt", 20)},
		"/a/b": {ffile("deep.txt", 30)},
	}
}

// collect drives buildWalk and returns the emitted nodes as "parent|name" keys.
func collect(t *testing.T, l lister, root string, ignore []string, maxDepth int, quit *chan struct{}) []ext.SearchNode {
	t.Helper()
	var got []ext.SearchNode
	err := buildWalk(context.Background(), l, root, ignore, maxDepth, quit, func(n ext.SearchNode) error {
		got = append(got, n)
		return nil
	})
	if err != nil {
		t.Fatalf("buildWalk: %v", err)
	}
	return got
}

func nodeKeys(nodes []ext.SearchNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Parent + "|" + n.Name
	}
	sort.Strings(out)
	return out
}

func eqKeys(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

// TestBuildWalkEmitsChildren proves the walk emits every object as a node parented
// on its listing dir, descends into subdirs, and never emits the start dir itself.
func TestBuildWalkEmitsChildren(t *testing.T) {
	got := collect(t, treeLister(sampleTree()), "/", nil, 20, nil)
	eqKeys(t, nodeKeys(got), []string{
		"/|a", "/|f1.txt",
		"/a|b", "/a|f2.txt",
		"/a/b|deep.txt",
	})
	// The start dir "/" is never emitted as a node.
	for _, n := range got {
		if n.Name == "" || (n.Parent == "" && n.Name == "/") {
			t.Fatalf("start dir must not be emitted: %+v", n)
		}
	}
	// Size + IsDir carried through.
	for _, n := range got {
		if n.Name == "deep.txt" && (n.Size != 30 || n.IsDir) {
			t.Fatalf("deep.txt node wrong: %+v", n)
		}
		if n.Name == "a" && !n.IsDir {
			t.Fatalf("dir a must be IsDir: %+v", n)
		}
	}
}

// TestBuildWalkMaxDepth bounds recursion: at maxDepth=1 the walk lists "/" and its
// children dirs one level down, but never descends into "/a/b".
func TestBuildWalkMaxDepth(t *testing.T) {
	got := collect(t, treeLister(sampleTree()), "/", nil, 1, nil)
	eqKeys(t, nodeKeys(got), []string{
		"/|a", "/|f1.txt",
		"/a|b", "/a|f2.txt",
	})
}

// TestBuildWalkSkipIgnorePrefix drops any path under an ignore prefix — the node
// and its whole subtree.
func TestBuildWalkSkipIgnorePrefix(t *testing.T) {
	got := collect(t, treeLister(sampleTree()), "/", []string{"/a"}, 20, nil)
	eqKeys(t, nodeKeys(got), []string{"/|f1.txt"})
}

// TestBuildWalkSkipDisableIndex proves a DisableIndex storage is excluded: the
// storageDisablesIndex seam reports true for a subtree, which prunes it.
func TestBuildWalkSkipDisableIndex(t *testing.T) {
	orig := storageDisablesIndex
	t.Cleanup(func() { storageDisablesIndex = orig })
	storageDisablesIndex = func(p string) bool { return p == "/a" || len(p) > 2 && p[:3] == "/a/" }

	got := collect(t, treeLister(sampleTree()), "/", nil, 20, nil)
	eqKeys(t, nodeKeys(got), []string{"/|f1.txt"})
}

// TestBuildWalkStopAborts proves quit-based cancellation returns immediately with
// nothing emitted when the channel is already signalled.
func TestBuildWalkStopAborts(t *testing.T) {
	quit := make(chan struct{}, 1)
	quit <- struct{}{}
	got := collect(t, treeLister(sampleTree()), "/", nil, 20, &quit)
	if len(got) != 0 {
		t.Fatalf("a pre-signalled quit must emit nothing, got %v", nodeKeys(got))
	}
}

// TestSkipBuildPathPredicate unit-tests the skip predicate directly.
func TestSkipBuildPathPredicate(t *testing.T) {
	orig := storageDisablesIndex
	t.Cleanup(func() { storageDisablesIndex = orig })
	storageDisablesIndex = func(string) bool { return false }

	if !skipBuildPath("/secret/x", []string{"/secret"}) {
		t.Error("ignore prefix must skip")
	}
	if skipBuildPath("/pub/x", []string{"/secret"}) {
		t.Error("non-matching prefix must not skip")
	}
	if skipBuildPath("/x", []string{""}) {
		t.Error("empty ignore entry must never swallow the tree")
	}
	storageDisablesIndex = func(p string) bool { return p == "/dz" }
	if !skipBuildPath("/dz", nil) {
		t.Error("DisableIndex storage must skip")
	}
}

func TestParseIgnorePaths(t *testing.T) {
	got := ParseIgnorePaths(" /a \n/b\r\n\n , /c ,")
	eqKeys(t, got, []string{"/a", "/b", "/c"})
}

// TestBuildIndexGuardRejectsReentry proves the concurrency guard: while a build
// holds Quit, Running() is true and a second BuildIndex returns BuildIndexIsRunning;
// clearing the guard flips Running() back to false.
func TestBuildIndexGuardRejectsReentry(t *testing.T) {
	if Running() {
		t.Fatal("no build should be running at start")
	}
	held := make(chan struct{}, 1)
	if !Quit.CompareAndSwap(nil, &held) {
		t.Fatal("could not install guard")
	}
	t.Cleanup(func() { Quit.Store(nil) })

	if !Running() {
		t.Fatal("Running() must be true while the guard is held")
	}
	if err := BuildIndex(context.Background(), []string{"/"}, nil, 20, false); !errors.Is(err, errs.BuildIndexIsRunning) {
		t.Fatalf("re-entry err = %v, want BuildIndexIsRunning", err)
	}

	Quit.Store(nil)
	if Running() {
		t.Fatal("Running() must be false after the guard is cleared")
	}
}

// TestBuildIndexNoActiveBackend: with no active indexer, BuildIndex acquires and
// releases the guard and returns SearchNotAvailable.
func TestBuildIndexNoActiveBackend(t *testing.T) {
	resetBase(t)
	if err := BuildIndex(context.Background(), []string{"/"}, nil, 20, false); !errors.Is(err, errs.SearchNotAvailable) {
		t.Fatalf("err = %v, want SearchNotAvailable", err)
	}
	if Running() {
		t.Fatal("guard must be released after BuildIndex returns")
	}
}

// setupBuildDB registers + activates the real database backend against the shared
// migrated in-memory DB (created by this package's init(), which already has the
// search_nodes + settings tables), so BuildIndex writes real nodes and progress.
// It clears the index up front for isolation and tears the backend down after.
func setupBuildDB(t *testing.T) {
	t.Helper()
	extID := database.ExtensionID
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    extID,
		Name:  map[string]string{"en": "db"},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}); err != nil {
		t.Fatalf("register owning extension: %v", err)
	}
	RegisterBackend(database.SearcherName, extID, &database.Indexer{})
	if err := Init(database.SearcherName); err != nil {
		t.Fatalf("Init(%s): %v", database.SearcherName, err)
	}
	if err := (&database.Indexer{}).Clear(context.Background()); err != nil {
		t.Fatalf("clear index for isolation: %v", err)
	}
	t.Cleanup(func() {
		_ = (&database.Indexer{}).Clear(context.Background())
		_ = Init("none")
		RegisterBackend(database.SearcherName, extID, nil)
		extension.DefaultRegistry.Deregister(extID)
	})
}

// TestBuildIndexEndToEnd drives a full build against the real database backend
// over a fake tree: every object lands in the index and progress ends IsDone with
// the right ObjCount.
func TestBuildIndexEndToEnd(t *testing.T) {
	setupBuildDB(t)

	// Seams: identity admin ctx (no seeded admin user needed) + fake tree lister.
	origCtx, origLister, origBatch := buildAdminCtx, buildLister, buildBatchSize
	t.Cleanup(func() { buildAdminCtx, buildLister, buildBatchSize = origCtx, origLister, origBatch })
	buildAdminCtx = func(ctx context.Context) (context.Context, error) { return ctx, nil }
	buildLister = treeLister(sampleTree())
	buildBatchSize = 2 // force multiple batches + interim progress writes

	if err := BuildIndex(context.Background(), []string{"/"}, nil, 20, true); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	// All 5 objects indexed (queryable through the active backend).
	ix := &database.Indexer{}
	roots, err := ix.Get(context.Background(), "/")
	if err != nil {
		t.Fatalf("Get(/): %v", err)
	}
	if len(roots) != 2 { // a, f1.txt
		t.Fatalf("root children = %d, want 2", len(roots))
	}
	deep, err := ix.Get(context.Background(), "/a/b")
	if err != nil || len(deep) != 1 || deep[0].Name != "deep.txt" {
		t.Fatalf("Get(/a/b) = %v (%v), want [deep.txt]", deep, err)
	}

	// Progress finished with the full object count.
	p, err := Progress()
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if !p.IsDone {
		t.Fatal("progress must end IsDone")
	}
	if p.ObjCount != 5 {
		t.Fatalf("progress ObjCount = %d, want 5", p.ObjCount)
	}
	if p.Error != "" {
		t.Fatalf("progress Error = %q, want empty", p.Error)
	}
	if p.LastDoneTime == nil {
		t.Fatal("progress LastDoneTime must be set on completion")
	}
	if Running() {
		t.Fatal("guard must be released after a completed build")
	}
}

// TestBuildIndexClearResetsProgress proves Clear empties the index and a progress
// reset writes a zeroed IsDone snapshot.
func TestBuildIndexClearResetsProgress(t *testing.T) {
	setupBuildDB(t)
	origCtx, origLister := buildAdminCtx, buildLister
	t.Cleanup(func() { buildAdminCtx, buildLister = origCtx, origLister })
	buildAdminCtx = func(ctx context.Context) (context.Context, error) { return ctx, nil }
	buildLister = treeLister(sampleTree())

	if err := BuildIndex(context.Background(), []string{"/"}, nil, 20, true); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	ix := &database.Indexer{}
	if roots, _ := ix.Get(context.Background(), "/"); len(roots) == 0 {
		t.Fatal("precondition: index should be populated")
	}

	if err := Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := WriteProgress(&ext.IndexProgress{ObjCount: 0, IsDone: true}); err != nil {
		t.Fatalf("WriteProgress reset: %v", err)
	}

	if roots, _ := ix.Get(context.Background(), "/"); len(roots) != 0 {
		t.Fatalf("Clear must empty the index, still has %d", len(roots))
	}
	p, _ := Progress()
	if p.ObjCount != 0 || !p.IsDone {
		t.Fatalf("reset progress = %+v, want {ObjCount:0 IsDone:true}", p)
	}
}
