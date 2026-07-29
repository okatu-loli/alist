package handles

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/search"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

const (
	// searchTimeout hard-bounds a single search so a huge or slow subtree
	// cannot hold a request open indefinitely; the walk returns whatever it has
	// collected so far with truncated=true when it fires.
	searchTimeout = 30 * time.Second
	// searchDefaultPerPage is the page size used when the caller omits per_page.
	searchDefaultPerPage = 50
	// searchMaxPerPage caps per_page so a caller cannot force one giant page.
	searchMaxPerPage = 200
	// maxAuthzPages bounds how many index pages the index path will fetch while
	// accumulating PerPage readable survivors (see fsSearchIndex). The index is
	// built as admin over the full path space, so a query can hit a long run of
	// nodes the requesting user cannot read (denied subtrees, hidden or
	// password-protected files) before reaching a readable one. Without a cap, an
	// index that is entirely unreadable to the caller would page to exhaustion —
	// or, for a backend whose result set is effectively unbounded, spin forever.
	// 20 pages at the max per_page of 200 caps one query at 4000 hits inspected
	// (and at the default per_page of 50, 1000 — matching the BFS result-limit
	// default); on hitting the cap the loop stops and reports truncated=true.
	maxAuthzPages = 20
)

// SearchReq is the /fs/search query (GET, form-bound like /fs/list). Parent is
// the virtual directory to search under; Keyword is the case-insensitive
// substring to match (blank is rejected); Page/PerPage drive in-memory paging
// over the already-bounded result set.
type SearchReq struct {
	model.PageReq
	Parent  string `json:"parent" form:"parent"`
	Keyword string `json:"keyword" form:"keyword"`
	// Password is threaded into the per-hit re-authorization on the INDEX path
	// (op.AuthzRequest.Password), mirroring v3's SearchReq.Password. When absent,
	// op.Authorize denies password-protected paths unless the caller holds the
	// access-without-password permission — the correct safe default, never
	// bypassed. It is inert on the BFS fallback (search.Search filters per
	// directory as it descends).
	Password string `json:"password" form:"password"`
}

// searchItem is the response DTO for one hit, kept separate from
// search.Result so the wire contract (snake_case) is pinned at the handler
// boundary independent of the core type.
type searchItem struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Size   int64  `json:"size"`
}

// FsSearch recursively searches file names under a directory subtree.
//
// The gate is an ActRead Authorize on the search ROOT: no read on the directory
// you ask to search is a 403. That entry check is unchanged on BOTH paths.
//
// When a real index backend is active (search.ActiveIndexer() != nil), the
// INDEX path serves the query: it pages the persistent index and — the security
// invariant — RE-AUTHORIZES every hit for the requesting user before returning
// it (see fsSearchIndex). The index is built as admin over the full path space,
// so an unfiltered hit would leak files the caller cannot read; per-hit
// op.Authorize (ActRead, Password) drops the unreadable, hidden, and
// password-protected ones, and the loop pages the index until PerPage survivors
// accumulate (or a page cap fires). If the backend races to inactive between the
// ActiveIndexer() check and the query, the index path yields and FsSearch falls
// back to the BFS below rather than erroring.
//
// With no active backend the BFS path serves the query (fsSearchBFS): the
// per-child ActRead + hide-meta filtering that keeps unreadable files out of the
// result set happens inside search.Search (op.FilterListing, applied per
// directory as the walk descends), so a caller who can read the root but not
// some subtree under it never sees that subtree's files. The walk is
// hard-bounded (ctx timeout + a directory-visit cap + a result limit inside the
// search package). Matching is a plain case-insensitive substring test, so a
// keyword full of regex metacharacters is inert (no ReDoS).
// @Summary Search files
// @Description Recursively searches file/directory names (case-insensitive substring match) under parent. Requires ActRead on parent; every hit is re-authorized (ActRead, per-hit password) before being returned, so a subtree the caller cannot read is never leaked, on both the index-backed and BFS-fallback paths.
// @Tags search
// @Security ApiKeyAuth
// @Produce json
// @Param parent query string true "directory to search under (joined with the caller's base path)"
// @Param keyword query string true "case-insensitive substring to match"
// @Param password query string false "password for password-protected paths, threaded into per-hit re-authorization"
// @Param page query int false "page number (default 1; BFS fallback only)"
// @Param per_page query int false "items per page (default 50, max 200)"
// @Success 200 {object} common.Resp[gin.H] "data: {content: []searchItem, total: int, truncated: bool}"
// @Failure 400 {object} common.Resp[any] "empty keyword"
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "no read permission on parent, or builtin.search disabled"
// @Failure 500 {object} common.Resp[any]
// @Router /v4/fs/search [get]
func FsSearch(c *gin.Context) {
	var req SearchReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	keyword := strings.TrimSpace(req.Keyword)
	if keyword == "" {
		common.ErrorResp(c, errors.New("search keyword must not be empty"), 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Parent)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// ActRead on the search root: you must be able to read the directory you ask
	// to search. Per-hit filtering below is what protects nested subtrees on
	// BOTH the index and the BFS path.
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: reqPath}); err != nil {
		common.ErrorResp(c, err, 403)
		return
	}

	page := req.Page
	if page < 1 {
		page = 1
	}
	perPage := req.PerPage
	if perPage < 1 {
		perPage = searchDefaultPerPage
	}
	if perPage > searchMaxPerPage {
		perPage = searchMaxPerPage
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), searchTimeout)
	defer cancel()

	// Index path when a backend is active; otherwise (and on a race to inactive)
	// the always-available BFS.
	if search.ActiveIndexer() != nil {
		if fsSearchIndex(c, ctx, user, reqPath, keyword, req.Password, perPage) {
			return
		}
	}
	fsSearchBFS(c, ctx, user, reqPath, keyword, page, perPage)
}

// fsSearchIndex serves the query from the active index, enforcing the security
// invariant: the index stored every node as admin, so EVERY hit is re-authorized
// for the requesting user before it can be returned. It pages the index and, per
// hit, calls op.Authorize(ActRead, {Path, Password}); a non-nil result (denied /
// hidden / wrong-or-missing password) DROPS the hit. The loop pages past dropped
// hits until PerPage survivors accumulate, the index is exhausted, or maxAuthzPages
// is reached (truncated=true).
//
// Returns true when it has written a response (success or a hard error) and the
// caller must stop. Returns false ONLY on the SearchNotAvailable race — the
// backend went inactive between ActiveIndexer() and the query — so the caller
// falls back to the BFS.
//
// total is len(survivors) for this single page (v3 semantics): a global survivor
// count would require re-authorizing the whole index. The request Page is not
// used here — the index path always returns the first PerPage readable survivors;
// cross-page navigation over a per-user-filtered index is a known limitation.
func fsSearchIndex(c *gin.Context, ctx context.Context, user *model.User, reqPath, keyword, password string, perPage int) bool {
	survivors := make([]ext.SearchNode, 0, perPage)
	indexExhausted := false
	for page := 1; len(survivors) < perPage; page++ {
		if page > maxAuthzPages {
			// Stopped on the page cap with the index not proven exhausted. Whether
			// this is "truncated" is decided below from the READABLE survivor set,
			// not from raw-index exhaustion: an all-unreadable index that fills the
			// page cap with zero survivors must NOT report truncated=true, or the
			// flag becomes an existence oracle ("unreadable matching content exists
			// under this root") that the BFS path never leaks.
			break
		}
		nodes, _, err := search.IndexSearch(ctx, ext.SearchReq{
			Parent:   reqPath,
			Keywords: keyword,
			Scope:    0,
			Page:     page,
			PerPage:  perPage,
		})
		if err != nil {
			if errors.Is(err, errs.SearchNotAvailable) {
				// Race: the backend was released after ActiveIndexer() said it was
				// live. Yield so FsSearch falls back to the BFS rather than erroring.
				return false
			}
			common.ErrorResp(c, err, 500)
			return true
		}
		if len(nodes) == 0 {
			indexExhausted = true
			break
		}
		for _, n := range nodes {
			// SECURITY INVARIANT: re-authorize the stored (admin-built) hit for the
			// requesting user. A non-nil error — unreadable scope, a hide-meta match
			// without see-hides, or a password-protected path with a missing/wrong
			// password — DROPS the hit; it is never returned unfiltered.
			hitPath := path.Join(n.Parent, n.Name)
			if op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: hitPath, Password: password}) != nil {
				continue
			}
			survivors = append(survivors, n)
			if len(survivors) >= perPage {
				break
			}
		}
	}

	// truncated reflects the READABLE result set, mirroring BFS semantics — not
	// raw-index exhaustion. It is true only when a full page of survivors was
	// returned (there are more results the caller CAN see) and the index was not
	// proven exhausted. When zero survivors accumulated (e.g. every hit was
	// unreadable and the page cap fired), truncated is false: there is nothing the
	// caller can see to be truncated, so the flag cannot leak the existence of
	// unreadable matching content.
	truncated := len(survivors) >= perPage && !indexExhausted

	items := make([]searchItem, 0, len(survivors))
	for _, n := range survivors {
		items = append(items, searchItem{Parent: n.Parent, Name: n.Name, IsDir: n.IsDir, Size: n.Size})
	}
	common.SuccessResp(c, gin.H{
		"content":   items,
		"total":     len(items),
		"truncated": truncated,
	})
	return true
}

// fsSearchBFS is the index-less fallback: the on-the-fly bounded directory walk
// (search.Search), unchanged from the MVP. It applies the per-child ActRead +
// hide-meta filter inside search.Search (op.FilterListing) as it descends, reads
// the admin-tunable depth/result bounds, and pages the bounded result set in
// memory.
func fsSearchBFS(c *gin.Context, ctx context.Context, user *model.User, reqPath, keyword string, page, perPage int) {
	// Read the admin-tunable bounds. GetInt returns 0 when a key is unset/blank,
	// which search.Search treats as "use the package default" (20 / 1000) — so a
	// present-but-zero setting still yields the safe default rather than an
	// unbounded walk. A positive value tightens or loosens the bound.
	maxDepth := setting.GetInt(conf.SearchMaxDepth, 0)
	limit := setting.GetInt(conf.SearchResultLimit, 0)
	results, truncated, err := search.Search(ctx, user, search.Options{
		Parent:   reqPath,
		Keyword:  keyword,
		MaxDepth: maxDepth,
		Limit:    limit,
	})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	items := make([]searchItem, 0, len(results))
	for _, r := range results {
		items = append(items, searchItem{Parent: r.Parent, Name: r.Name, IsDir: r.IsDir, Size: r.Size})
	}
	total := len(items)
	common.SuccessResp(c, gin.H{
		"content":   pageSearchItems(items, page, perPage),
		"total":     total,
		"truncated": truncated,
	})
}

// pageSearchItems returns the page-th slice (1-based) of at most perPage items,
// or an empty (non-nil) slice when the page is past the end — so the JSON
// content is always [] rather than null.
func pageSearchItems(items []searchItem, page, perPage int) []searchItem {
	start := (page - 1) * perPage
	if start >= len(items) {
		return []searchItem{}
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}
