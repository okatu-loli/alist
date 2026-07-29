package model

import "time"

// Label is a shared, global vocabulary entry: one team-wide tag with a name and
// a display color. Unlike per-user tags, a Label is not owned by a user; anyone
// with the right permission binds it onto files (see LabelBinding). Name is
// unique so the vocabulary has no duplicates; Color is a "#RRGGBB" hex string or
// empty (validated in op, never rendered as HTML — the name is plain text).
type Label struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Name      string    `json:"name" gorm:"size:64;uniqueIndex;not null"`
	Color     string    `json:"color" gorm:"size:32"`
	CreatedAt time.Time `json:"created_at"`
}

// LabelBinding attaches a Label to a filesystem object by NodeID — the same
// stable node identity permissions and metas anchor onto. Anchoring on the node
// (not a virtual path) means a label follows its file when the file is moved or
// renamed within alist: relocation is a single nodes-table edit and this binding
// needs no update, because the node it points at is the one that moved. Deleting
// the file leaves the node (and this binding) in place, matching permission/meta
// semantics; deleting the Label cascades its bindings away (db.DeleteLabel).
//
// UNIQUE(label_id, node_id) makes a bind idempotent — the same label on the same
// node is a single row. The separate node_id index backs the per-node lookup the
// list decorator does (LabelsByNode / BindingsByNodeIDs).
type LabelBinding struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	LabelID   uint      `json:"label_id" gorm:"uniqueIndex:idx_label_binding_identity,priority:1;index"`
	NodeID    uint      `json:"node_id" gorm:"column:node_id;uniqueIndex:idx_label_binding_identity,priority:2;index"`
	CreatedAt time.Time `json:"created_at"`
}
