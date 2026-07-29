package search

import (
	"context"
	stdpath "path"
	"strings"
	"sync/atomic"
	"time"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// This file adds the index BUILD path (子项目 A, wave A3) to the search base. It
// walks every storage as ADMIN, feeds each object to the active ext.SearchIndexer
// (through search.BatchIndex), and persists progress — under a single-flight
// concurrency guard with cooperative cancellation.
//
// v3→v4 adaptation: v4 has no fs.WalkFS, so the walk is a bounded breadth-first
// list-and-recurse over fs.List (the same shape as the index-less MVP in
// search.go). Because fs.List already merges op.GetStorageVirtualFilesByPath, the
// walk crosses mount boundaries automatically — nested storages surface as
// virtual-file children of the parent listing, with no separate merge step.

// Quit is the build concurrency guard AND the cancellation channel. A nil value
// means no build is running; BuildIndex CAS-installs a fresh buffered channel for
// the duration of a build and clears it on exit. StopIndex sends into the loaded
// channel to ask an in-flight build to abort. It is exported because the handler
// layer (StopIndex route) needs to reach it, mirroring v3.
var Quit = atomic.Pointer[chan struct{}]{}

// buildBatchSize is how many nodes accumulate before a flush to BatchIndex. A
// package var (not const) so tests can shrink it to exercise multi-batch flushes.
var buildBatchSize = 1000

// maxIndexDirsVisited caps how many directories a single build lists before it
// stops, a cycle guard mirroring the MVP's maxDirsVisited (a misbehaving driver
// that lists itself must not spin forever even within maxDepth). Package var so
// tests can tighten it; production never mutates it.
var maxIndexDirsVisited = 5_000_000

// lister lists a directory's children. The default (adminLister) is fs.List over
// an admin-carrying context; tests inject an in-memory tree so the emit/skip core
// is exercisable without real storages.
type lister func(ctx context.Context, dir string) ([]model.Obj, error)

// buildLister is the directory lister the walk uses. Kept as a package var (not a
// BuildIndex parameter) so BuildIndex keeps the fixed A3 signature while tests can
// still swap in a fake tree.
var buildLister lister = adminLister

// buildAdminCtx derives the context the walk lists under: the caller's ctx with
// op.GetAdmin() installed as ctxkey.User, so fs.List returns the RAW admin listing
// (the index stores everything; authz is deferred to query time in A4). A package
// var so tests can bypass the admin lookup (which needs a seeded DB).
var buildAdminCtx = func(ctx context.Context) (context.Context, error) {
	admin, err := op.GetAdmin()
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, ctxkey.User, admin), nil
}

// storageDisablesIndex reports whether the storage owning p has DisableIndex set.
// A package var so tests can simulate a DisableIndex storage without wiring a real
// driver. A path with no resolvable storage (e.g. the root "/") is not disabled.
var storageDisablesIndex = func(p string) bool {
	storage, _, err := op.GetStorageAndActualPath(p)
	if err != nil {
		return false
	}
	return storage.GetStorage().DisableIndex
}

func adminLister(ctx context.Context, dir string) ([]model.Obj, error) {
	return fs.List(ctx, dir, &fs.ListArgs{NoLog: true})
}

// BuildIndex walks each indexPath as admin and feeds every discovered object to
// the active ext.SearchIndexer in batches. It is guarded by Quit: only one build
// runs at a time (a concurrent call returns errs.BuildIndexIsRunning), and it
// requires an active backend (errs.SearchNotAvailable otherwise). When count is
// true it writes an ext.IndexProgress snapshot on start, on each flushed batch,
// and (via defer) a final IsDone snapshot carrying the object count and any error.
// Cancellation is cooperative: StopIndex signals Quit and the walk aborts at the
// next directory boundary, still flushing what it has collected.
func BuildIndex(ctx context.Context, indexPaths []string, ignorePaths []string, maxDepth int, count bool) error {
	quit := make(chan struct{}, 1)
	if !Quit.CompareAndSwap(nil, &quit) {
		return errs.BuildIndexIsRunning
	}

	var (
		objCount uint64
		buildErr error
	)
	defer func() {
		Quit.Store(nil)
		if count {
			now := time.Now()
			eMsg := ""
			if buildErr != nil {
				eMsg = buildErr.Error()
			}
			if err := WriteProgress(&ext.IndexProgress{
				ObjCount:     objCount,
				IsDone:       true,
				LastDoneTime: &now,
				Error:        eMsg,
			}); err != nil {
				log.Errorf("write final index progress: %+v", err)
			}
		}
	}()

	if ActiveIndexer() == nil {
		buildErr = errs.SearchNotAvailable
		return buildErr
	}

	adminCtx, err := buildAdminCtx(ctx)
	if err != nil {
		buildErr = err
		return err
	}

	log.Infof("build index for: %+v (ignore: %+v)", indexPaths, ignorePaths)
	if count {
		if err := WriteProgress(&ext.IndexProgress{ObjCount: 0, IsDone: false}); err != nil {
			log.Errorf("write initial index progress: %+v", err)
		}
	}

	batch := make([]ext.SearchNode, 0, buildBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := BatchIndex(ctx, batch); err != nil {
			return err
		}
		objCount += uint64(len(batch))
		batch = batch[:0]
		if count {
			if err := WriteProgress(&ext.IndexProgress{ObjCount: objCount, IsDone: false}); err != nil {
				log.Errorf("write index progress: %+v", err)
			}
		}
		return nil
	}
	emit := func(n ext.SearchNode) error {
		batch = append(batch, n)
		if len(batch) >= buildBatchSize {
			return flush()
		}
		return nil
	}

	for _, indexPath := range indexPaths {
		if err := buildWalk(adminCtx, buildLister, indexPath, ignorePaths, maxDepth, &quit, emit); err != nil {
			buildErr = err
			break
		}
	}
	// Flush whatever remains, whether the walk finished, aborted, or errored.
	if err := flush(); err != nil && buildErr == nil {
		buildErr = err
	}
	return buildErr
}

type indexQueueItem struct {
	path  string
	depth int
}

// buildWalk lists indexPath's subtree breadth-first and emits every object as an
// ext.SearchNode{Parent: <dir>, Name, IsDir, Size}. The start dir itself is never
// emitted (its children are — so for indexPath "/" the mount roots are indexed as
// children of "/", not "/" itself). It crosses mount boundaries for free because
// the lister (fs.List) merges virtual files. A child under an ignorePaths prefix
// or owned by a DisableIndex storage is neither emitted nor descended into. The
// walk terminates on the maxDepth bound and the maxIndexDirsVisited cycle guard,
// and aborts promptly when quit is signalled or ctx is cancelled (flushing is the
// caller's job). A directory that fails to list is skipped, not fatal.
func buildWalk(ctx context.Context, list lister, indexPath string, ignorePaths []string, maxDepth int, quit *chan struct{}, emit func(ext.SearchNode) error) error {
	visited := 0
	queue := []indexQueueItem{{path: indexPath, depth: 0}}
	for len(queue) > 0 {
		if stopRequested(quit) {
			log.Debugf("build index walk for %s received quit", indexPath)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if visited >= maxIndexDirsVisited {
			log.Warnf("build index walk for %s hit the visited cap", indexPath)
			return nil
		}
		item := queue[0]
		queue = queue[1:]
		visited++

		objs, err := list(ctx, item.path)
		if err != nil {
			// A single unreadable/erroring directory must not abort the whole
			// build; skip it and keep walking its siblings.
			continue
		}
		for _, o := range objs {
			childPath := stdpath.Join(item.path, o.GetName())
			if skipBuildPath(childPath, ignorePaths) {
				continue
			}
			if err := emit(ext.SearchNode{
				Parent: item.path,
				Name:   o.GetName(),
				IsDir:  o.IsDir(),
				Size:   o.GetSize(),
			}); err != nil {
				return err
			}
			if o.IsDir() && item.depth+1 <= maxDepth {
				queue = append(queue, indexQueueItem{path: childPath, depth: item.depth + 1})
			}
		}
	}
	return nil
}

// skipBuildPath reports whether p must be excluded from the index: it sits under
// an ignore prefix, or its owning storage has DisableIndex set. Empty ignore
// entries are ignored so a blank line in the setting never swallows the whole tree.
func skipBuildPath(p string, ignorePaths []string) bool {
	for _, ig := range ignorePaths {
		if ig != "" && strings.HasPrefix(p, ig) {
			return true
		}
	}
	return storageDisablesIndex(p)
}

// stopRequested non-blockingly reports whether a stop was signalled on quit.
func stopRequested(quit *chan struct{}) bool {
	if quit == nil {
		return false
	}
	select {
	case <-*quit:
		return true
	default:
		return false
	}
}

// StopIndex asks an in-flight build to abort by sending into the guard channel.
// It is a no-op when no build is running. The send is non-blocking (the channel
// is buffered size 1); a second Stop while one is already queued is dropped.
func StopIndex() {
	quit := Quit.Load()
	if quit == nil {
		return
	}
	select {
	case *quit <- struct{}{}:
	default:
	}
}

// Progress reads the persisted build-progress snapshot from the index_progress
// setting. An empty/unset value decodes to the zero IndexProgress (no build yet).
func Progress() (*ext.IndexProgress, error) {
	p := setting.GetStr(conf.IndexProgress)
	var progress ext.IndexProgress
	if strings.TrimSpace(p) == "" {
		return &progress, nil
	}
	err := utils.Json.UnmarshalFromString(p, &progress)
	return &progress, err
}

// WriteProgress persists progress as the index_progress setting (SINGLE + PRIVATE
// internal state, the same shape v4 seeds it with).
func WriteProgress(progress *ext.IndexProgress) error {
	p, err := utils.Json.MarshalToString(progress)
	if err != nil {
		return err
	}
	return op.SaveSettingItem(&model.SettingItem{
		Key:   conf.IndexProgress,
		Value: p,
		Type:  conf.TypeText,
		Group: model.SINGLE,
		Flag:  model.PRIVATE,
	})
}

// ParseIgnorePaths splits the ignore_paths setting (newline- or comma-separated,
// per the design) into trimmed, non-empty virtual-path prefixes.
func ParseIgnorePaths(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
