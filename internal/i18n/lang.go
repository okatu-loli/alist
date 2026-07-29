package i18n

import "strings"

// Supported lists the locales the catalog can render. Keep this in sync
// with the files under locales/*.json.
var Supported = []string{"en", "zh-CN"}

// Negotiate resolves the locale to render text in, given an HTTP
// Accept-Language header value and an optional explicit user preference.
// userPref, when non-empty and supported, always wins over the header.
// Otherwise the first supported tag found in acceptLanguage is used.
// Falls back to DefaultLang when nothing matches.
func Negotiate(acceptLanguage, userPref string) string {
	if lang, ok := matchSupported(userPref); ok {
		return lang
	}
	for _, tag := range strings.Split(acceptLanguage, ",") {
		tag = strings.TrimSpace(tag)
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i] // drop q-weight, e.g. "zh-CN;q=0.9"
		}
		if lang, ok := matchSupported(tag); ok {
			return lang
		}
	}
	return DefaultLang
}

// matchSupported matches tag against Supported, case-insensitively, first by
// exact match and then by primary subtag (e.g. "zh" matches "zh-CN").
func matchSupported(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	for _, lang := range Supported {
		if strings.EqualFold(tag, lang) {
			return lang, true
		}
	}
	primary := tag
	if i := strings.IndexByte(tag, '-'); i >= 0 {
		primary = tag[:i]
	}
	for _, lang := range Supported {
		langPrimary := lang
		if i := strings.IndexByte(lang, '-'); i >= 0 {
			langPrimary = lang[:i]
		}
		if strings.EqualFold(primary, langPrimary) {
			return lang, true
		}
	}
	return "", false
}
