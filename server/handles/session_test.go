package handles

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/device"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// TestKickSessionRevokesTokensImmediately pins the whole point of wiring
// eviction through tokenstore: once an admin/user kicks a session, its
// access token must be rejected right away — without waiting for the
// client to make another request through HandleSession (which is what
// would previously catch a stale session, but only reactively).
func TestKickSessionRevokesTokensImmediately(t *testing.T) {
	user := mustRefreshTestUser(t, "kick-user")
	login := doLogin(t, "kick-user", "pw-kick-user")

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, user)
	body, _ := json.Marshal(EvictSessionReq{SessionID: login.Data.DeviceKey})
	req := httptest.NewRequest("POST", "/api/me/sessions/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	EvictMySession(c)
	if w.Code != 200 {
		t.Fatalf("evict should succeed: http=%d body=%s", w.Code, w.Body.String())
	}

	if _, err := common.ParseToken(login.Data.Token); err == nil {
		t.Fatal("access token for a kicked session must be rejected immediately, without going through HandleSession")
	}
}

// TestEvictOldestRevokesItsTokens pins that the evict_oldest device policy
// doesn't just flip a session to inactive — it also kills the tokens that
// were already issued for that session, so a device pushed out by a newer
// login can't keep using its still-unexpired access token.
func TestEvictOldestRevokesItsTokens(t *testing.T) {
	user := &model.User{Username: "evict-oldest-user"}
	user.SetPassword("pw-evict-oldest-user")
	if err := op.CreateUser(user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(user.ID) })

	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.MaxDevices, Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.DeviceEvictPolicy, Value: "evict_oldest"}); err != nil {
		t.Fatal(err)
	}

	deviceKeyA := "device-a-" + user.Username
	deviceKeyB := "device-b-" + user.Username

	if err := device.EnsureActiveOnLogin(user.ID, deviceKeyA, "ua-a", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	tokenA, err := common.GenerateToken(user, deviceKeyA)
	if err != nil {
		t.Fatal(err)
	}
	claimsA, err := common.ParseTokenClaims(tokenA)
	if err != nil {
		t.Fatal(err)
	}

	// Device B logs in while max_devices=1/evict_oldest is active: this must
	// evict device A's session AND its already-issued token.
	if err := device.EnsureActiveOnLogin(user.ID, deviceKeyB, "ua-b", "2.2.2.2"); err != nil {
		t.Fatal(err)
	}

	if _, err := common.ParseToken(tokenA); err == nil {
		t.Fatal("evicted device A's token must be rejected immediately")
	}
	if _, err := tokenstore.Get().Get(claimsA.JTI); !errors.Is(err, tokenstore.ErrTokenRevoked) {
		t.Fatalf("evicted device A's jti must be revoked in the store, got: %v", err)
	}
}
