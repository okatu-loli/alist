package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// File-search handler tests (plan Wave 2). The load-bearing parts are the
// ActRead gate on the search ROOT (no read on the root you ask to search is a
// 403) and — the head invariant — that a result set driven end to end through
// the handler never contains a file the caller cannot read, even when the
// search root itself is readable but a subtree under it is denied. Pagination
// is pinned separately. All cases run against a real Local storage + scoped
// role, mirroring labels_test.go / offline_download_test.go.

// searchPageItems is the number of "item*" files setupSearchFixture plants
// under <mount>/vis/many, used by the pagination case.
const searchPageItems = 5

// setupSearchFixture builds an isolated Local storage the returned non-admin
// user can read broadly (scope on the whole mount) EXCEPT for a read-denied
// <mount>/sec subtree. Disk layout:
//
//	vis/alpha.txt          (readable, matches "alpha")
//	vis/deep/beta.txt      (readable, matches "beta")
//	vis/many/item{0..N}.txt(readable, match "item" — pagination)
//	sec/alpha_sec.txt      (read-DENIED, also matches "alpha")
//
// A search under the mount root must surface vis/alpha.txt but never
// sec/alpha_sec.txt (FilterListing drops the denied sec dir before its subtree
// is ever walked). A search rooted AT <mount>/sec must 403.
func setupSearchFixture(t *testing.T) (*model.User, string) {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	mount := fmt.Sprintf("/sr%d", n)

	dir := t.TempDir()
	for _, d := range []string{"vis/deep", "vis/many", "sec"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writes := map[string]string{
		"vis/alpha.txt":     "a",
		"vis/deep/beta.txt": "b",
		"sec/alpha_sec.txt": "s",
	}
	for i := 0; i < searchPageItems; i++ {
		writes[filepath.Join("vis", "many", "item"+strconv.Itoa(i)+".txt")] = "i"
	}
	for rel, body := range writes {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	// Role: broad scope on the whole mount (ActRead is implicit for every
	// scope-covered path), then a manual recursive ActRead DENY on <mount>/sec
	// so the caller can read the root but not that subtree.
	r := createScopedRole(t, fmt.Sprintf("sr-role-%d", n), []model.PermissionEntry{
		{Path: mount, Permission: 1 << 3},
	})
	deny := &model.Permission{
		Name: fmt.Sprintf("sr-deny-%d", n), Path: mount + "/sec", Recursive: true,
		Effect: model.EffectDeny, Actions: model.Actions{model.ActRead},
	}
	if err := op.CreatePermission(deny); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(deny.ID) })
	if err := op.SetRolePermissions(r.ID, append(mustScopePermIDs(t, r.ID), deny.ID)); err != nil {
		t.Fatal(err)
	}

	user := &model.User{ID: 80000 + uint(n), Username: fmt.Sprintf("sruser%d", n), Role: model.Roles{int(r.ID)}}
	return user, mount
}

// driveSearch runs FsSearch as a GET with query params and the caller in
// context, mirroring how /fs/search is mounted.
func driveSearch(u *model.User, q url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?"+q.Encode(), nil)
	c.Set(ctxkey.User, u)
	FsSearch(c)
	return w
}

// searchResp is the pinned response envelope: content is snake_case items, plus
// total and truncated (the frontend Wave 3 contract).
type searchResp struct {
	Content []struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
		IsDir  bool   `json:"is_dir"`
		Size   int64  `json:"size"`
	} `json:"content"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

func parseSearchResp(t *testing.T, w *httptest.ResponseRecorder) searchResp {
	t.Helper()
	var env struct {
		Data searchResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse search resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// TestFsSearchAuthz pins the ActRead gate on the search root: searching a
// read-denied root is 403, and searching a readable root is 200 with the hit
// present.
func TestFsSearchAuthz(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)

	t.Run("no-read-root-403", func(t *testing.T) {
		w := driveSearch(u, url.Values{"parent": {mount + "/sec"}, "keyword": {"alpha"}})
		if got := respCode(t, w); got != 403 {
			t.Fatalf("search under a read-denied root must be 403, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("readable-root-200-hits", func(t *testing.T) {
		w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {"alpha"}})
		if got := respCode(t, w); got != 200 {
			t.Fatalf("search under a readable root must be 200, got %d: %s", got, w.Body.String())
		}
		resp := parseSearchResp(t, w)
		var found bool
		for _, it := range resp.Content {
			if it.Name == "alpha.txt" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected alpha.txt in results, got %+v", resp.Content)
		}
	})
}

// TestFsSearchEmptyKeyword pins that a blank keyword is a 400, not an unbounded
// walk that matches everything.
func TestFsSearchEmptyKeyword(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)
	for _, kw := range []string{"", "   "} {
		w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {kw}})
		if got := respCode(t, w); got != 400 {
			t.Fatalf("empty keyword %q must be 400, got %d: %s", kw, got, w.Body.String())
		}
	}
}

// TestFsSearchPagination pins in-memory paging: total is the full hit count
// regardless of page, and each page carries at most per_page items.
func TestFsSearchPagination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)

	// Page 1 of 2-per-page over searchPageItems "item*" hits.
	w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {"item"}, "page": {"1"}, "per_page": {"2"}})
	if got := respCode(t, w); got != 200 {
		t.Fatalf("paginated search must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseSearchResp(t, w)
	if resp.Total != searchPageItems {
		t.Fatalf("total must be the full hit count %d, got %d", searchPageItems, resp.Total)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("page 1 of per_page=2 must carry 2 items, got %d: %+v", len(resp.Content), resp.Content)
	}

	// Last page (page 3) carries the remainder (5 -> 2,2,1).
	w = driveSearch(u, url.Values{"parent": {mount}, "keyword": {"item"}, "page": {"3"}, "per_page": {"2"}})
	resp = parseSearchResp(t, w)
	if resp.Total != searchPageItems {
		t.Fatalf("total on last page must still be %d, got %d", searchPageItems, resp.Total)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("page 3 of per_page=2 over 5 hits must carry 1 item, got %d: %+v", len(resp.Content), resp.Content)
	}
}

// setSearchSetting seeds a search settings key for one test and removes it on
// cleanup, so the handler reads a real admin-tunable value via setting.GetInt.
func setSearchSetting(t *testing.T, key, val string) {
	t.Helper()
	if err := op.SaveSettingItem(&model.SettingItem{
		Key: key, Value: val, Type: conf.TypeNumber, Group: model.INDEX,
	}); err != nil {
		t.Fatalf("seed %s=%q: %v", key, val, err)
	}
	t.Cleanup(func() { _ = op.DeleteSettingItemByKey(key) })
}

// TestFsSearchReadsSettings pins the W6 wiring: FsSearch reads search_result_limit
// and search_max_depth via setting.GetInt and passes them into search.Options, so
// an admin can tune the index-less walk. A tight result_limit truncates the hit
// set; a shallow max_depth stops the walk from descending into deep subtrees.
func TestFsSearchReadsSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)

	t.Run("result-limit-truncates", func(t *testing.T) {
		// searchPageItems (5) files match "item"; cap the result set at 2.
		setSearchSetting(t, conf.SearchResultLimit, "2")
		w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {"item"}, "per_page": {"200"}})
		if got := respCode(t, w); got != 200 {
			t.Fatalf("search must be 200, got %d: %s", got, w.Body.String())
		}
		resp := parseSearchResp(t, w)
		if resp.Total != 2 {
			t.Fatalf("search_result_limit=2 must cap total at 2, got %d", resp.Total)
		}
		if !resp.Truncated {
			t.Fatalf("hitting the result limit must report truncated=true")
		}
	})

	t.Run("max-depth-bounds-descent", func(t *testing.T) {
		// vis/deep/beta.txt sits two levels below the root. depth=1 stops the walk
		// before it descends into vis/deep, so beta.txt is never found; the default
		// (or a deeper setting) does find it.
		setSearchSetting(t, conf.SearchMaxDepth, "1")
		w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {"beta"}, "per_page": {"200"}})
		if got := respCode(t, w); got != 200 {
			t.Fatalf("search must be 200, got %d: %s", got, w.Body.String())
		}
		if resp := parseSearchResp(t, w); resp.Total != 0 {
			t.Fatalf("max_depth=1 must not reach vis/deep/beta.txt, got total=%d: %+v", resp.Total, resp.Content)
		}

		// Loosen the depth: the same keyword now resolves the deep file.
		setSearchSetting(t, conf.SearchMaxDepth, "5")
		w = driveSearch(u, url.Values{"parent": {mount}, "keyword": {"beta"}, "per_page": {"200"}})
		if resp := parseSearchResp(t, w); resp.Total != 1 {
			t.Fatalf("max_depth=5 must find vis/deep/beta.txt, got total=%d: %+v", resp.Total, resp.Content)
		}
	})
}

// TestFsSearchNoLeak is the head-invariant regression driven through the
// handler: a caller who can read the search root but NOT the <mount>/sec
// subtree must never see sec/alpha_sec.txt, even though its name matches. The
// readable sibling vis/alpha.txt must still appear.
func TestFsSearchNoLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)

	// Ask for many pages so the whole (bounded) result set is inspected.
	w := driveSearch(u, url.Values{"parent": {mount}, "keyword": {"alpha"}, "per_page": {"200"}})
	if got := respCode(t, w); got != 200 {
		t.Fatalf("search must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseSearchResp(t, w)
	var sawVisible bool
	for _, it := range resp.Content {
		if strings.Contains(it.Name, "_sec") || strings.Contains(it.Parent, "/sec") {
			t.Fatalf("read-denied file leaked into results: %+v (all=%+v)", it, resp.Content)
		}
		if it.Name == "alpha.txt" {
			sawVisible = true
		}
	}
	if !sawVisible {
		t.Fatalf("readable vis/alpha.txt must still be found, got %+v", resp.Content)
	}
}
