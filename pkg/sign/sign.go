package sign

import "errors"

type Sign interface {
	Sign(data string, expire int64) string
	Verify(data, sign string) error
}

var (
	ErrSignExpired   = errors.New("sign expired")
	ErrSignInvalid   = errors.New("sign invalid")
	ErrExpireInvalid = errors.New("expire invalid")
	ErrExpireMissing = errors.New("expire missing")
	ErrKeyUnknown    = errors.New("sign key unknown")
	// ErrSignerUnavailable is returned when no signer could be built from the
	// current signing key store (e.g. no active key yet). Verify must fail
	// closed with this rather than treat "no signer" as "signature valid".
	ErrSignerUnavailable = errors.New("no signer available")
)
