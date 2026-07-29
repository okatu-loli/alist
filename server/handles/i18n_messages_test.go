package handles

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// i18nCtx builds a gin context whose ctxkey.Lang is negotiated from the given
// Accept-Language header, exactly as the I18n middleware would set it.
func i18nCtx(acceptLanguage string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	if acceptLanguage != "" {
		c.Request.Header.Set("Accept-Language", acceptLanguage)
	}
	c.Set(ctxkey.Lang, i18n.Negotiate(c.GetHeader("Accept-Language"), ""))
	return c, w
}

func i18nMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp common.Resp[interface{}]
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", w.Body.String(), err)
	}
	return resp.Message
}

// TestHandlerMessagesI18n asserts a representative migrated error from each
// heavy handler renders Chinese under Accept-Language: zh-CN and English under
// en / absent header.
func TestHandlerMessagesI18n(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		args   []any
		code   int
		wantZh string
		wantEn string
	}{
		{
			name:   "user role assign admin only",
			key:    "user.role_assign_admin_only",
			code:   403,
			wantZh: "只有管理员才能分配用户的角色",
			wantEn: "only admin can assign a user's role binding",
		},
		{
			name:   "auth too many login attempts",
			key:    "auth.too_many_login_attempts",
			code:   429,
			wantZh: "登录失败次数过多（用户名或密码错误），请稍后再试。",
			wantEn: "Too many unsuccessful sign-in attempts have been made using an incorrect username or password, Try again later.",
		},
		{
			name:   "task not found",
			key:    "task.not_found",
			code:   404,
			wantZh: "任务不存在",
			wantEn: "task not found",
		},
		{
			name:   "fsmanage empty file names",
			key:    "fsmanage.empty_file_names",
			code:   400,
			wantZh: "文件名为空",
			wantEn: "Empty file names",
		},
		{
			name:   "fsmanage file exists with param",
			key:    "fsmanage.file_exists",
			args:   []any{"name", "report.pdf"},
			code:   403,
			wantZh: "文件 [report.pdf] 已存在",
			wantEn: "file [report.pdf] exists",
		},
		{
			name:   "fsup upload not supported",
			key:    "errs.upload_not_supported",
			code:   405,
			wantZh: "不支持上传",
			wantEn: "upload not supported",
		},
		{
			name:   "fsread refresh without permission",
			key:    "fsread.refresh_without_permission",
			code:   403,
			wantZh: "没有权限刷新",
			wantEn: "Refresh without permission",
		},
		{
			name:   "permission system admin only",
			key:    "permission.system_admin_only",
			code:   403,
			wantZh: "只有管理员才能管理系统权限",
			wantEn: "only admin can manage system permissions",
		},
		{
			name:   "setting default role",
			key:    "setting.cannot_set_admin_or_guest_default_role",
			code:   400,
			wantZh: "无法将管理员或访客设置为默认角色",
			wantEn: "cannot set admin or guest as default role",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, lc := range []struct {
				acceptLanguage string
				want           string
			}{
				{"zh-CN", tc.wantZh},
				{"en", tc.wantEn},
				{"", tc.wantEn},
			} {
				c, w := i18nCtx(lc.acceptLanguage)
				common.ErrorI18n(c, tc.key, tc.code, tc.args...)
				if got := i18nMessage(t, w); got != lc.want {
					t.Errorf("%s [Accept-Language=%q]: message = %q, want %q", tc.name, lc.acceptLanguage, got, lc.want)
				}
			}
		})
	}
}
