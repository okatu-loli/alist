package i18n

import "testing"

// TestLog_DefaultLocaleIsEnglish guards the zero-config path: without any
// SetLogLang call, Log renders operator logs in English exactly as the
// hard-coded strings did before i18n.
func TestLog_DefaultLocaleIsEnglish(t *testing.T) {
	if got := LogLang(); got != DefaultLang {
		t.Fatalf("LogLang() default = %q, want %q", got, DefaultLang)
	}
	key := "log.test_render"
	catalog["en"][key] = "server started @ {addr}"
	catalog["zh-CN"][key] = "服务已启动 @ {addr}"
	defer delete(catalog["en"], key)
	defer delete(catalog["zh-CN"], key)

	if got := Log(key, "addr", "127.0.0.1"); got != "server started @ 127.0.0.1" {
		t.Errorf("Log(en default) = %q, want english render", got)
	}
}

func TestLog_RendersConfiguredLocale(t *testing.T) {
	key := "log.test_render"
	catalog["en"][key] = "server started @ {addr}"
	catalog["zh-CN"][key] = "服务已启动 @ {addr}"
	defer delete(catalog["en"], key)
	defer delete(catalog["zh-CN"], key)

	SetLogLang("zh-CN")
	defer SetLogLang(DefaultLang)

	if got := Log(key, "addr", "127.0.0.1"); got != "服务已启动 @ 127.0.0.1" {
		t.Errorf("Log(zh-CN) = %q, want chinese render", got)
	}
	if got := LogLang(); got != "zh-CN" {
		t.Errorf("LogLang() = %q, want zh-CN", got)
	}
}

// TestSetLogLang_UnsupportedResetsToEnglish keeps a bad config value (or the
// empty default) from silently breaking log rendering.
func TestSetLogLang_UnsupportedResetsToEnglish(t *testing.T) {
	SetLogLang("zh-CN")
	SetLogLang("fr-FR")
	if got := LogLang(); got != DefaultLang {
		t.Errorf("LogLang() after unsupported = %q, want %q", got, DefaultLang)
	}
	SetLogLang("")
	if got := LogLang(); got != DefaultLang {
		t.Errorf("LogLang() after empty = %q, want %q", got, DefaultLang)
	}
}
