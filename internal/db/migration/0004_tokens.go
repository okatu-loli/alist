package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the tokens table introduced by this migration. No
// explicit TableName(): "token" pluralizes to "tokens". Tags must byte-match
// model.Token's — the drift guard AutoMigrates the live model and diffs the
// resulting schema against this frozen snapshot. column:jti is pinned for
// the same reason as signingKey.KID in 0003: gorm doesn't treat "JTI" as a
// known initialism.
type token struct {
	JTI       string `gorm:"column:jti;primaryKey;size:36"`
	UserID    uint   `gorm:"index"`
	SessionID string `gorm:"index;size:64"`
	Kind      string `gorm:"size:8"`
	ExpiresAt int64  `gorm:"index"`
	Revoked   bool
	CreatedAt int64
}

func init() {
	register(&gormigrate.Migration{
		ID: "0004_tokens",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&token{})
		},
		Rollback: func(tx *gorm.DB) error {
			return tx.Migrator().DropTable(&token{})
		},
	})
}
