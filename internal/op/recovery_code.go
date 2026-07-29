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

// Recovery codes (Phase C: internal TOTP second factor) are one-time backup
// credentials minted alongside a confirmed TOTP enrollment, so a user who
// loses their authenticator device can still complete the login step-up.
// Like app passwords (design W3), only a salted hash is ever persisted; the
// plaintext is generated here and returned to the caller exactly once.

const (
	// RecoveryCodeCount is how many codes a single (re)generation mints.
	RecoveryCodeCount = 10
	// recoveryCodeRandBytes is the crypto/rand entropy behind each plaintext:
	// 10 bytes base32-encoded is 16 chars (~80 bits) — short enough to type by
	// hand, far beyond brute-force range given the throttle on the verify path.
	recoveryCodeRandBytes = 10
	// recoveryCodeGroupSize groups the body into dash-separated chunks purely
	// for human readability.
	recoveryCodeGroupSize = 4
	// recoveryCodeSaltLen mirrors app-password's independent per-row salt.
	recoveryCodeSaltLen = 16
)

// generateRecoveryCodePlaintext draws crypto/rand entropy, base32-encodes it
// (unpadded, lowercased) and groups the body into "xxxx-xxxx-xxxx-xxxx" for
// readability.
func generateRecoveryCodePlaintext() (string, error) {
	buf := make([]byte, recoveryCodeRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	body := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))

	var b strings.Builder
	for i := 0; i < len(body); i += recoveryCodeGroupSize {
		if i > 0 {
			b.WriteByte('-')
		}
		end := i + recoveryCodeGroupSize
		if end > len(body) {
			end = len(body)
		}
		b.WriteString(body[i:end])
	}
	return b.String(), nil
}

// normalizeRecoveryCode strips whitespace and lowercases a user-submitted
// recovery code so "AAAA-BBBB-CCCC-DDDD" and "aaaa-bbbb-cccc-dddd" (and one
// with stray spaces pasted in) hash identically to what was generated.
func normalizeRecoveryCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

func hashRecoveryCode(plaintext, salt string) string {
	return model.HashPwd(plaintext, salt)
}

// GenerateRecoveryCodes mints RecoveryCodeCount fresh recovery codes for
// userID, replacing any existing set (a regeneration invalidates every
// previously-issued code — there is only ever one live batch). The plaintexts
// are returned once; only their hashes are persisted.
func GenerateRecoveryCodes(userID uint) ([]string, error) {
	if err := db.DeleteRecoveryCodesByUser(userID); err != nil {
		return nil, err
	}
	plaintexts := make([]string, 0, RecoveryCodeCount)
	rows := make([]model.RecoveryCode, 0, RecoveryCodeCount)
	for i := 0; i < RecoveryCodeCount; i++ {
		plaintext, err := generateRecoveryCodePlaintext()
		if err != nil {
			return nil, err
		}
		salt := random.String(recoveryCodeSaltLen)
		plaintexts = append(plaintexts, plaintext)
		rows = append(rows, model.RecoveryCode{
			UserID: userID,
			Hash:   hashRecoveryCode(normalizeRecoveryCode(plaintext), salt),
			Salt:   salt,
		})
	}
	if err := db.CreateRecoveryCodes(rows); err != nil {
		return nil, err
	}
	return plaintexts, nil
}

// ConsumeRecoveryCode verifies plaintext against userID's unused recovery
// codes and, on a match, marks that single row used so it can never be
// consumed again. A wrong code and an already-used code both return
// errs.TOTPInvalidCode (indistinguishable — see that error's doc).
//
// The match (read) and the consume (write) are two separate steps, but the
// write itself is atomic and conditional (db.MarkRecoveryCodeUsedIfUnused —
// red-team I7): if two goroutines both read the same still-unused row for the
// same code, only ONE of them wins the subsequent conditional update; the
// loser gets errs.TOTPInvalidCode exactly as if the code had already been
// used, so the same recovery code can never complete two concurrent step-ups.
func ConsumeRecoveryCode(userID uint, plaintext string) error {
	rows, err := db.ListRecoveryCodesByUser(userID)
	if err != nil {
		return err
	}
	normalized := normalizeRecoveryCode(plaintext)
	var matched *model.RecoveryCode
	for i := range rows {
		if rows[i].UsedAt != nil {
			continue
		}
		candidate := hashRecoveryCode(normalized, rows[i].Salt)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(rows[i].Hash)) == 1 {
			matched = &rows[i]
		}
	}
	if matched == nil {
		return errs.TOTPInvalidCode
	}
	ok, err := db.MarkRecoveryCodeUsedIfUnused(matched.ID, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return errs.TOTPInvalidCode
	}
	return nil
}

// RecoveryCodeStatus reports how many recovery codes userID has left, without
// ever exposing plaintext or hashes — the account page shows "3 of 10
// remaining", nothing more.
func RecoveryCodeStatus(userID uint) (total, remaining int, err error) {
	rows, err := db.ListRecoveryCodesByUser(userID)
	if err != nil {
		return 0, 0, err
	}
	total = len(rows)
	for _, r := range rows {
		if r.UsedAt == nil {
			remaining++
		}
	}
	return total, remaining, nil
}

// DeleteRecoveryCodes removes every recovery code for userID (TOTP unbind
// leaves no dangling backup credentials behind).
func DeleteRecoveryCodes(userID uint) error {
	return db.DeleteRecoveryCodesByUser(userID)
}
