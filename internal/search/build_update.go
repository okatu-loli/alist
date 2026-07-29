package search

import (
	"context"
	stdpath "path"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	mapset "github.com/deckarep/golang-set/v2"
	log "github.com/sirupsen/logrus"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
)

// This file adds the search index AUTO-UPDATE path (子项目 A, wave A7). It hooks
// op.List's fresh-listing observer: every time a directory is listed with a
// fresh (non-cached) driver result, Update diffs that listing against what the
// active backend has indexed for the same parent and reconciles the difference —
// deleting vanished children, indexing new files, and recursively building new
// folders. It runs on the op.List hot path, so it FAST-BAILS on a handful of
// cheap checks whenever auto-update is not applicable (the common case).
func init() {
	op.RegisterObjsUpdateHook(Update)
}

// Update reconciles the index for parent against its fresh listing objs.
//
// RECURSION GUARD: the new-folder branch calls BuildIndex, which walks via
// fs.List → op.List and RE-FIRES this very hook on each descended directory.
// BuildIndex installs Quit for its entire duration (Running() == true), so the
// !Running() fast-bail below makes every re-fired Update return immediately —
// breaking the op.List → Update → BuildIndex → op.List cycle. It must therefore
// stay ahead of any index work.
//
// The fast-bail ordering is cheapest-decisive-first: with no active backend
// (search disabled — the overwhelmingly common state) it returns after a single
// ActiveIndexer() resolution, before touching settings, progress, or paths.
func Update(parent string, objs []model.Obj) {
	// FAST-BAIL: return immediately unless every precondition holds.
	s := ActiveIndexer()
	if s == nil || !s.Config().AutoUpdate || !setting.GetBool(conf.AutoUpdateIndex) || Running() {
		return
	}
	// Only maintain the index once a full build has completed; otherwise a
	// partial index would grow inconsistent diffs.
	progress, err := Progress()
	if err != nil || progress == nil || !progress.IsDone {
		return
	}
	if isIgnorePath(parent) {
		return
	}

	// All index reads/writes run as ADMIN on a background context — never the
	// browsing user's ctx (whose cancellation/identity must not leak into the
	// admin-owned index).
	ctx, err := buildAdminCtx(context.Background())
	if err != nil {
		log.Errorf("update index: admin ctx for %s: %+v", parent, err)
		return
	}

	old, err := s.Get(ctx, parent)
	if err != nil {
		log.Errorf("update index: get indexed children of %s: %+v", parent, err)
		return
	}

	now := mapset.NewSet[string]()
	for _, o := range objs {
		now.Add(o.GetName())
	}
	oldSet := mapset.NewSet[string]()
	for _, n := range old {
		oldSet.Add(n.Name)
	}

	// DELETE index entries whose child no longer exists in the fresh listing —
	// but SKIP a child that is itself a storage mount root: its subtree is owned
	// by that storage's own listing/build, and a parent-level diff must not tear
	// it out just because the mount does not surface as a normal child object.
	for _, n := range old {
		if now.Contains(n.Name) {
			continue
		}
		child := stdpath.Join(parent, n.Name)
		if op.HasStorage(child) {
			continue
		}
		if err := s.Del(ctx, child); err != nil {
			log.Errorf("update index: del %s: %+v", child, err)
			return
		}
	}

	// ADD newly appeared children: files are indexed in place; folders are
	// recursively built (bounded by the remaining depth budget).
	maxDepth := setting.GetInt(conf.MaxIndexDepth, 20)
	ignore := ParseIgnorePaths(setting.GetStr(conf.IgnorePaths))
	for _, o := range objs {
		if oldSet.Contains(o.GetName()) {
			continue
		}
		child := stdpath.Join(parent, o.GetName())
		if o.IsDir() {
			depth := maxDepth - strings.Count(child, "/")
			if err := BuildIndex(context.Background(), []string{child}, ignore, depth, false); err != nil {
				log.Errorf("update index: build new folder %s: %+v", child, err)
				return
			}
		} else {
			if err := s.Index(ctx, ext.SearchNode{
				Parent: parent,
				Name:   o.GetName(),
				IsDir:  false,
				Size:   o.GetSize(),
			}); err != nil {
				log.Errorf("update index: index new file %s: %+v", child, err)
				return
			}
		}
	}
}

// isIgnorePath reports whether p sits under any configured ignore_paths prefix.
// Reuses ParseIgnorePaths so the setting is parsed identically to the build path.
func isIgnorePath(p string) bool {
	for _, ig := range ParseIgnorePaths(setting.GetStr(conf.IgnorePaths)) {
		if strings.HasPrefix(p, ig) {
			return true
		}
	}
	return false
}
