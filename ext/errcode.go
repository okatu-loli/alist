package ext

// ErrCode is the cross-boundary error code (9.0 §5). v3 drove control flow with
// sentinel errors (errs.NotImplement/NotSupport trigger op-layer fallbacks). Once
// gRPC flattens an error to a status string, errors.Is stops working — so the
// code travels explicitly and the host boundary maps it back to the matching
// sentinel, keeping op-layer control flow (errors.Is(err, errs.NotImplement))
// intact.
type ErrCode string

const (
	ErrCodeNotImplement         ErrCode = "NOT_IMPLEMENT"
	ErrCodeNotSupport           ErrCode = "NOT_SUPPORT"
	ErrCodeObjectNotFound       ErrCode = "OBJECT_NOT_FOUND"
	ErrCodeWrongArchivePassword ErrCode = "WRONG_ARCHIVE_PASSWORD"
	ErrCodePermissionDenied     ErrCode = "PERMISSION_DENIED"
	ErrCodeStorageNotInit       ErrCode = "STORAGE_NOT_INIT"
	ErrCodeStorageNotFound      ErrCode = "STORAGE_NOT_FOUND"
	// ErrCodeArchiveIsDir is returned by an ArchiveTool's Extract when
	// innerPath names a directory rather than a file (Phase E wave 2). Distinct
	// from ErrCodeObjectNotFound: the path DOES resolve, just not to something
	// extractable.
	ErrCodeArchiveIsDir ErrCode = "ARCHIVE_IS_DIR"
	// ErrCodeArchiveInvalid is returned by an ArchiveTool's Meta/List/Extract
	// when the source bytes don't parse as that tool's archive format at all
	// (corrupt or truncated content) — internal/archive maps this back to
	// ErrNotArchive.
	ErrCodeArchiveInvalid ErrCode = "ARCHIVE_INVALID"
)

// Error is an error carrying an ErrCode across the boundary. The host boundary
// layer maps Code back to the corresponding internal sentinel error.
type Error struct {
	Code    ErrCode `json:"code"`
	Message string  `json:"message,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// NewError builds an *Error with a code and message.
func NewError(code ErrCode, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// ErrPermissionDenied is the sentinel a Host capability returns when the
// plugin's Grant does not authorize the call (design §6.3): an ungranted
// capability, a network host outside Grant.Network, or an FS/Sign target the
// plugin's principal cannot reach. It carries ErrCodePermissionDenied so it
// maps back to errs.PermissionDenied across the boundary.
var ErrPermissionDenied = NewError(ErrCodePermissionDenied, "permission denied")

// CodeOf returns the ErrCode of err if it is (or wraps) an *Error, and whether
// one was found.
func CodeOf(err error) (ErrCode, bool) {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e.Code, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return "", false
		}
		err = u.Unwrap()
	}
	return "", false
}
