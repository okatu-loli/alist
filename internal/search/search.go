// Package search implements the file-search MVP: an on-the-fly, breadth-first
// walk of a directory subtree that collects children whose name matches a
// keyword. It builds no index. Every directory listing is passed through
// op.FilterListing with THAT directory's path, which is the sole per-child
// ActRead + hide-meta enforcement point — so a search result can never contain
// a file the requesting user cannot read or one hidden by meta rules (the head
// invariant). Matching is a case-insensitive substring test only; a user regexp
// is never compiled, so there is no ReDoS surface.
package search

import (
	"context"
	stdpath "path"
	"strings"

	"github.com/pkg/errors"

	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// Result is one search hit: a child object the requesting user may see. The
// object's virtual path is Parent + "/" + Name.
type Result struct {
	Parent string
	Name   string
	IsDir  bool
	Size   int64
}

// Options parameterizes one Search call. Parent is the virtual directory to
// search under (the caller must already have authorized read on it); Keyword is
// the case-insensitive substring to match; MaxDepth bounds recursion depth and
// Limit bounds the number of results (both fall back to package defaults when
// non-positive).
type Options struct {
	Parent   string
	Keyword  string
	MaxDepth int
	Limit    int
}

const (
	// defaultMaxDepth bounds how many directory levels below Parent the walk
	// descends when Options.MaxDepth is not set.
	defaultMaxDepth = 20
	// defaultLimit caps the number of results when Options.Limit is not set,
	// so a keyword with no explicit limit still cannot return unbounded output.
	defaultLimit = 1000
)

// maxDirsVisited caps how many directories a single Search lists before it
// stops and reports truncated=true, protecting against huge or slow subtrees.
// It is a package var (not const) so tests can tighten it to exercise the
// bound; production never mutates it.
var maxDirsVisited = 5000

type queueItem struct {
	path  string
	depth int
}

// Search walks opts.Parent's subtree breadth-first, collecting children whose
// name contains opts.Keyword (case-insensitive substring). Each directory's
// listing passes through op.FilterListing with that directory's own path, so
// unreadable or hidden children are dropped before matching — results never
// leak. The walk is bounded by ctx (timeout/cancel), maxDirsVisited, MaxDepth
// and Limit; hitting any bound returns what was collected so far with
// truncated=true. A directory that fails to list is skipped (one bad subtree
// must not abort the whole search). An empty keyword is rejected.
func Search(ctx context.Context, user *model.User, opts Options) (results []Result, truncated bool, err error) {
	kw := strings.ToLower(strings.TrimSpace(opts.Keyword))
	if kw == "" {
		return nil, false, errors.New("search keyword must not be empty")
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	visited := 0
	queue := []queueItem{{path: opts.Parent, depth: 0}}
	for len(queue) > 0 {
		// Stop promptly on timeout/cancel, returning what we already have.
		select {
		case <-ctx.Done():
			return results, true, nil
		default:
		}
		if visited >= maxDirsVisited {
			return results, true, nil
		}
		item := queue[0]
		queue = queue[1:]
		visited++

		objs, listErr := fs.List(ctx, item.path, &fs.ListArgs{NoLog: true})
		if listErr != nil {
			// A single unreadable/erroring directory must not abort the whole
			// search; skip it and keep walking its siblings.
			continue
		}
		// THE leak guard: filter this directory's children using THIS
		// directory's path, so every hit is a child the user may actually read.
		objs = op.FilterListing(ctx, user, item.path, objs)
		for _, o := range objs {
			if strings.Contains(strings.ToLower(o.GetName()), kw) {
				results = append(results, Result{
					Parent: item.path,
					Name:   o.GetName(),
					IsDir:  o.IsDir(),
					Size:   o.GetSize(),
				})
				if len(results) >= limit {
					return results, true, nil
				}
			}
			if o.IsDir() && item.depth+1 <= maxDepth {
				queue = append(queue, queueItem{
					path:  stdpath.Join(item.path, o.GetName()),
					depth: item.depth + 1,
				})
			}
		}
	}
	return results, false, nil
}
