package op

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils/random"
)

// App passwords (design W3) are per-client, protocol-only credentials —
// WebDAV/FTP and friends cannot do 2FA, so a user mints one of these instead
// of handing out their real password. Unlike op.GetUserByName, this file
// deliberately has NO cache: op.VerifyAppPassword hits the DB on every call so
// revoking a credential (op.DeleteAppPassword) takes effect on the very next
// request, not after a cache TTL.

const (
	// appPasswordPrefix marks the plaintext as an app password at a glance
	// (and lets a protocol handler cheaply distinguish it from a WebAuthn/OTP
	// value if it ever needs to).
	appPasswordPrefix = "alp_"
	// appPasswordRandBytes is the crypto/rand entropy behind the plaintext:
	// 24 bytes = 192 bits, far beyond brute-force range.
	appPasswordRandBytes = 24
	// appPasswordGroupSize groups the base32 body into dash-separated chunks
	// purely for human readability when hand-typing into a client.
	appPasswordGroupSize = 4
	// appPasswordSaltLen mirrors model.User's SetPassword salt length.
	appPasswordSaltLen = 16
	// AppPasswordMaxPerUser is the per-user cap (abuse guard).
	AppPasswordMaxPerUser = 20
)

// generateAppPasswordPlaintext draws 24 crypto/rand bytes, base32-encodes
// them (unpadded, lowercased) and groups the body into "alp_xxxx-xxxx-..."
// for readability. It is only ever computed here; the caller (CreateAppPassword)
// hashes it immediately and returns the plaintext exactly once.
func generateAppPasswordPlaintext() (string, error) {
	buf := make([]byte, appPasswordRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	body := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))

	var b strings.Builder
	b.WriteString(appPasswordPrefix)
	for i := 0; i < len(body); i += appPasswordGroupSize {
		if i > 0 {
			b.WriteByte('-')
		}
		end := i + appPasswordGroupSize
		if end > len(body) {
			end = len(body)
		}
		b.WriteString(body[i:end])
	}
	return b.String(), nil
}

// hashAppPassword hashes plaintext with the same primitive family as the
// primary user password (model.HashPwd, SHA-256 + salt) but under an
// independent per-row salt, and skips model's client-side StaticHash
// pre-hashing layer: that layer exists so a browser never sends the raw
// password over the wire, which does not apply here — the plaintext is
// generated server-side and shown to the caller exactly once over HTTPS.
func hashAppPassword(plaintext, salt string) string {
	return model.HashPwd(plaintext, salt)
}

// CreateAppPassword mints a new app password for userID: a fresh plaintext is
// drawn, hashed under a fresh salt, and persisted; the plaintext itself is
// returned once and never stored. Capped at AppPasswordMaxPerUser per user.
func CreateAppPassword(userID uint, name string) (string, model.AppPassword, error) {
	count, err := db.CountAppPasswordsByUser(userID)
	if err != nil {
		return "", model.AppPassword{}, err
	}
	if count >= AppPasswordMaxPerUser {
		return "", model.AppPassword{}, errs.AppPasswordLimitExceeded
	}

	plaintext, err := generateAppPasswordPlaintext()
	if err != nil {
		return "", model.AppPassword{}, err
	}
	salt := random.String(appPasswordSaltLen)
	ap := model.AppPassword{
		UserID: userID,
		Name:   name,
		Hash:   hashAppPassword(plaintext, salt),
		Salt:   salt,
	}
	if err := db.CreateAppPassword(&ap); err != nil {
		return "", model.AppPassword{}, err
	}
	return plaintext, ap, nil
}

// ListAppPasswords returns every app password userID owns (Hash/Salt are
// already excluded from JSON by the model tags; callers get the live rows).
func ListAppPasswords(userID uint) ([]model.AppPassword, error) {
	return db.ListAppPasswordsByUser(userID)
}

// DeleteAppPassword revokes an app password by id, scoped to its owner: a
// missing id and an id owned by a different user both collapse to
// errs.AppPasswordNotFound (enumeration defense, same treatment as
// op.ResolveShare) — this op has no admin bypass (design W3: self-service
// only, "admin 无代删").
func DeleteAppPassword(userID, id uint) error {
	ap, err := db.GetAppPasswordByID(id)
	if err != nil {
		return err
	}
	if ap.UserID != userID {
		return errs.AppPasswordNotFound
	}
	return db.DeleteAppPasswordByID(id)
}

// VerifyAppPassword resolves (username, plaintext) to the owning *model.User.
// Every miss shape — unknown username, disabled user, guest account, user
// with zero app passwords, and a wrong plaintext against an existing one —
// collapses to the single errs.PermissionDenied so a protocol client can
// never enumerate which case it hit. The guest rejection is defense-in-depth
// (mirrors internal/webdav/auth.go's raw-password branch): a guest should
// never authenticate to a protocol, even in the hypothetical case where one
// somehow had an app-password row. A hit updates LastUsed best-effort (its
// own failure never fails the verify) and always re-reads the DB — no cache,
// so a just-revoked password fails on the very next call.
func VerifyAppPassword(username, plaintext string) (*model.User, error) {
	user, err := db.GetUserByName(username)
	if err != nil {
		return nil, errs.PermissionDenied
	}
	if user.Disabled || user.IsGuest() {
		return nil, errs.PermissionDenied
	}

	aps, err := db.ListAppPasswordsByUser(user.ID)
	if err != nil {
		return nil, errs.PermissionDenied
	}

	var matched *model.AppPassword
	for i := range aps {
		candidate := hashAppPassword(plaintext, aps[i].Salt)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(aps[i].Hash)) == 1 {
			matched = &aps[i]
		}
	}
	if matched == nil {
		return nil, errs.PermissionDenied
	}

	// Best-effort: LastUsed is a UX nicety, never a gate.
	_ = db.UpdateAppPasswordLastUsed(matched.ID, time.Now())

	return user, nil
}
