package op

import (
	"fmt"
	stdpath "path"
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/pkg/singleflight"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
)

// nodePath is a node's derived storage-internal location: the storage it lives
// in and its relative path within that storage. It deliberately excludes the
// mount point — that is resolved live in NodeVirtualPath so a mount rename is
// correct for free without touching this cache.
type nodePath struct {
	storageID uint
	relPath   string
}

// nodePathCache memoizes node_id -> (storage_id, relPath) so the parent-chain
// walk runs once per node. It is invalidated on node changes (relocation);
// because any ancestor edit shifts a whole subtree's relPath, invalidation is
// coarse (InvalidateAllNodePathCache) — node mutation is low-frequency.
var nodePathCache = cache.NewMemCache[nodePath](cache.WithShards[nodePath](4))
var nodePathG singleflight.Group[nodePath]

const nodePathCacheExpiration = time.Hour

// NodeRelPath returns the storage ID and storage-internal relative path of a
// node by walking parent_id to the storage root, cached. A corrupt parent
// chain that loops back on itself returns an error instead of spinning.
func NodeRelPath(nodeID uint) (storageID uint, relPath string, err error) {
	key := fmt.Sprint(nodeID)
	if np, ok := nodePathCache.Get(key); ok {
		return np.storageID, np.relPath, nil
	}
	np, err, _ := nodePathG.Do(key, func() (nodePath, error) {
		return computeNodeRelPath(nodeID)
	})
	if err != nil {
		return 0, "", err
	}
	nodePathCache.Set(key, np, cache.WithEx[nodePath](nodePathCacheExpiration))
	return np.storageID, np.relPath, nil
}

func computeNodeRelPath(nodeID uint) (nodePath, error) {
	visited := make(map[uint]struct{})
	var segs []string
	var storageID uint
	for id := nodeID; id != 0; {
		if _, seen := visited[id]; seen {
			return nodePath{}, errors.Errorf("node %d: cycle detected in parent chain at node %d", nodeID, id)
		}
		visited[id] = struct{}{}
		n, err := db.GetNode(id)
		if err != nil {
			return nodePath{}, err
		}
		storageID = n.StorageID
		if n.Name != "" { // the storage root contributes no segment
			segs = append(segs, n.Name)
		}
		id = n.ParentID
	}
	// segs was collected leaf -> root; reverse to root -> leaf.
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	rel := utils.FixAndCleanPath("/" + stdpath.Join(segs...))
	return nodePath{storageID: storageID, relPath: rel}, nil
}

// NodeVirtualPath derives a node's virtual (mount-relative) path: its relPath
// joined onto the storage's live mount point. The mount point is looked up
// fresh (never cached) so renaming a mount is correct without invalidation;
// a ".balanceN" suffix is stripped so balance-group members share one path.
func NodeVirtualPath(nodeID uint) (string, error) {
	storageID, relPath, err := NodeRelPath(nodeID)
	if err != nil {
		return "", err
	}
	storage, err := db.GetStorageById(storageID)
	if err != nil {
		return "", err
	}
	mountPath := utils.GetActualMountPath(storage.MountPath)
	return utils.FixAndCleanPath(stdpath.Join(mountPath, relPath)), nil
}

// InvalidateNodePathCache drops one node's cached path.
func InvalidateNodePathCache(nodeID uint) {
	nodePathCache.Del(fmt.Sprint(nodeID))
}

// InvalidateAllNodePathCache clears every cached node path. Called on
// relocation, where an ancestor edit can shift an unbounded set of descendants.
func InvalidateAllNodePathCache() {
	nodePathCache.Clear()
}
