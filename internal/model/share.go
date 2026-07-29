package model

import "time"

// Share is a public, unguessable link that delegates read access to one
// filesystem object (a file or a directory subtree). A logged-in creator with
// read permission on the path mints one; anyone holding the Token may then
// access it without logging in, optionally gated by a password and/or an
// expiry.
//
// Like labels, permissions and metas, a Share anchors on NodeID — the stable
// node identity — rather than a virtual path, so the share follows its target
// when the file is moved or renamed inside alist: relocation is a single
// nodes-table edit and this row needs no update, because the node it points at
// is the one that moved (op.NodeVirtualPath re-derives the current path at
// resolve time).
//
// Token is stored in plaintext because it is itself the lookup key and the
// bearer credential (like a signed URL), not a password; it carries crypto/rand
// entropy (random.String(32)) and is uniquely indexed. PasswordHash is the
// salted, one-way HashToken output (empty means no password). ExpiresAt nil
// means the share never expires; Disabled revokes it. The MVP deliberately
// omits access-count limits (YAGNI — a later migration adds them if needed).
type Share struct {
	ID           uint       `json:"id" gorm:"primaryKey"`
	Token        string     `json:"token" gorm:"size:64;uniqueIndex;not null"`
	NodeID       uint       `json:"node_id" gorm:"index"`
	CreatorID    uint       `json:"creator_id" gorm:"index"`
	PasswordHash string     `json:"-" gorm:"size:128"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
}
