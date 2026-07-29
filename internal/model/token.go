package model

const (
	TokenKindAccess  = "access"
	TokenKindRefresh = "refresh"
	// TokenKindExtScoped marks an extension scoped token (Step 10). Kept short
	// (fits the Kind column's size:8) and distinct so common.ParseToken — which
	// only accepts TokenKindAccess as a bearer — rejects it as a normal login
	// token, while extension.AuthenticateScopedToken accepts only this kind.
	TokenKindExtScoped = "ext"
)

// Token is one issued token's revocation record: it exists so a jti can be
// looked up and marked revoked before its natural expiry, and so every token
// tied to a session or user can be swept at once (logout / kick / password
// change).
type Token struct {
	// column:jti pinned explicitly — gorm's default naming strategy does not
	// treat "JTI" as a known initialism, so the bare field name would not
	// reliably render to "jti" (same reasoning as SigningKey.KID).
	JTI       string `gorm:"column:jti;primaryKey;size:36"`
	UserID    uint   `gorm:"index"`
	SessionID string `gorm:"index;size:64"` // device session key
	Kind      string `gorm:"size:8"`        // "access" | "refresh"
	ExpiresAt int64  `gorm:"index"`
	Revoked   bool
	CreatedAt int64
}
