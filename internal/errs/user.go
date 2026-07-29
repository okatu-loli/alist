package errs

import "errors"

var (
	EmptyUsername      = errors.New("username is empty")
	EmptySSOID         = errors.New("sso id is empty")
	EmptyPassword      = errors.New("password is empty")
	WrongPassword      = errors.New("password is incorrect")
	DeleteAdminOrGuest = errors.New("cannot delete admin or guest")
	LastAdmin          = errors.New("at least one enabled admin must be kept")

	// AppPasswordLimitExceeded is returned when a user tries to mint more than
	// the per-user app-password cap (design W3 §op layer, abuse guard).
	AppPasswordLimitExceeded = errors.New("app password limit exceeded")
	// AppPasswordNotFound is returned uniformly for a missing app-password id
	// AND for an id that exists but belongs to a different user — the two
	// cases are deliberately indistinguishable (enumeration defense, same
	// treatment as errs.ShareNotFound).
	AppPasswordNotFound = errors.New("app password not found")

	// TOTPInvalidCode is returned uniformly for a wrong TOTP code, a replayed
	// (already-consumed) code, and an already-used recovery code — the three
	// cases are deliberately indistinguishable at the API boundary so a
	// brute-forcer cannot tell which check failed.
	TOTPInvalidCode = errors.New("invalid totp code")
)
