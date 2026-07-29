package i18n

// localizedError is a catalog-keyed error. Error() renders the message in the
// locale it was built with, so it can be handed straight to an API envelope's
// "message" field, while still being usable with errors.Is/As against the
// wrapped sentinel. It retains its key + args so the envelope can re-render it
// in the viewer's negotiated locale (see Render).
type localizedError struct {
	lang string
	key  string
	err  error
	args []any
}

func (e *localizedError) Error() string { return translate(e.lang, e.key, e.args...) }
func (e *localizedError) Unwrap() error { return e.err }

// Key returns the catalog key that produced this error's message, useful
// for structured logging/telemetry that wants the key rather than the
// localized text.
func (e *localizedError) Key() string { return e.key }

// Error builds a localized error by rendering key for lang via T, wrapping
// the given sentinel err.
//
// This is the pilot for the following pattern: a handler negotiates the
// caller's locale once, then renders any user-facing error through the
// catalog instead of returning the English sentinel message directly, e.g.:
//
//	lang := i18n.Negotiate(c.GetHeader("Accept-Language"), user.Language)
//	if obj == nil {
//	    return i18n.Error(lang, "errs.object_not_found", errs.ObjectNotFound)
//	}
//
// The sentinel (errs.ObjectNotFound here) is preserved via Unwrap so
// errors.Is(err, errs.ObjectNotFound) still works for callers that branch
// on error identity rather than display text.
func Error(lang, key string, err error, args ...any) error {
	return &localizedError{
		lang: lang,
		key:  key,
		err:  err,
		args: args,
	}
}
