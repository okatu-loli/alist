package model

import "fmt"

// AnchorKind values for ResourceRef.
const (
	// AnchorRelPath anchors by storage-internal relative path. It is the only
	// anchor kind: the download-signing chain (server/common/sign.go,
	// server/middlewares/down.go) builds a relpath ref via ResolvePathAnchor
	// and covers its Canonical() form, so a mount rename does not invalidate
	// signatures. Directory identity for permissions/metas moved to the nodes
	// table (v4 permission stage 1) and no longer uses ResourceRef.
	AnchorRelPath uint8 = iota
)

// ResourceRef is the stable anchor of an object: the mount point may be
// renamed freely (StorageID survives) while the relative-path anchor stays
// put, so a signature over its canonical form survives a mount rename.
type ResourceRef struct {
	StorageID  uint   `json:"storage_id"`
	Anchor     string `json:"anchor"`
	AnchorKind uint8  `json:"anchor_kind"`
}

// Canonical returns the stable string form of the ref, used as the payload
// signatures cover: it identifies the object independent of the virtual
// mount path, so renaming a mount point does not invalidate signatures
// issued before the rename.
func (r ResourceRef) Canonical() string {
	return fmt.Sprintf("sig:v1:%d:%d:%s", r.StorageID, r.AnchorKind, r.Anchor)
}
