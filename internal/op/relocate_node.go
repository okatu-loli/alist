package op

import (
	stdpath "path"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// RegisterNodeRelocation installs the v4 node-based relocate handler, replacing
// the legacy per-anchor meta/permission rewrite hooks. A move/rename is now a
// single nodes-table edit: find the node at the source relpath and repoint it at
// the new parent + name. The whole anchored subtree — every permission AND meta
// beneath it — follows for free, because their paths are derived by walking
// parent_id (NodeRelPath). Removals do not touch nodes: a deny rule must not
// silently vanish when its object is deleted, and recreating the same path walks
// back to the same node so old rules reapply. Registered once at bootstrap;
// tests call it directly.
func RegisterNodeRelocation() {
	RegisterObjRelocateHook(func(storageID uint, kind RelocateKind, oldRel, newRel string, isDir bool) {
		if kind != RelocateMove && kind != RelocateRename {
			return // RelocateRemove: nodes are never deleted (spec §1)
		}
		// Serialize relocations per-storage to avoid database lock contention
		// (same rationale as the legacy handlers this replaces).
		mu := getRelocateMutex(storageID)
		mu.Lock()
		defer mu.Unlock()

		err := withRelocateRetry(func() error {
			return relocateNode(storageID, oldRel, newRel)
		})
		if err != nil {
			log.Errorf("node relocation failed (storage %d, %s -> %s): %+v",
				storageID, oldRel, newRel, err)
			return
		}
		// The moved node shifts a whole subtree's derived paths; every
		// path-derived cache is now stale.
		InvalidateAllNodePathCache()
		invalidateAllPolicy()
		invalidateMetaIndex()
	})
}

// relocateNode repoints the node at (storageID, oldRel) onto newRel. When no
// node exists at oldRel — nothing in this subtree was ever anchored — it is a
// no-op. The destination parent chain is created on demand so a move into a
// previously-unanchored directory still lands correctly.
func relocateNode(storageID uint, oldRel, newRel string) error {
	nodeID, found, err := db.GetNodeByChain(storageID, oldRel)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	newRel = utils.FixAndCleanPath(newRel)
	newParentID, err := db.EnsureNodeChain(storageID, stdpath.Dir(newRel))
	if err != nil {
		return err
	}
	return db.UpdateNodeParentName(nodeID, newParentID, stdpath.Base(newRel))
}
