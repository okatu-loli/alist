package op

import (
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	log "github.com/sirupsen/logrus"
)

// adoptNodelessAnchors is the intake path for permission/meta rows that carry a
// virtual Path but no node yet: rows written by a v3→v4 migration (which runs
// before any storage is live) and any hand-crafted data. It resolves each such
// fs row's Path against the now-loaded storages and, on success, builds its node
// chain and backfills node_id. Rows that still resolve to no storage (above every
// mount, or a storage not loaded yet) are left for a later load — adoption is
// idempotent and best-effort: a per-row failure is logged and skipped, never
// aborting storage load.
//
// Called after each storage finishes loading (op.LoadStorage). Scope-origin and
// system permission rows are intentionally never adopted (their truth is the
// role declaration / a domain selector, not a node).
func adoptNodelessAnchors() {
	adopted := false

	ps, err := db.GetNodelessPermissions()
	if err != nil {
		log.Errorf("node adoption: load nodeless permissions: %+v", err)
	} else {
		for i := range ps {
			p := &ps[i]
			if p.Origin == model.OriginScope || !isFS(p.ResourceType) {
				continue
			}
			ref, rerr := ResolvePathAnchor(p.Path)
			if rerr != nil {
				continue // above every mount, or storage not loaded yet
			}
			nid, nerr := db.EnsureNodeChain(ref.StorageID, ref.Anchor)
			if nerr != nil {
				log.Errorf("node adoption: ensure chain for permission %d (%s): %+v", p.ID, p.Path, nerr)
				continue
			}
			if uerr := db.SetPermissionNodeID(p.ID, nid); uerr != nil {
				log.Errorf("node adoption: backfill permission %d: %+v", p.ID, uerr)
				continue
			}
			adopted = true
		}
	}

	ms, err := db.GetNodelessMetas()
	if err != nil {
		log.Errorf("node adoption: load nodeless metas: %+v", err)
	} else {
		for i := range ms {
			m := &ms[i]
			ref, rerr := ResolvePathAnchor(m.Path)
			if rerr != nil {
				continue
			}
			nid, nerr := db.EnsureNodeChain(ref.StorageID, ref.Anchor)
			if nerr != nil {
				log.Errorf("node adoption: ensure chain for meta %d (%s): %+v", m.ID, m.Path, nerr)
				continue
			}
			if uerr := db.SetMetaNodeID(m.ID, nid); uerr != nil {
				log.Errorf("node adoption: backfill meta %d: %+v", m.ID, uerr)
				continue
			}
			adopted = true
		}
	}

	if adopted {
		invalidateAllPolicy()
		invalidateMetaIndex()
	}
}
