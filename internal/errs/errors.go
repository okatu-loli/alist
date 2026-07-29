package errs

import (
	"errors"
	"fmt"

	pkgerr "github.com/pkg/errors"
)

var (
	NotImplement = errors.New("not implement")
	NotSupport   = errors.New("not support")
	RelativePath = errors.New("access using relative path is not allowed")

	MoveBetweenTwoStorages = errors.New("can't move files between two storages, try to copy")
	UploadNotSupported     = errors.New("upload not supported")

	MetaNotFound    = errors.New("meta not found")
	StorageNotFound = errors.New("storage not found")

	// ShareNotFound is returned when a share token does not resolve to a live
	// share. It is deliberately used for every non-live case — missing, disabled
	// and expired shares all map to this one error — so the public endpoints
	// answer with a single 404 and leak no information distinguishing them
	// (enumeration defense; see internal/op/share.go ResolveShare).
	ShareNotFound = errors.New("share not found")
	// ShareOutOfScope is returned when a requested sub-path escapes the share's
	// subtree. It is the single confinement gate (op.ShareSubPath): any path that
	// is not the share root itself or a descendant of it is rejected with this.
	ShareOutOfScope  = errors.New("share sub-path out of scope")
	StreamIncomplete = errors.New("upload/download stream incomplete, possible network issue")
	StreamPeekFail   = errors.New("StreamPeekFail")

	UnknownArchiveFormat      = errors.New("unknown archive format")
	WrongArchivePassword      = errors.New("wrong archive password")
	DriverExtractNotSupported = errors.New("driver extraction not supported")
)

// NewErr wrap constant error with an extra message
// use errors.Is(err1, StorageNotFound) to check if err belongs to any internal error
func NewErr(err error, format string, a ...any) error {
	return fmt.Errorf("%w; %s", err, fmt.Sprintf(format, a...))
}

func IsNotFoundError(err error) bool {
	return errors.Is(pkgerr.Cause(err), ObjectNotFound) || errors.Is(pkgerr.Cause(err), StorageNotFound)
}

func IsNotSupportError(err error) bool {
	return errors.Is(pkgerr.Cause(err), NotSupport)
}
func IsNotImplement(err error) bool {
	return errors.Is(pkgerr.Cause(err), NotImplement)
}
