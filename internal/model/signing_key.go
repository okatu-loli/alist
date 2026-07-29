package model

const (
	SignPurposeDownload = "download"
	// Reserved for when the archive/share packages return (not wired in Step 6):
	SignPurposeArchive = "archive"
	SignPurposeShare   = "share"
)

// SigningKey is one HMAC secret for a signing purpose, addressed by a stable
// KID so signatures can be verified across a rotation grace window. Exactly one
// key per purpose is Active (used for new signatures); non-active keys remain
// verifiable until an admin removes them.
type SigningKey struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	Purpose string `json:"purpose" gorm:"index:idx_signing_key_purpose"`
	// column:kid pinned explicitly — gorm's default naming strategy renders
	// the bare field name "k_id" (its "ID" initialism handling collides with
	// the leading K), not "kid".
	KID       string `json:"kid" gorm:"column:kid;uniqueIndex;size:32"`
	Secret    string `json:"-"`
	Active    bool   `json:"active"`
	CreatedAt int64  `json:"created_at"`
}
