package model

import "time"

// AppPassword is a per-client, protocol-only credential a user mints for
// clients that cannot do 2FA (WebDAV/FTP and friends — design W3). It is
// scoped to exactly one user (UserID) and carries its own independent salt,
// so it is hashed with the same family as the primary password (model.HashPwd)
// but never shares a salt with it or with any other app password row —
// compromising one row's hash never helps against another.
//
// The plaintext is generated once at creation time (op.CreateAppPassword) and
// returned to the caller exactly once; only Hash+Salt are ever persisted, so
// both are excluded from JSON (`json:"-"`) — a leaked API response or log line
// can never reconstruct or replay the credential.
//
// LastUsed is updated best-effort on a successful op.VerifyAppPassword so the
// account page can show "last used" without that update ever gating whether
// the credential itself is accepted.
type AppPassword struct {
	ID        uint       `json:"id" gorm:"primaryKey"`
	UserID    uint       `json:"user_id" gorm:"index"`
	Name      string     `json:"name"`
	Hash      string     `json:"-"`
	Salt      string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}
