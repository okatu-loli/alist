package op

import (
	stdpath "path"
	"sync"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
)

// metaIndex is a process-wide map from a meta's effective virtual path to its
// row, replacing the former per-path metaCache. Node-anchored rows (NodeID != 0)
// contribute their DERIVED path (NodeVirtualPath), so a move/rename that repoints
// the node is reflected here after a single invalidation; the returned meta's
// Path field is overwritten with the derived value so callers that compare
// m.Path (ResolveEffectiveMeta) see the live path, not the stale stored column.
// A node whose storage was deleted derives nothing and is dropped from the index
// (spec §7 — it stops participating). Unanchored rows (NodeID 0: above-mount
// virtual paths, not-yet-adopted migration rows) key off their stored Path.
//
// Built lazily and dropped wholesale on any meta/node/storage change. The table
// is small (one row per admin-configured directory) so a full rebuild is cheap.
var (
	metaIndexMu   sync.RWMutex
	metaIndexData map[string]*model.Meta
	metaIndexOK   bool
)

func invalidateMetaIndex() {
	metaIndexMu.Lock()
	metaIndexData, metaIndexOK = nil, false
	metaIndexMu.Unlock()
}

func loadMetaIndex() (map[string]*model.Meta, error) {
	metaIndexMu.RLock()
	if metaIndexOK {
		d := metaIndexData
		metaIndexMu.RUnlock()
		return d, nil
	}
	metaIndexMu.RUnlock()

	metaIndexMu.Lock()
	defer metaIndexMu.Unlock()
	if metaIndexOK { // another goroutine built it while we waited for the lock
		return metaIndexData, nil
	}
	metas, err := db.GetAllMetas()
	if err != nil {
		return nil, err
	}
	idx := make(map[string]*model.Meta, len(metas))
	for i := range metas {
		m := &metas[i]
		var key string
		if m.NodeID != 0 {
			vp, derr := NodeVirtualPath(m.NodeID)
			if derr != nil {
				continue // storage gone: drop from matching (spec §7)
			}
			m.Path = vp // callers compare m.Path; keep it live, not the stored value
			key = vp
		} else {
			key = utils.FixAndCleanPath(m.Path)
		}
		idx[key] = m
	}
	metaIndexData, metaIndexOK = idx, true
	return idx, nil
}

func GetNearestMeta(path string) (*model.Meta, error) {
	return getNearestMeta(utils.FixAndCleanPath(path))
}
func getNearestMeta(path string) (*model.Meta, error) {
	meta, err := getMetaByPath(path)
	if err == nil {
		return meta, nil
	}
	if errors.Cause(err) != errs.MetaNotFound {
		return nil, err
	}
	if path == "/" {
		return nil, errs.MetaNotFound
	}
	return getNearestMeta(stdpath.Dir(path))
}

func GetMetaByPath(path string) (*model.Meta, error) {
	return getMetaByPath(utils.FixAndCleanPath(path))
}

// getMetaByPath resolves an already-cleaned virtual path to its meta via the
// process-wide index. A miss is errs.MetaNotFound (the negative-cache setting
// the former metaCache honored is moot: an index miss is already O(1)).
func getMetaByPath(path string) (*model.Meta, error) {
	idx, err := loadMetaIndex()
	if err != nil {
		return nil, err
	}
	if m, ok := idx[path]; ok {
		return m, nil
	}
	return nil, errs.MetaNotFound
}

// anchorMeta resolves u.Path to a storage node and backfills NodeID, from which
// the meta's live path is henceforth derived (the meta index overrides the
// stored Path with the derived one). Unlike a permission, a meta keeps its
// stored Path even when node-anchored: Path is the meta's v3 identity and its
// unique key, so it cannot be blanked. A path above every mount point stays
// unanchored (NodeID 0) and matches on Path. EnsureNodeChain failure is
// surfaced so a create/update does not silently persist a row that should have
// followed its node but did not.
func anchorMeta(u *model.Meta) error {
	if ref, err := ResolvePathAnchor(u.Path); err == nil {
		nid, nerr := db.EnsureNodeChain(ref.StorageID, ref.Anchor)
		if nerr != nil {
			return nerr
		}
		u.NodeID = nid
	} else {
		// ResolvePathAnchor's only error is errs.StorageNotFound (a virtual path
		// above every mount): such metas legitimately stay path-anchored.
		u.NodeID = 0
	}
	return nil
}

// metaLivePath returns the effective virtual path a meta resolves at: the
// node-derived path when anchored, else the stored Path. It is exactly the key
// loadMetaIndex builds on, and the identity used for logical uniqueness.
func metaLivePath(m *model.Meta) (string, error) {
	if m.NodeID != 0 {
		return NodeVirtualPath(m.NodeID)
	}
	return utils.FixAndCleanPath(m.Path), nil
}

// ensureMetaPathFree rejects creating/updating a meta onto a live path already
// owned by a DIFFERENT meta row. This is the v4 replacement for the DB
// unique(path) key: identity is the live derived path, so a stale stored Path
// left behind by a move no longer blocks reclaiming that path, while two metas at
// the same live directory are still refused. Must run after anchorMeta so NodeID
// (and thus the derived live path) is settled.
func ensureMetaPathFree(m *model.Meta) error {
	live, err := metaLivePath(m)
	if err != nil {
		return err
	}
	idx, err := loadMetaIndex()
	if err != nil {
		return err
	}
	if other, ok := idx[live]; ok && other.ID != m.ID {
		return errors.Errorf("a meta already exists at %s", live)
	}
	return nil
}

func DeleteMetaById(id uint) error {
	if err := db.DeleteMetaById(id); err != nil {
		return err
	}
	invalidateMetaIndex()
	return nil
}

func UpdateMeta(u *model.Meta) error {
	u.Path = utils.FixAndCleanPath(u.Path)
	if err := anchorMeta(u); err != nil {
		return err
	}
	if err := ensureMetaPathFree(u); err != nil {
		return err
	}
	if err := db.UpdateMeta(u); err != nil {
		return err
	}
	invalidateMetaIndex()
	return nil
}

func CreateMeta(u *model.Meta) error {
	u.Path = utils.FixAndCleanPath(u.Path)
	if err := anchorMeta(u); err != nil {
		return err
	}
	if err := ensureMetaPathFree(u); err != nil {
		return err
	}
	if err := db.CreateMeta(u); err != nil {
		return err
	}
	invalidateMetaIndex()
	return nil
}

// GetMetaById (single-row read, also the fetch step of an update) derives a
// node-anchored row's live virtual path for display, keeping the stored Path on
// derivation failure so a mutation round-trip is not re-homed to "/".
func GetMetaById(id uint) (*model.Meta, error) {
	m, err := db.GetMetaById(id)
	if err != nil {
		return nil, err
	}
	if m.NodeID != 0 {
		if vp, verr := NodeVirtualPath(m.NodeID); verr == nil {
			m.Path = vp
		}
	}
	return m, nil
}

// GetMetas is the admin LIST endpoint: like GetPermissions it derives each
// node-anchored row's live virtual path for display (spec §9), keeping
// storage-deleted rows listed with a blank Path for manual cleanup (spec §7).
func GetMetas(pageIndex, pageSize int) (metas []model.Meta, count int64, err error) {
	metas, count, err = db.GetMetas(pageIndex, pageSize)
	if err != nil {
		return nil, 0, err
	}
	for i := range metas {
		if metas[i].NodeID == 0 {
			continue // unanchored: stored Path is its truth
		}
		if vp, verr := NodeVirtualPath(metas[i].NodeID); verr == nil {
			metas[i].Path = vp
		} else {
			metas[i].Path = ""
		}
	}
	return metas, count, nil
}
