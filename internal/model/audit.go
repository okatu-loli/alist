package model

import "time"

// AuditLog is one security-relevant event: an authorization decision made by
// op.Authorize, or a discrete event (login, logout, session eviction,
// role/permission mutation). Rows are written asynchronously by
// internal/audit's buffered writer and pruned by retention (rollout Step 12).
type AuditLog struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// ActorUID is the acting user's id at event time; 0 for anonymous actors
	// (e.g. a failed login for an unknown username). Actor snapshots the
	// username because uids can be reused after deletion.
	ActorUID uint   `json:"actor_uid" gorm:"index"`
	Actor    string `json:"actor" gorm:"size:256"`
	// Action is a dotted verb: "fs.<act>" / "system.<domain>.<act>" for
	// Authorize decisions, "auth.login" / "session.evict" / "role.update" …
	// for events.
	Action string `json:"action" gorm:"size:64;index"`
	// TargetRef is what the action addressed: an fs path, a system domain, or
	// an entity reference like "user:42".
	TargetRef string `json:"target_ref" gorm:"size:1024"`
	// Protocol is the entrypoint ("http" today; s3/webdav/ftp when those
	// surfaces return as plugins).
	Protocol string `json:"protocol" gorm:"size:16"`
	IP       string `json:"ip" gorm:"size:64"`
	// Result is "allow" / "deny" for decisions, "success" / "failure" for events.
	Result string    `json:"result" gorm:"size:16;index"`
	Ts     time.Time `json:"ts" gorm:"index"`
}
