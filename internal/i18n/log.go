package i18n

import "sync/atomic"

// logLang holds the locale used to render operator-facing (OPS) logs via Log.
// It mirrors conf.Conf.Log.Lang, set once at startup by bootstrap through
// SetLogLang, and defaults to English so log output is byte-for-byte unchanged
// until an operator explicitly switches it. i18n stays conf-free (see the
// package comment in catalog.go) by taking the value through a setter rather
// than importing internal/conf.
var logLang atomic.Value // string

func init() {
	logLang.Store(DefaultLang)
}

// SetLogLang selects the locale Log renders in. An empty or unsupported lang
// falls back to English. Call once after config load (bootstrap.Log).
func SetLogLang(lang string) {
	if _, ok := matchSupported(lang); !ok {
		lang = DefaultLang
	}
	logLang.Store(lang)
}

// LogLang reports the locale Log currently renders in.
func LogLang() string {
	if v, ok := logLang.Load().(string); ok {
		return v
	}
	return DefaultLang
}

// Log renders an operator-facing log message for key in the configured log
// locale (conf.Conf.Log.Lang, default "en"). When the locale is English — the
// default — this is a straight catalog template render with no extra cost and
// no behavior change versus the previous hard-coded English log strings, e.g.:
//
//	utils.Log.Info(i18n.Log("log.server_started_http", "addr", httpBase))
//
// Only core lifecycle/error logs are keyed; the mass of debug/info logs stay
// plain English (see the package comment in catalog.go).
func Log(key string, args ...any) string {
	return translate(LogLang(), key, args...)
}
