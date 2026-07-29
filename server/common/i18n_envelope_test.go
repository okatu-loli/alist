package common

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// newCtx builds a gin context whose ctxkey.Lang is negotiated from the given
// Accept-Language header, exactly as the I18n middleware would set it.
func newCtx(acceptLanguage string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	if acceptLanguage != "" {
		c.Request.Header.Set("Accept-Language", acceptLanguage)
	}
	c.Set(ctxkey.Lang, i18n.Negotiate(c.GetHeader("Accept-Language"), ""))
	return c, w
}

func decodeMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp Resp[interface{}]
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", w.Body.String(), err)
	}
	return resp.Message
}

func TestErrorResp_RendersErrsSentinelByLang(t *testing.T) {
	cases := []struct {
		acceptLanguage string
		want           string
	}{
		{"zh-CN", "对象不存在"},
		{"zh-CN,en;q=0.8", "对象不存在"},
		{"en", "object not found"},
		{"", "object not found"}, // absent header → default en
	}
	for _, tc := range cases {
		c, w := newCtx(tc.acceptLanguage)
		ErrorResp(c, errs.ObjectNotFound, 404)
		if got := decodeMessage(t, w); got != tc.want {
			t.Errorf("Accept-Language=%q: message = %q, want %q", tc.acceptLanguage, got, tc.want)
		}
	}
}

func TestErrorResp_UnknownErrorPassesThrough(t *testing.T) {
	c, w := newCtx("zh-CN")
	ErrorResp(c, errors.New("some raw failure"), 500)
	if got := decodeMessage(t, w); got != "some raw failure" {
		t.Errorf("unknown error message = %q, want passthrough", got)
	}
}
