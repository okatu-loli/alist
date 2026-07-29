package i18n

import (
	"errors"

	"github.com/alist-org/alist/v3/internal/errs"
)

// sentinelKey maps the backend's error sentinels to their catalog keys. It is
// the translation table for errors that are returned as bare sentinels (via
// errors.Is identity) rather than pre-localized *localizedError values. Keep
// it in sync with internal/errs and locales/*.json (a test enforces every
// mapped key exists in both locales).
var sentinelKey = map[error]string{
	// errors.go
	errs.NotImplement:              "errs.not_implement",
	errs.NotSupport:                "errs.not_support",
	errs.RelativePath:              "errs.relative_path",
	errs.MoveBetweenTwoStorages:    "errs.move_between_two_storages",
	errs.UploadNotSupported:        "errs.upload_not_supported",
	errs.MetaNotFound:              "errs.meta_not_found",
	errs.StorageNotFound:           "errs.storage_not_found",
	errs.StreamIncomplete:          "errs.stream_incomplete",
	errs.StreamPeekFail:            "errs.stream_peek_fail",
	errs.UnknownArchiveFormat:      "errs.unknown_archive_format",
	errs.WrongArchivePassword:      "errs.wrong_archive_password",
	errs.DriverExtractNotSupported: "errs.driver_extract_not_supported",
	// user.go
	errs.EmptyUsername:      "errs.empty_username",
	errs.EmptyPassword:      "errs.empty_password",
	errs.WrongPassword:      "errs.wrong_password",
	errs.DeleteAdminOrGuest: "errs.delete_admin_or_guest",
	errs.LastAdmin:          "errs.last_admin",
	// object.go
	errs.ObjectNotFound: "errs.object_not_found",
	errs.NotFolder:      "errs.not_folder",
	errs.NotFile:        "errs.not_file",
	// operate.go
	errs.PermissionDenied: "errs.permission_denied",
	errs.InvalidName:      "errs.invalid_name",
	// driver.go
	errs.EmptyToken: "errs.empty_token",
	errs.LinkIsDir:  "errs.link_is_dir",
	// role.go
	errs.ErrChangeDefaultRole: "errs.change_default_role",
	// search.go
	errs.SearchNotAvailable:  "errs.search_not_available",
	errs.BuildIndexIsRunning: "errs.build_index_is_running",
	// device.go
	errs.TooManyDevices:  "errs.too_many_devices",
	errs.SessionInactive: "errs.session_inactive",
}

// Render resolves a user-facing message for err in lang, for the API envelope:
//
//   - a *localizedError re-renders its key+args in lang (so the viewer's
//     negotiated locale wins over whatever locale built the error);
//   - a bare/wrapped errs sentinel is looked up in sentinelKey and rendered;
//   - anything else passes through as err.Error() untranslated — this is the
//     progressive-compatibility path for messages not yet migrated to keys.
func Render(lang string, err error) string {
	if err == nil {
		return ""
	}
	var le *localizedError
	if errors.As(err, &le) {
		return translate(lang, le.key, le.args...)
	}
	for sentinel, key := range sentinelKey {
		if errors.Is(err, sentinel) {
			return translate(lang, key)
		}
	}
	return err.Error()
}
