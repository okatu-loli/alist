// Package i18n provides a unified, keyed message catalog shared between the
// alist backend and frontend. The catalog is the single source of truth for
// any cross-boundary text (errors, task names/progress, user-visible
// operation logs): both sides render the same key from the same per-locale
// JSON files, so translating a string in one place takes effect everywhere.
//
// Server operational logs (stdout/file) are not part of this catalog and
// stay English by default.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed locales/*.json
var localeFS embed.FS

// DefaultLang is used as the fallback locale when a key is missing from the
// requested locale, and as the default when negotiation finds no match.
const DefaultLang = "en"

// catalog maps lang -> key -> message template.
var catalog map[string]map[string]string

func init() {
	entries, err := localeFS.ReadDir("locales")
	if err != nil {
		panic(fmt.Sprintf("i18n: failed to read embedded locales: %v", err))
	}
	catalog = make(map[string]map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		lang := strings.TrimSuffix(entry.Name(), ".json")
		data, err := localeFS.ReadFile("locales/" + entry.Name())
		if err != nil {
			panic(fmt.Sprintf("i18n: failed to read locale %q: %v", lang, err))
		}
		var messages map[string]string
		if err := json.Unmarshal(data, &messages); err != nil {
			panic(fmt.Sprintf("i18n: failed to parse locale %q: %v", lang, err))
		}
		catalog[lang] = messages
	}
	if _, ok := catalog[DefaultLang]; !ok {
		panic(fmt.Sprintf("i18n: missing required default locale %q", DefaultLang))
	}
}

// Has reports whether key exists in lang's catalog (no fallback).
func Has(lang, key string) bool {
	messages, ok := catalog[lang]
	if !ok {
		return false
	}
	_, ok = messages[key]
	return ok
}

// T renders the message for key, falling back to DefaultLang if the key is
// missing in the resolved locale, and finally to the key itself if it is
// missing everywhere.
//
// loc selects the locale: either a raw locale string ("en", "zh-CN") or a
// request context carrying the negotiated locale (e.g. *gin.Context, resolved
// via Lang). Any other value falls back to DefaultLang.
//
// args are optional name/value pairs used to substitute "{name}"
// placeholders in the message template, e.g.:
//
//	T("zh-CN", "errs.quota_exceeded", "limit", 10)
//	T(c, "errs.quota_exceeded", "limit", 10)
//	// template: "quota exceeded: limit is {limit}"
//
// A malformed args list (odd length, or a non-string name) is ignored for
// the offending pair rather than causing a panic, since T is typically
// called on error/response paths where failing fast is undesirable.
func T(loc any, key string, args ...any) string {
	switch v := loc.(type) {
	case string:
		return translate(v, key, args...)
	case langCarrier:
		return translate(Lang(v), key, args...)
	default:
		return translate(DefaultLang, key, args...)
	}
}

// translate is the locale-explicit primitive underlying T; internal callers
// (Render, localizedError) use it directly to skip the loc dispatch.
func translate(lang, key string, args ...any) string {
	template, ok := lookup(lang, key)
	if !ok {
		template, ok = lookup(DefaultLang, key)
	}
	if !ok {
		return key
	}
	return substitute(template, args)
}

func lookup(lang, key string) (string, bool) {
	messages, ok := catalog[lang]
	if !ok {
		return "", false
	}
	msg, ok := messages[key]
	return msg, ok
}

func substitute(template string, args []any) string {
	if len(args) == 0 {
		return template
	}
	replacer := make([]string, 0, len(args))
	for i := 0; i+1 < len(args); i += 2 {
		name, ok := args[i].(string)
		if !ok {
			continue
		}
		replacer = append(replacer, "{"+name+"}", fmt.Sprint(args[i+1]))
	}
	if len(replacer) == 0 {
		return template
	}
	return strings.NewReplacer(replacer...).Replace(template)
}
