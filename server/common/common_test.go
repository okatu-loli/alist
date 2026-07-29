package common

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:common_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

// AuthzErrorResp must classify op.Authorize errors: policy/password denials
// are caller-facing 403s, anything else is an engine-internal failure and
// must surface as 500 rather than masquerade as a permission problem.
func TestAuthzErrorResp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name     string
		err      error
		wantCode int
	}{
		{"permission denied", errs.PermissionDenied, 403},
		{"wrong password", errs.WrongPassword, 403},
		{"engine failure", errors.New("db down"), 500},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			AuthzErrorResp(c, tc.err, "permission denied")

			var resp struct {
				Code int `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d (body: %s)", resp.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}
