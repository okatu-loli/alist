package i18n

import (
	"errors"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
)

func TestRender_ErrsSentinelByLang(t *testing.T) {
	cases := []struct {
		lang string
		want string
	}{
		{"zh-CN", "对象不存在"},
		{"en", "object not found"},
		{"fr", "object not found"}, // unsupported → default en
	}
	for _, tc := range cases {
		if got := Render(tc.lang, errs.ObjectNotFound); got != tc.want {
			t.Errorf("Render(%q, ObjectNotFound) = %q, want %q", tc.lang, got, tc.want)
		}
	}
}

func TestRender_UnknownErrorPassthrough(t *testing.T) {
	err := errors.New("some untranslated failure")
	if got := Render("zh-CN", err); got != "some untranslated failure" {
		t.Errorf("Render(zh-CN, unknown) = %q, want passthrough", got)
	}
}

func TestRender_WrappedSentinel(t *testing.T) {
	wrapped := errs.NewErr(errs.StorageNotFound, "detail")
	if got := Render("zh-CN", wrapped); got != "存储器不存在" {
		t.Errorf("Render(zh-CN, wrapped StorageNotFound) = %q, want 存储器不存在", got)
	}
}

func TestRender_LocalizedErrorReRendersByLang(t *testing.T) {
	// Built in en, but Render must honor the requested lang from ctx.
	err := Error("en", "errs.object_not_found", errs.ObjectNotFound)
	if got := Render("zh-CN", err); got != "对象不存在" {
		t.Errorf("Render(zh-CN, localizedError) = %q, want 对象不存在", got)
	}
}

func TestError_PreservesErrorsIs(t *testing.T) {
	err := Error("zh-CN", "errs.object_not_found", errs.ObjectNotFound)
	if !errors.Is(err, errs.ObjectNotFound) {
		t.Error("errors.Is(Error(...), errs.ObjectNotFound) = false, want true")
	}
	if err.Error() != "对象不存在" {
		t.Errorf("Error().Error() = %q, want 对象不存在", err.Error())
	}
}

func TestError_ArgsSurviveReRender(t *testing.T) {
	key := "test.render_args"
	catalog["en"][key] = "hello {name}"
	catalog["zh-CN"][key] = "你好 {name}"
	defer delete(catalog["en"], key)
	defer delete(catalog["zh-CN"], key)

	err := Error("en", key, errors.New("x"), "name", "alice")
	if got := Render("zh-CN", err); got != "你好 alice" {
		t.Errorf("Render re-render with args = %q, want 你好 alice", got)
	}
}

// fakeCtx implements the langCarrier surface (a subset of *gin.Context).
type fakeCtx map[string]string

func (f fakeCtx) GetString(k string) string { return f[k] }

func TestLang_Ctx(t *testing.T) {
	if got := Lang(fakeCtx{ctxkey.Lang: "zh-CN"}); got != "zh-CN" {
		t.Errorf("Lang(ctx zh-CN) = %q, want zh-CN", got)
	}
	if got := Lang(fakeCtx{}); got != DefaultLang {
		t.Errorf("Lang(empty ctx) = %q, want %q", got, DefaultLang)
	}
}

func TestT_CtxDispatch(t *testing.T) {
	if got := T(fakeCtx{ctxkey.Lang: "zh-CN"}, "errs.object_not_found"); got != "对象不存在" {
		t.Errorf("T(ctx, key) = %q, want 对象不存在", got)
	}
	if got := T("en", "errs.object_not_found"); got != "object not found" {
		t.Errorf("T(lang, key) = %q, want object not found", got)
	}
}

// Every mapped sentinel must have a key present in both locales.
func TestSentinelKeyCatalogCoverage(t *testing.T) {
	for sentinel, key := range sentinelKey {
		if !Has("en", key) {
			t.Errorf("sentinel %v → key %q missing in en", sentinel, key)
		}
		if !Has("zh-CN", key) {
			t.Errorf("sentinel %v → key %q missing in zh-CN", sentinel, key)
		}
	}
}
