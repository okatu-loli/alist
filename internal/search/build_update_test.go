package search

import (
	"context"
	"os"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/search/database"
)

// --- recording fake indexer (bail tests) ---

// recIndexer records every write it receives so a bail test can assert that
// Update performed NO index mutations. Config().AutoUpdate is configurable so
// the "backend not auto-updatable" bail is exercisable.
type recIndexer struct {
	name       string
	autoUpdate bool

	mu       sync.Mutex
	getCalls int
	indexed  []ext.SearchNode
	deleted  []string
	batched  int
}

func (r *recIndexer) Manifest() ext.Manifest                     { return ext.Manifest{ID: r.name} }
func (r *recIndexer) Init(ctx context.Context, h ext.Host) error { return nil }
func (r *recIndexer) Shutdown(ctx context.Context) error         { return nil }
func (r *recIndexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: r.name, AutoUpdate: r.autoUpdate}
}
func (r *recIndexer) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	return nil, 0, nil
}
func (r *recIndexer) Index(ctx context.Context, node ext.SearchNode) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.indexed = append(r.indexed, node)
	return nil
}
func (r *recIndexer) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batched++
	return nil
}
func (r *recIndexer) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getCalls++
	return nil, nil
}
func (r *recIndexer) Del(ctx context.Context, prefix string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, prefix)
	return nil
}
func (r *recIndexer) Clear(ctx context.Context) error { return nil }

func (r *recIndexer) touchedIndex() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.indexed) > 0 || len(r.deleted) > 0 || r.batched > 0
}

// regRec registers fake as an active backend (enabled owning extension +
// searcherName mapping + Init), mirroring regBackend but for an arbitrary
// instance. Cleanup deregisters both and resets the base.
func regRec(t *testing.T, fake *recIndexer) {
	t.Helper()
	extID := "builtin.search." + fake.name
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    extID,
		Name:  map[string]string{"en": fake.name},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}); err != nil {
		t.Fatalf("register owning extension: %v", err)
	}
	RegisterBackend(fake.name, extID, fake)
	if err := Init(fake.name); err != nil {
		t.Fatalf("Init(%s): %v", fake.name, err)
	}
	t.Cleanup(func() {
		_ = Init("none")
		RegisterBackend(fake.name, extID, nil)
		extension.DefaultRegistry.Deregister(extID)
	})
}

// setSetting writes a setting to the shared DB and restores it on cleanup, so
// Update's real setting reads (auto_update_index, ignore_paths) can be driven.
func setSetting(t *testing.T, key, val string) {
	t.Helper()
	orig := ""
	if item, err := op.GetSettingItemByKey(key); err == nil && item != nil {
		orig = item.Value
	}
	save := func(v string) {
		if err := op.SaveSettingItem(&model.SettingItem{
			Key: key, Value: v, Type: conf.TypeString, Group: model.SINGLE,
		}); err != nil {
			t.Fatalf("save setting %s=%q: %v", key, v, err)
		}
	}
	save(val)
	t.Cleanup(func() { save(orig) })
}

// enableAutoUpdate puts the base into the state where Update proceeds: the given
// backend active, auto_update_index=true, ignore_paths empty, progress IsDone,
// and an identity admin ctx (no seeded admin user needed).
func enableAutoUpdate(t *testing.T) {
	t.Helper()
	setSetting(t, conf.AutoUpdateIndex, "true")
	setSetting(t, conf.IgnorePaths, "")
	if err := WriteProgress(&ext.IndexProgress{ObjCount: 1, IsDone: true}); err != nil {
		t.Fatalf("seed progress: %v", err)
	}
	origCtx := buildAdminCtx
	t.Cleanup(func() { buildAdminCtx = origCtx })
	buildAdminCtx = func(ctx context.Context) (context.Context, error) { return ctx, nil }
}

// --- fast-bail tests ---

// TestUpdateBailsAutoUpdateSettingOff: with auto_update_index=false, Update never
// touches the backend even though every other precondition holds.
func TestUpdateBailsAutoUpdateSettingOff(t *testing.T) {
	fake := &recIndexer{name: "bail_setting", autoUpdate: true}
	regRec(t, fake)
	enableAutoUpdate(t)
	setSetting(t, conf.AutoUpdateIndex, "false") // flip the one condition off

	Update("/p", []model.Obj{ffile("new.txt", 1)})
	if fake.touchedIndex() {
		t.Fatal("auto_update_index=false must bail before any index write")
	}
}

// TestUpdateBailsBackendNotAutoUpdatable: an active backend whose Config reports
// AutoUpdate=false must bail.
func TestUpdateBailsBackendNotAutoUpdatable(t *testing.T) {
	fake := &recIndexer{name: "bail_noupd", autoUpdate: false}
	regRec(t, fake)
	enableAutoUpdate(t)

	Update("/p", []model.Obj{ffile("new.txt", 1)})
	if fake.touchedIndex() {
		t.Fatal("backend AutoUpdate=false must bail before any index write")
	}
}

// TestUpdateBailsWhileRunning: while a build holds Quit (Running()==true) Update
// must bail — this is the recursion guard that breaks op.List→Update→BuildIndex→
// op.List.
func TestUpdateBailsWhileRunning(t *testing.T) {
	fake := &recIndexer{name: "bail_running", autoUpdate: true}
	regRec(t, fake)
	enableAutoUpdate(t)

	held := make(chan struct{}, 1)
	if !Quit.CompareAndSwap(nil, &held) {
		t.Fatal("could not install build guard")
	}
	t.Cleanup(func() { Quit.Store(nil) })

	Update("/p", []model.Obj{ffile("new.txt", 1)})
	if fake.touchedIndex() {
		t.Fatal("Running()==true must bail (recursion guard) before any index write")
	}
}

// TestUpdateBailsProgressNotDone: with a build never finished (IsDone=false)
// Update must bail so it never diffs against a partial index.
func TestUpdateBailsProgressNotDone(t *testing.T) {
	fake := &recIndexer{name: "bail_progress", autoUpdate: true}
	regRec(t, fake)
	enableAutoUpdate(t)
	if err := WriteProgress(&ext.IndexProgress{ObjCount: 0, IsDone: false}); err != nil {
		t.Fatalf("seed progress: %v", err)
	}

	Update("/p", []model.Obj{ffile("new.txt", 1)})
	if fake.touchedIndex() {
		t.Fatal("progress not IsDone must bail before any index write")
	}
}

// TestUpdateBailsIgnorePath: a parent under an ignore_paths prefix must bail.
func TestUpdateBailsIgnorePath(t *testing.T) {
	fake := &recIndexer{name: "bail_ignore", autoUpdate: true}
	regRec(t, fake)
	enableAutoUpdate(t)
	setSetting(t, conf.IgnorePaths, "/p")

	Update("/p/sub", []model.Obj{ffile("new.txt", 1)})
	if fake.touchedIndex() {
		t.Fatal("isIgnorePath(parent) must bail before any index write")
	}
}

// --- diff tests over the real database backend ---

// TestUpdateDiffAddsAndDeletes seeds the index for P with {a, b} (files), then
// Update(P, [a, c]) must delete b, index c, and leave a untouched.
func TestUpdateDiffAddsAndDeletes(t *testing.T) {
	setupBuildDB(t)
	enableAutoUpdate(t)
	ix := &database.Indexer{}
	ctx := context.Background()
	const P = "/diff"

	for _, n := range []string{"a", "b"} {
		if err := ix.Index(ctx, ext.SearchNode{Parent: P, Name: n, Size: 1}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	Update(P, []model.Obj{ffile("a", 1), ffile("c", 1)})

	got, err := ix.Get(ctx, P)
	if err != nil {
		t.Fatalf("Get(%s): %v", P, err)
	}
	names := map[string]bool{}
	for _, n := range got {
		names[n.Name] = true
	}
	if names["b"] {
		t.Error("b vanished from the listing and must be deleted from the index")
	}
	if !names["c"] {
		t.Error("c is new and must be indexed")
	}
	if !names["a"] {
		t.Error("a is unchanged and must stay indexed")
	}
	if len(got) != 2 {
		t.Fatalf("index children of %s = %d (%+v), want 2 {a,c}", P, len(got), got)
	}
}

// TestUpdateNewFolderRecurses: Update(P, [d(dir)]) with d owning children (via the
// injected build lister) must recursively index d's subtree.
func TestUpdateNewFolderRecurses(t *testing.T) {
	setupBuildDB(t)
	enableAutoUpdate(t)
	ix := &database.Indexer{}
	ctx := context.Background()
	const P = "/recurse"

	// Inject a fake tree so BuildIndex (invoked by the new-folder branch) walks
	// d's children without a real storage.
	origLister := buildLister
	t.Cleanup(func() { buildLister = origLister })
	buildLister = treeLister(map[string][]model.Obj{
		P + "/d":     {ffile("dchild.txt", 5), fdir("sub")},
		P + "/d/sub": {ffile("deep.txt", 6)},
	})

	Update(P, []model.Obj{fdir("d")})

	kids, err := ix.Get(ctx, P+"/d")
	if err != nil || len(kids) != 2 {
		t.Fatalf("Get(%s/d) = %+v (%v), want [dchild.txt sub]", P, kids, err)
	}
	deep, err := ix.Get(ctx, P+"/d/sub")
	if err != nil || len(deep) != 1 || deep[0].Name != "deep.txt" {
		t.Fatalf("Get(%s/d/sub) = %+v (%v), want [deep.txt]", P, deep, err)
	}
}

// TestUpdateSkipsMountRootDeletion: a child that IS a storage mount root must NOT
// be deleted from the index even when it is absent from the fresh listing — its
// subtree is owned by that storage, not by the parent-level diff.
func TestUpdateSkipsMountRootDeletion(t *testing.T) {
	setupBuildDB(t)
	enableAutoUpdate(t)
	ix := &database.Indexer{}
	ctx := context.Background()

	// Mount a real Local storage so op.HasStorage("/mskip/mnt") is true.
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid, err := op.CreateStorage(ctx, model.Storage{
		Driver: "Local", MountPath: "/mskip/mnt",
		Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatalf("mount storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(ctx, sid) })
	if !op.HasStorage("/mskip/mnt") {
		t.Fatal("precondition: /mskip/mnt must be a mounted storage")
	}

	const P = "/mskip"
	for _, n := range []string{"mnt", "gone"} {
		if err := ix.Index(ctx, ext.SearchNode{Parent: P, Name: n, IsDir: n == "mnt"}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	// Fresh listing shows neither child. gone must be deleted; mnt must survive.
	Update(P, nil)

	got, err := ix.Get(ctx, P)
	if err != nil {
		t.Fatalf("Get(%s): %v", P, err)
	}
	names := map[string]bool{}
	for _, n := range got {
		names[n.Name] = true
	}
	if names["gone"] {
		t.Error("non-mount child absent from listing must be deleted")
	}
	if !names["mnt"] {
		t.Error("mount-root child must be preserved even when absent from the listing")
	}
}

// TestUpdateHookWired proves the auto-update Update is registered as an
// op.ObjsUpdateHook, so a fresh op.List reaches it through HandleObjsUpdateHook.
// (The op.List→hook edge itself is covered structurally in package op; here we
// prove the search side registered the listener at init.)
func TestUpdateHookWired(t *testing.T) {
	fake := &recIndexer{name: "wired", autoUpdate: true}
	regRec(t, fake)
	enableAutoUpdate(t)

	// Firing the registry with a fresh listing must drive Update, which — with
	// every precondition satisfied — reaches the backend (indexes the new file).
	op.HandleObjsUpdateHook("/wired", []model.Obj{ffile("n.txt", 1)})
	if !fake.touchedIndex() {
		t.Fatal("HandleObjsUpdateHook must reach the registered Update and index the new file")
	}
}
