package handles

import (
	"context"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/search"
	"github.com/alist-org/alist/v3/internal/search/database"
	"github.com/gin-gonic/gin"
)

// Index-path security tests (子项目 A, wave A4). The head invariant is that the
// index is built as ADMIN over the full path space, so every hit FsSearch pulls
// from the index MUST be re-authorized for the requesting user before it is
// returned. These drive the real op.Authorize decision (RBAC deny, hide-meta,
// password meta) end to end through the handler, with the real database backend
// active over the package's shared in-memory sqlite (x_search_nodes seeded
// directly — no storage needed, since the index path never touches fs.List).

// ensureDBIndexExt registers the builtin.search.database manageable extension
// entry once (idempotent across tests): ActiveIndexer's enabled-gate reads it.
func ensureDBIndexExt(t *testing.T) {
	t.Helper()
	if _, ok := extension.DefaultRegistry.Get(database.ExtensionID); ok {
		return
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    database.ExtensionID,
		Name:  map[string]string{"en": "Search: Database index"},
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}); err != nil {
		t.Fatalf("register db search extension: %v", err)
	}
}

// activateDatabaseIndex makes search.ActiveIndexer() resolve the real database
// backend for the duration of one test, then releases it on cleanup so the BFS
// tests (and any later index test) see a clean slate.
func activateDatabaseIndex(t *testing.T) {
	t.Helper()
	ensureDBIndexExt(t)
	search.RegisterBackend(database.SearcherName, database.ExtensionID, &database.Indexer{})
	if err := search.Init(database.SearcherName); err != nil {
		t.Fatalf("activate database index: %v", err)
	}
	if search.ActiveIndexer() == nil {
		t.Fatal("precondition: database index must be active after Init")
	}
	t.Cleanup(func() { _ = search.Init("none") })
}

// seedIndexNodes writes nodes straight into x_search_nodes and clears the whole
// table on cleanup (tests run sequentially, so a table-wide clear is safe and
// keeps one test's seed from bleeding into the next).
func seedIndexNodes(t *testing.T, nodes []model.SearchNode) {
	t.Helper()
	if err := db.ClearSearchNodes(); err != nil {
		t.Fatalf("pre-clear index: %v", err)
	}
	if len(nodes) > 0 {
		if err := db.BatchCreateSearchNodes(&nodes); err != nil {
			t.Fatalf("seed index nodes: %v", err)
		}
	}
	t.Cleanup(func() { _ = db.ClearSearchNodes() })
}

// idxReader builds a non-admin user with a recursive read scope over root (bit
// mask 0 -> ActRead only, since ActRead is implicit for every scope-covered
// path). extraBit adds one legacy permission bit on top (e.g. see_hides).
func idxReader(t *testing.T, root string, extraBit int32) *model.User {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	r := createScopedRole(t, fmt.Sprintf("idx-read-%d", n), []model.PermissionEntry{
		{Path: root, Permission: extraBit},
	})
	return &model.User{ID: 90000 + uint(n), Username: fmt.Sprintf("idxread%d", n), Role: model.Roles{int(r.ID)}}
}

// idxDeniedUnder builds a non-admin user with a recursive read scope over root
// but an explicit recursive ActRead DENY on denyPath, so op.Authorize really
// denies that subtree (mirrors setupSearchFixture's deny wiring).
func idxDeniedUnder(t *testing.T, root, denyPath string) *model.User {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	r := createScopedRole(t, fmt.Sprintf("idx-deny-%d", n), []model.PermissionEntry{
		{Path: root, Permission: 0},
	})
	deny := &model.Permission{
		Name: fmt.Sprintf("idx-deny-perm-%d", n), Path: denyPath, Recursive: true,
		Effect: model.EffectDeny, Actions: model.Actions{model.ActRead},
	}
	if err := op.CreatePermission(deny); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(deny.ID) })
	if err := op.SetRolePermissions(r.ID, append(mustScopePermIDs(t, r.ID), deny.ID)); err != nil {
		t.Fatal(err)
	}
	return &model.User{ID: 92000 + uint(n), Username: fmt.Sprintf("idxdeny%d", n), Role: model.Roles{int(r.ID)}}
}

func idxRoot(t *testing.T) string {
	return fmt.Sprintf("/idx%d", atomic.AddInt64(&labelFixtureSeq, 1))
}

func mustIndexMeta(t *testing.T, m *model.Meta) {
	t.Helper()
	if err := op.CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })
}

// TestFsSearchIndexUnreadableFiltered: a user whose RBAC scope denies root/secret
// gets ZERO hits for the file indexed there, while a user with read on the whole
// root gets it. The real op.Authorize deny (recursive EffectDeny ActRead) is what
// drops the hit.
func TestFsSearchIndexUnreadableFiltered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	activateDatabaseIndex(t)
	root := idxRoot(t)
	seedIndexNodes(t, []model.SearchNode{
		{Parent: root + "/pub", Name: "afile.txt", Size: 1},
		{Parent: root + "/secret", Name: "afile.txt", Size: 2},
	})

	denied := idxDeniedUnder(t, root, root+"/secret")
	reader := idxReader(t, root, 0)

	// Denied user: only the /pub hit, never the /secret one.
	w := driveSearch(denied, url.Values{"parent": {root}, "keyword": {"afile"}, "per_page": {"200"}})
	if got := respCode(t, w); got != 200 {
		t.Fatalf("index search must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseSearchResp(t, w)
	if len(resp.Content) != 1 {
		t.Fatalf("denied user must get exactly the readable hit, got %d: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Parent != root+"/pub" {
		t.Fatalf("denied user leaked a non-/pub hit: %+v", resp.Content[0])
	}

	// Reader with read on the whole root: both hits.
	w = driveSearch(reader, url.Values{"parent": {root}, "keyword": {"afile"}, "per_page": {"200"}})
	resp = parseSearchResp(t, w)
	if len(resp.Content) != 2 {
		t.Fatalf("full-read user must see both hits, got %d: %+v", len(resp.Content), resp.Content)
	}
}

// TestFsSearchIndexHideMetaFiltered: a file matching a hide pattern is dropped for
// a user lacking see_hides and returned for one who has it. Real op.Authorize
// resolves the hide meta and enforces ActSeeHides.
func TestFsSearchIndexHideMetaFiltered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	activateDatabaseIndex(t)
	root := idxRoot(t)
	seedIndexNodes(t, []model.SearchNode{
		{Parent: root + "/pub", Name: "hidden_note.txt", Size: 1},
	})
	mustIndexMeta(t, &model.Meta{Path: root + "/pub", Hide: "hidden.*"})

	noHides := idxReader(t, root, 0)      // read only
	seesHides := idxReader(t, root, 1<<0) // + ActSeeHides (legacy bit 0)

	w := driveSearch(noHides, url.Values{"parent": {root}, "keyword": {"hidden"}, "per_page": {"200"}})
	if resp := parseSearchResp(t, w); len(resp.Content) != 0 {
		t.Fatalf("hidden file must be dropped without see_hides, got %+v", resp.Content)
	}

	w = driveSearch(seesHides, url.Values{"parent": {root}, "keyword": {"hidden"}, "per_page": {"200"}})
	if resp := parseSearchResp(t, w); len(resp.Content) != 1 {
		t.Fatalf("see_hides user must get the hidden file, got %+v", resp.Content)
	}
}

// TestFsSearchIndexPasswordFiltered: a hit under a password-protected meta is
// dropped when req.password is empty/wrong and returned when correct. Real
// op.Authorize enforces the password (the user lacks access_without_password).
func TestFsSearchIndexPasswordFiltered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	activateDatabaseIndex(t)
	root := idxRoot(t)
	seedIndexNodes(t, []model.SearchNode{
		{Parent: root + "/locked", Name: "report.txt", Size: 1},
	})
	mustIndexMeta(t, &model.Meta{Path: root + "/locked", Password: "pw", PSub: true})

	reader := idxReader(t, root, 0)

	// No password -> dropped.
	w := driveSearch(reader, url.Values{"parent": {root}, "keyword": {"report"}, "per_page": {"200"}})
	if resp := parseSearchResp(t, w); len(resp.Content) != 0 {
		t.Fatalf("password-protected hit must be dropped with no password, got %+v", resp.Content)
	}
	// Wrong password -> dropped.
	w = driveSearch(reader, url.Values{"parent": {root}, "keyword": {"report"}, "password": {"nope"}, "per_page": {"200"}})
	if resp := parseSearchResp(t, w); len(resp.Content) != 0 {
		t.Fatalf("password-protected hit must be dropped with wrong password, got %+v", resp.Content)
	}
	// Correct password -> returned.
	w = driveSearch(reader, url.Values{"parent": {root}, "keyword": {"report"}, "password": {"pw"}, "per_page": {"200"}})
	if resp := parseSearchResp(t, w); len(resp.Content) != 1 {
		t.Fatalf("correct password must surface the hit, got %+v", resp.Content)
	}
}

// TestFsSearchIndexPagingAccumulates: a full first page of UNREADABLE hits
// followed by readable ones must not short-circuit to an empty result — the loop
// pages past the unreadable run and still returns up to per_page readable
// survivors. Names are chosen so the (name-asc) index order puts the unreadable
// /secret hits before the readable /pub ones.
func TestFsSearchIndexPagingAccumulates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	activateDatabaseIndex(t)
	root := idxRoot(t)
	seedIndexNodes(t, []model.SearchNode{
		{Parent: root + "/secret", Name: "kw_a0.txt"},
		{Parent: root + "/secret", Name: "kw_a1.txt"},
		{Parent: root + "/secret", Name: "kw_a2.txt"},
		{Parent: root + "/pub", Name: "kw_z0.txt"},
		{Parent: root + "/pub", Name: "kw_z1.txt"},
	})
	denied := idxDeniedUnder(t, root, root+"/secret")

	// per_page=2: page 1 of the index is entirely unreadable /secret hits, so the
	// loop must fetch further pages to collect the two readable /pub survivors.
	w := driveSearch(denied, url.Values{"parent": {root}, "keyword": {"kw"}, "per_page": {"2"}})
	resp := parseSearchResp(t, w)
	if len(resp.Content) != 2 {
		t.Fatalf("must accumulate 2 readable survivors past the unreadable page, got %d: %+v", len(resp.Content), resp.Content)
	}
	for _, it := range resp.Content {
		if it.Parent != root+"/pub" {
			t.Fatalf("unreadable /secret hit leaked: %+v", it)
		}
	}
}

// TestFsSearchIndexLoopCap: an index whose matching hits are ALL unreadable and
// number more than maxAuthzPages*per_page must return empty and stop on the page
// cap WITHOUT reporting truncated=true. A truncated=true here would be an
// existence oracle: it would tell a low-privilege caller that unreadable matching
// content exists under a root they can read, a distinction the BFS path never
// leaks (BFS returns truncated=false, total=0 for the same query). Zero readable
// survivors => nothing to truncate => truncated MUST be false.
func TestFsSearchIndexLoopCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	activateDatabaseIndex(t)
	root := idxRoot(t)
	// per_page=2 * maxAuthzPages=20 = 40 hits inspected; seed 45 so the cap fires
	// before the index is exhausted.
	nodes := make([]model.SearchNode, 0, 45)
	for i := 0; i < 45; i++ {
		nodes = append(nodes, model.SearchNode{Parent: root + "/secret", Name: fmt.Sprintf("cap_%03d.txt", i)})
	}
	seedIndexNodes(t, nodes)
	denied := idxDeniedUnder(t, root, root+"/secret")

	w := driveSearch(denied, url.Values{"parent": {root}, "keyword": {"cap"}, "per_page": {"2"}})
	resp := parseSearchResp(t, w)
	if len(resp.Content) != 0 {
		t.Fatalf("all-unreadable index must yield no survivors, got %+v", resp.Content)
	}
	if resp.Truncated {
		t.Fatal("all-unreadable page-cap case must report truncated=false (no oracle), got truncated=true")
	}
}

// TestFsSearchIndexAllUnreadableNoOracle is the security assertion for this fix:
// an index full of matching-but-unreadable hits that exceeds the page cap must
// return exactly {total:0, content:[], truncated:false} — indistinguishable from
// a root with no matches at all. Any truncated=true would betray that unreadable
// matching content exists under this root.
func TestFsSearchIndexAllUnreadableNoOracle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	activateDatabaseIndex(t)
	root := idxRoot(t)
	// Seed well past maxAuthzPages*per_page so the cap fires with zero survivors.
	nodes := make([]model.SearchNode, 0, 60)
	for i := 0; i < 60; i++ {
		nodes = append(nodes, model.SearchNode{Parent: root + "/secret", Name: fmt.Sprintf("leak_%03d.txt", i)})
	}
	seedIndexNodes(t, nodes)
	denied := idxDeniedUnder(t, root, root+"/secret")

	w := driveSearch(denied, url.Values{"parent": {root}, "keyword": {"leak"}, "per_page": {"2"}})
	if got := respCode(t, w); got != 200 {
		t.Fatalf("index search must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseSearchResp(t, w)
	if len(resp.Content) != 0 {
		t.Fatalf("oracle: all-unreadable index must return no content, got %+v", resp.Content)
	}
	if resp.Total != 0 {
		t.Fatalf("oracle: all-unreadable index must return total=0, got %d", resp.Total)
	}
	if resp.Truncated {
		t.Fatal("oracle: all-unreadable index must return truncated=false (matches BFS total:0,truncated:false), got truncated=true")
	}
}

// TestFsSearchIndexGenuinelyCappedReadable pins that the legitimate "more readable
// results exist" signal still works: with MORE than per_page READABLE hits, the
// index path returns a full page of per_page survivors and truncated=true. This is
// the non-oracle case — every survivor is content the caller can actually see, so
// reporting "there is more" leaks nothing.
func TestFsSearchIndexGenuinelyCappedReadable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	activateDatabaseIndex(t)
	root := idxRoot(t)
	// 5 readable hits under /pub with per_page=2 => a full first page of 2 survivors
	// and the index not exhausted => truncated=true.
	nodes := make([]model.SearchNode, 0, 5)
	for i := 0; i < 5; i++ {
		nodes = append(nodes, model.SearchNode{Parent: root + "/pub", Name: fmt.Sprintf("more_%03d.txt", i)})
	}
	seedIndexNodes(t, nodes)
	reader := idxReader(t, root, 0)

	w := driveSearch(reader, url.Values{"parent": {root}, "keyword": {"more"}, "per_page": {"2"}})
	if got := respCode(t, w); got != 200 {
		t.Fatalf("index search must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseSearchResp(t, w)
	if len(resp.Content) != 2 {
		t.Fatalf("must return a full page of 2 readable survivors, got %d: %+v", len(resp.Content), resp.Content)
	}
	if !resp.Truncated {
		t.Fatal("more readable results than per_page must report truncated=true")
	}
}

// TestFsSearchIndexBranchSelection pins that the index path is taken when a
// backend is active and the BFS when it is not. Against a real Local storage
// holding vis/alpha.txt: with the (empty) index active, a search returns 0 (the
// index has no nodes); with the index released, the same search falls to the BFS
// and finds the file.
func TestFsSearchIndexBranchSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)

	// BFS (no active indexer): the on-disk file is found.
	w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {"alpha"}, "per_page": {"200"}})
	var sawBFS bool
	for _, it := range parseSearchResp(t, w).Content {
		if it.Name == "alpha.txt" {
			sawBFS = true
		}
	}
	if !sawBFS {
		t.Fatal("precondition: BFS must find vis/alpha.txt when no index is active")
	}

	// Activate the (empty) index: the index path serves the query and finds
	// nothing, proving FsSearch consulted the index rather than the BFS.
	activateDatabaseIndex(t)
	seedIndexNodes(t, nil)
	w = driveSearch(u, url.Values{"parent": {mount}, "keyword": {"alpha"}, "per_page": {"200"}})
	if resp := parseSearchResp(t, w); len(resp.Content) != 0 {
		t.Fatalf("active empty index must serve the query (0 hits), got %+v", resp.Content)
	}
}

// raceIndexer is an active backend whose Search always reports
// errs.SearchNotAvailable — simulating the backend going inactive between
// ActiveIndexer() and the query (a race). FsSearch must fall back to the BFS.
type raceIndexer struct{}

func (raceIndexer) Manifest() ext.Manifest {
	return ext.Manifest{ID: "builtin.search.racetest", Kinds: []ext.Kind{ext.KindSearchIndexer}}
}
func (raceIndexer) Init(ctx context.Context, host ext.Host) error { return nil }
func (raceIndexer) Shutdown(ctx context.Context) error            { return nil }
func (raceIndexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: "racetest"}
}
func (raceIndexer) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	return nil, 0, errs.SearchNotAvailable
}
func (raceIndexer) Index(ctx context.Context, n ext.SearchNode) error        { return nil }
func (raceIndexer) BatchIndex(ctx context.Context, n []ext.SearchNode) error { return nil }
func (raceIndexer) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	return nil, nil
}
func (raceIndexer) Del(ctx context.Context, prefix string) error { return nil }
func (raceIndexer) Clear(ctx context.Context) error              { return nil }

// TestFsSearchIndexRaceFallsBackToBFS: with a backend active whose IndexSearch
// races to SearchNotAvailable, FsSearch must fall back to the BFS (not error),
// serving the on-disk file from the real storage.
func TestFsSearchIndexRaceFallsBackToBFS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)

	if _, ok := extension.DefaultRegistry.Get("builtin.search.racetest"); !ok {
		if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, raceIndexer{}.Manifest()); err != nil {
			t.Fatalf("register race extension: %v", err)
		}
	}
	search.RegisterBackend("racetest", "builtin.search.racetest", raceIndexer{})
	if err := search.Init("racetest"); err != nil {
		t.Fatalf("activate race backend: %v", err)
	}
	t.Cleanup(func() { _ = search.Init("none") })
	if search.ActiveIndexer() == nil {
		t.Fatal("precondition: race backend must be active")
	}

	w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {"alpha"}, "per_page": {"200"}})
	if got := respCode(t, w); got != 200 {
		t.Fatalf("race fallback must be 200 (BFS), got %d: %s", got, w.Body.String())
	}
	var found bool
	for _, it := range parseSearchResp(t, w).Content {
		if it.Name == "alpha.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("SearchNotAvailable race must fall back to BFS and find vis/alpha.txt")
	}
}
