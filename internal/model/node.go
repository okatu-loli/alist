package model

// Node is alist's inode layer: the lazily-created identity of a directory (or
// file) that something anchors onto. A permission or meta references a Node by
// ID instead of carrying a virtual Path, so moving/renaming the object is a
// single row edit here (parent_id/name) that every anchored row follows for
// free. The virtual path is a derived quantity: walk parent_id to the storage
// root, then join the live mount point (see op.NodeVirtualPath).
//
// Lazy: a node exists only because something anchored onto it (permission/meta
// creation, migration adoption); the table's size is roughly the number of
// anchored directories, never a mirror of the remote tree. The storage root is
// the single row (storage_id, parent_id=0, name=""); a relative path of "/"
// resolves to it. Nodes are never deleted when the underlying object is removed
// so deny rules do not silently vanish; recreating a same-named object walks
// back to the same node and its old rules apply again.
type Node struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// StorageID names the storage the node lives in. For balance groups it is
	// the primary member's ID (same normalization ResolvePathAnchor applies).
	StorageID uint `json:"storage_id" gorm:"uniqueIndex:idx_node_identity,priority:1"`
	// ParentID is the parent node's ID; 0 marks the storage root.
	ParentID uint `json:"parent_id" gorm:"uniqueIndex:idx_node_identity,priority:2"`
	// Name is a single path segment; the storage root has an empty Name.
	Name string `json:"name" gorm:"size:255;uniqueIndex:idx_node_identity,priority:3"`
	// DriverObjectID is reserved for a future cloud-drive reconciliation hook
	// (drivers with stable object IDs). Stage 1 never writes or reads it.
	DriverObjectID string `json:"driver_object_id"`
}
