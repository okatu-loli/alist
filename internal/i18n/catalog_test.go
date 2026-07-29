package i18n

import (
	"errors"
	"testing"
)

func TestT_Lookup(t *testing.T) {
	got := T("en", "errs.object_not_found")
	want := "object not found"
	if got != want {
		t.Errorf("T(en, errs.object_not_found) = %q, want %q", got, want)
	}

	got = T("zh-CN", "errs.object_not_found")
	want = "对象不存在"
	if got != want {
		t.Errorf("T(zh-CN, errs.object_not_found) = %q, want %q", got, want)
	}
}

func TestT_FallsBackToEnglishWhenLocaleMissingKey(t *testing.T) {
	// zh-CN's catalog does not have this key; en does.
	key := "errs.only_in_en_for_test"
	catalog["en"][key] = "english only message"
	defer delete(catalog["en"], key)

	got := T("zh-CN", key)
	want := "english only message"
	if got != want {
		t.Errorf("T(zh-CN, %s) = %q, want fallback %q", key, got, want)
	}
}

func TestT_MissingKeyReturnsKeyItself(t *testing.T) {
	key := "errs.does_not_exist_anywhere"
	got := T("en", key)
	if got != key {
		t.Errorf("T(en, %s) = %q, want key echoed back %q", key, got, key)
	}
}

func TestT_ParamSubstitution(t *testing.T) {
	key := "test.with_params"
	catalog["en"][key] = "hello {name}, you have {count} items"
	defer delete(catalog["en"], key)

	got := T("en", key, "name", "alice", "count", 3)
	want := "hello alice, you have 3 items"
	if got != want {
		t.Errorf("T with params = %q, want %q", got, want)
	}
}

func TestHas(t *testing.T) {
	if !Has("en", "errs.permission_denied") {
		t.Error("Has(en, errs.permission_denied) = false, want true")
	}
	if Has("en", "errs.nope") {
		t.Error("Has(en, errs.nope) = true, want false")
	}
	if Has("fr", "errs.permission_denied") {
		t.Error("Has(fr, ...) = true, want false (unsupported locale)")
	}
}

// TestLocaleParity enforces that every key present in any locale exists in
// every supported locale, so a migrated message never falls back to English
// (or to the bare key) for lack of a translation.
func TestLocaleParity(t *testing.T) {
	keys := map[string]bool{}
	for _, messages := range catalog {
		for key := range messages {
			keys[key] = true
		}
	}
	for _, lang := range Supported {
		for key := range keys {
			if !Has(lang, key) {
				t.Errorf("key %q missing in locale %q", key, lang)
			}
		}
	}
}

func TestNegotiate(t *testing.T) {
	cases := []struct {
		name           string
		acceptLanguage string
		userPref       string
		want           string
	}{
		{"user pref wins over header", "en", "zh-CN", "zh-CN"},
		{"exact match in header", "zh-CN,en;q=0.8", "", "zh-CN"},
		{"primary subtag match", "zh-TW,en;q=0.5", "", "zh-CN"},
		{"unsupported falls back to default", "fr-FR,de;q=0.9", "", DefaultLang},
		{"empty everything falls back to default", "", "", DefaultLang},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Negotiate(tc.acceptLanguage, tc.userPref)
			if got != tc.want {
				t.Errorf("Negotiate(%q, %q) = %q, want %q", tc.acceptLanguage, tc.userPref, got, tc.want)
			}
		})
	}
}

// TestError_PilotIntegration demonstrates the end-to-end pattern a handler
// would use: negotiate a locale, then render a sentinel error through the
// catalog while still preserving errors.Is against the sentinel.
func TestError_PilotIntegration(t *testing.T) {
	sentinel := errors.New("object not found")
	lang := Negotiate("zh-CN,en;q=0.9", "")

	err := Error(lang, "errs.object_not_found", sentinel)

	wantMsg := "对象不存在"
	if err.Error() != wantMsg {
		t.Errorf("err.Error() = %q, want %q", err.Error(), wantMsg)
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is(err, sentinel) = false, want true")
	}
}
