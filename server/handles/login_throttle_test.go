package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// doLoginFromIP drives the real password Login handler (auth.go) from a
// caller-controlled client IP WITHOUT resetting the throttle, so a test can
// exercise per-IP lockout across attempts.
func doLoginFromIP(t *testing.T, username, password, ip string) (*httptest.ResponseRecorder, authnFinishResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{"username": username, "password": password})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	c.Request = req
	Login(c)
	var resp authnFinishResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// setLoginThrottleSetting overrides a login-throttle setting for the duration of
// a test and restores it on cleanup.
func setLoginThrottleSetting(t *testing.T, key, value, restore string) {
	t.Helper()
	if err := op.SaveSettingItem(&model.SettingItem{
		Key: key, Value: value, Type: conf.TypeNumber, Group: model.GLOBAL, Flag: model.PRIVATE,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = op.SaveSettingItem(&model.SettingItem{
			Key: key, Value: restore, Type: conf.TypeNumber, Group: model.GLOBAL, Flag: model.PRIVATE,
		})
	})
}

// TestLoginThrottleUnit exercises the shared throttle directly: it blocks once
// the failure count reaches the threshold, a reset clears it, and the threshold
// is settings-driven.
func TestLoginThrottleUnit(t *testing.T) {
	const ip = "198.51.100.10"
	tr := newLoginThrottle()

	// Default threshold (5): not blocked below it, blocked at it.
	for i := 0; i < defaultTimes-1; i++ {
		tr.Fail(ip)
	}
	if tr.Blocked(ip) {
		t.Fatalf("must not be blocked before %d failures", defaultTimes)
	}
	tr.Fail(ip)
	if !tr.Blocked(ip) {
		t.Fatalf("must be blocked once %d failures accumulate", defaultTimes)
	}
	tr.Reset(ip)
	if tr.Blocked(ip) {
		t.Fatal("reset must clear the counter")
	}

	// A tightened setting takes effect immediately.
	setLoginThrottleSetting(t, conf.LoginMaxAttempts, "2", "5")
	tr2 := newLoginThrottle()
	const ip2 = "198.51.100.11"
	tr2.Fail(ip2)
	if tr2.Blocked(ip2) {
		t.Fatal("with threshold 2, one failure must not block")
	}
	tr2.Fail(ip2)
	if !tr2.Blocked(ip2) {
		t.Fatal("with threshold 2, two failures must block")
	}
}

// TestPasswordLoginThrottleBlocksAfterThreshold: the password path (auth.go)
// locks out an IP after defaultTimes failures, and the next attempt is refused
// with 429 even when the credentials are CORRECT — proof the lockout wraps the
// password check rather than depending on it.
func TestPasswordLoginThrottleBlocksAfterThreshold(t *testing.T) {
	mustAuthNUser(t, "pw-throttle", model.GENERAL, false)
	const ip = "198.51.100.20"
	passwordThrottle.Reset(ip)
	t.Cleanup(func() { passwordThrottle.Reset(ip) })

	for i := 0; i < defaultTimes; i++ {
		w, resp := doLoginFromIP(t, "pw-throttle", "wrong-password", ip)
		if resp.Code == 429 {
			t.Fatalf("attempt %d must be a plain rejection, not yet throttled: http=%d", i+1, w.Code)
		}
	}
	// Correct credentials now, but the IP is locked out.
	w, resp := doLoginFromIP(t, "pw-throttle", "pw-pw-throttle", ip)
	if resp.Code != 429 {
		t.Fatalf("after %d failures the next attempt must be 429 even if valid, got http=%d code=%d body=%s",
			defaultTimes, w.Code, resp.Code, w.Body.String())
	}
	if resp.Data.Token != "" {
		t.Fatal("a throttled attempt must never return a token")
	}
}

// TestPasswordLoginSuccessResetsThrottle: a few failures short of the threshold
// followed by a success must clear the counter, so an honest user who mistypes
// then succeeds is not one failure away from a lockout on their next session.
func TestPasswordLoginSuccessResetsThrottle(t *testing.T) {
	mustAuthNUser(t, "pw-reset", model.GENERAL, false)
	const ip = "198.51.100.21"
	passwordThrottle.Reset(ip)
	t.Cleanup(func() { passwordThrottle.Reset(ip) })

	// Stay strictly below the threshold with wrong attempts.
	for i := 0; i < defaultTimes-1; i++ {
		if _, resp := doLoginFromIP(t, "pw-reset", "nope", ip); resp.Code == 429 {
			t.Fatalf("attempt %d must not be throttled below the threshold", i+1)
		}
	}
	// A success clears the counter.
	if w, resp := doLoginFromIP(t, "pw-reset", "pw-pw-reset", ip); resp.Code != 200 {
		t.Fatalf("correct credentials below the threshold must succeed, got http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
	// Fresh budget: another wrong attempt is a plain 400, not a lockout.
	if _, resp := doLoginFromIP(t, "pw-reset", "nope", ip); resp.Code == 429 {
		t.Fatal("a success must have reset the failure counter")
	}
}

// TestPasswordLoginDefaultLenient: the shipped default (5 attempts) must never
// trip on ordinary use — a single mistyped password is a clean 400, not a 429.
func TestPasswordLoginDefaultLenient(t *testing.T) {
	mustAuthNUser(t, "pw-lenient", model.GENERAL, false)
	const ip = "198.51.100.22"
	passwordThrottle.Reset(ip)
	t.Cleanup(func() { passwordThrottle.Reset(ip) })

	w, resp := doLoginFromIP(t, "pw-lenient", "oops", ip)
	if resp.Code == 429 {
		t.Fatalf("a single wrong attempt must not be throttled under the lenient default, got http=%d", w.Code)
	}
}
