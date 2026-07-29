package model

import "time"

// RecoveryCode is one single-use TOTP backup code (Phase C: internal TOTP
// second factor). A confirmed TOTP enrollment mints a fresh batch of 10; each
// row stores only a salted hash of the code (the same primitive family as
// AppPassword — model.HashPwd under an independent per-row salt), never the
// plaintext, so a leaked DB row cannot be replayed by reading it. The
// plaintext is shown to the user exactly once, at generation time.
//
// UsedAt is set the first (and only) time the code is successfully consumed
// (op.ConsumeRecoveryCode); a used row is kept (not deleted) so "10 issued, 3
// remaining" can be reported without re-deriving history, but a used code can
// never be consumed again.
type RecoveryCode struct {
	ID        uint       `json:"id" gorm:"primaryKey"`
	UserID    uint       `json:"user_id" gorm:"index"`
	Hash      string     `json:"-"`
	Salt      string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
}
