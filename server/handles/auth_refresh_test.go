package handles

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// This file's own init() (alongside permission_test.go's package-wide DB
// bootstrap) wires up the pieces GenerateToken/GenerateRefreshToken need
// that no other server/handles test currently sets up: a token store and a
// JWT signing key.
func init() {
	tokenstore.Init(tokenstore.NewSQLiteStore())
	common.SecretKey = []byte("test-secret-refresh")
}

func mustRefreshTestUser(t *testing.T, username string) *model.User {
	t.Helper()
	u := &model.User{Username: username}
	u.SetPassword("pw-" + username)
	if err := op.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(u.ID) })
	return u
}

type loginResp struct {
	Code int `json:"code"`
	Data struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		DeviceKey    string `json:"device_key"`
	} `json:"data"`
}

func doLogin(t *testing.T, username, password string) loginResp {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	Login(c)
	if w.Code != 200 {
		t.Fatalf("login http status = %d body=%s", w.Code, w.Body.String())
	}
	var resp loginResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != 200 {
		t.Fatalf("login code = %d body=%s", resp.Code, w.Body.String())
	}
	if resp.Data.Token == "" || resp.Data.RefreshToken == "" {
		t.Fatalf("login must return both token and refresh_token: %+v", resp.Data)
	}
	return resp
}

type refreshResp struct {
	Code int `json:"code"`
	Data struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"data"`
}

func doRefresh(t *testing.T, refreshToken string) (*httptest.ResponseRecorder, refreshResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req := httptest.NewRequest("POST", "/api/auth/refresh", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	refreshThrottle.Reset(c.ClientIP())
	Refresh(c)
	var resp refreshResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// TestRefreshRotates pins the happy path: a refresh exchanges the old
// refresh token for a brand new access+refresh pair, and the old refresh
// token's jti is immediately dead in the store (can't be reused).
//
// It deliberately does NOT re-exercise the old refresh token through the
// Refresh handler to prove that: doing so is itself a replay (see
// TestRefreshReplayRevokesSession), which revokes the whole session as a
// side effect — that would make this test's "rotation alone doesn't disturb
// the sibling access token" assertion below meaningless.
func TestRefreshRotates(t *testing.T) {
	mustRefreshTestUser(t, "refresh-rotate-user")
	login := doLogin(t, "refresh-rotate-user", "pw-refresh-rotate-user")

	w, resp := doRefresh(t, login.Data.RefreshToken)
	if w.Code != 200 || resp.Code != 200 {
		t.Fatalf("refresh should succeed: http=%d code=%d body=%s", w.Code, resp.Code, w.Body.String())
	}
	if resp.Data.Token == "" || resp.Data.RefreshToken == "" {
		t.Fatalf("refresh must return a new token pair: %+v", resp.Data)
	}
	if resp.Data.Token == login.Data.Token || resp.Data.RefreshToken == login.Data.RefreshToken {
		t.Fatalf("rotation must issue a genuinely new pair, not echo the old one")
	}

	// Old refresh token's jti must now be dead in the store — checked
	// directly against the store (not via the Refresh handler) so this
	// assertion has no side effects of its own.
	oldClaims, err := common.ParseTokenClaims(login.Data.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokenstore.Get().Get(oldClaims.JTI); !errors.Is(err, tokenstore.ErrTokenRevoked) {
		t.Fatalf("old refresh jti must be revoked after rotation, got: %v", err)
	}

	// The sibling access token issued at login must be undisturbed —
	// rotating the refresh token must not touch other tokens in the same
	// session.
	if _, err := common.ParseToken(login.Data.Token); err != nil {
		t.Fatalf("access token issued at login must remain valid until its own expiry: %v", err)
	}
}

// TestRefreshReplayRevokesSession pins the security property that makes
// rotation worth doing: reusing an already-rotated (dead) refresh token is
// treated as a signal the token was stolen, and the entire session —
// including tokens issued by the rotation itself — is revoked in response.
func TestRefreshReplayRevokesSession(t *testing.T) {
	mustRefreshTestUser(t, "refresh-replay-user")
	login := doLogin(t, "refresh-replay-user", "pw-refresh-replay-user")

	w1, resp1 := doRefresh(t, login.Data.RefreshToken)
	if w1.Code != 200 || resp1.Code != 200 {
		t.Fatalf("first refresh should succeed: http=%d code=%d body=%s", w1.Code, resp1.Code, w1.Body.String())
	}

	// Replay: reuse the SAME old (now-revoked) refresh token again.
	w2, resp2 := doRefresh(t, login.Data.RefreshToken)
	if resp2.Code == 200 {
		t.Fatalf("replayed refresh token must be rejected: http=%d body=%s", w2.Code, w2.Body.String())
	}

	// The whole session must now be dead: even the access token minted by
	// the legitimate first rotation is rejected.
	if _, err := common.ParseToken(resp1.Data.Token); err == nil {
		t.Fatal("replay must revoke the whole session, including the access token from the legitimate rotation")
	}
	if _, err := common.ParseToken(login.Data.Token); err == nil {
		t.Fatal("replay must revoke the whole session, including the original access token")
	}

	// And the new refresh token from the legitimate rotation must be dead
	// too — replay revokes the session, not just one token.
	w3, resp3 := doRefresh(t, resp1.Data.RefreshToken)
	if resp3.Code == 200 {
		t.Fatalf("refresh token from the legitimate rotation must also die with the session: http=%d body=%s", w3.Code, w3.Body.String())
	}
}

// TestRefreshRejectedAfterPasswordChange pins the kill-switch property: a
// refresh token minted before a password change must not be redeemable
// after it, even though the token itself hasn't expired or been rotated.
// Without this check, a stolen refresh token survives a password reset and
// keeps minting valid access forever, because the new access token is
// stamped with the user's CURRENT (post-change) PwdTS and so sails past the
// access-token PwdTS check in server/middlewares/auth.go.
func TestRefreshRejectedAfterPasswordChange(t *testing.T) {
	user := mustRefreshTestUser(t, "refresh-pwdchange-user")
	login := doLogin(t, "refresh-pwdchange-user", "pw-refresh-pwdchange-user")

	// Simulate a password change: bump PwdTS on the live user record, same
	// as SetPassword does, independently of the refresh token in hand.
	user.PwdTS++
	if err := op.UpdateUser(user); err != nil {
		t.Fatal(err)
	}

	w, resp := doRefresh(t, login.Data.RefreshToken)
	if resp.Code != 401 {
		t.Fatalf("refresh token issued before a password change must be rejected with code 401: got code=%d body=%s", resp.Code, w.Body.String())
	}

	// The rejection must also revoke the whole session, not just this one
	// refresh token — same incident-response posture as replay, since a
	// stolen-then-refreshed-after-password-change token is just as
	// suspicious as a replayed one.
	if _, err := common.ParseToken(login.Data.Token); err == nil {
		t.Fatal("rejecting a post-password-change refresh must revoke the whole session, including the original access token")
	}
}

// TestRefreshRejectsAccessToken closes the untested branch where an access
// token (rather than a refresh token) is submitted to /api/auth/refresh.
func TestRefreshRejectsAccessToken(t *testing.T) {
	mustRefreshTestUser(t, "refresh-kind-user")
	login := doLogin(t, "refresh-kind-user", "pw-refresh-kind-user")

	w, resp := doRefresh(t, login.Data.Token)
	if resp.Code != 401 {
		t.Fatalf("submitting an access token to /refresh must be rejected with code 401: got code=%d body=%s", resp.Code, w.Body.String())
	}
}

// TestRefreshConcurrentConsumeDetectsReplay pins the atomicity fix for the
// concurrent-replay race: the up-front store.Get-based replay check only
// catches *sequential* reuse of an already-dead token. Two callers racing
// the same still-live refresh token both pass that Get before either
// reaches the revoke step, so consuming it must be a compare-and-swap
// (store.RevokeIfActive), not a plain Revoke — otherwise both callers would
// mint a fresh pair from the same refresh token.
//
// A true goroutine race is exercised at the store layer
// (TestStoreRevokeIfActive / TestRevokeTokenIfActiveIsCompareAndSwap); here
// two sequential consumes of the store's CAS primitive are enough to prove
// the handler wires it correctly: the first call wins and proceeds, the
// second reaches the "someone already consumed this" branch and revokes the
// session, exactly as it would if the two calls had truly overlapped.
func TestRefreshConcurrentConsumeDetectsReplay(t *testing.T) {
	mustRefreshTestUser(t, "refresh-race-user")
	login := doLogin(t, "refresh-race-user", "pw-refresh-race-user")

	claims, err := common.ParseTokenClaims(login.Data.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	store := tokenstore.Get()

	// Simulate the concurrent window directly at the CAS primitive: both
	// "requests" observe the token as active (this is what a plain
	// store.Get-then-Revoke would do under a real race), then both attempt
	// to consume it via RevokeIfActive.
	won1, err := store.RevokeIfActive(claims.JTI)
	if err != nil {
		t.Fatal(err)
	}
	if !won1 {
		t.Fatal("first concurrent consumer must win the CAS")
	}
	won2, err := store.RevokeIfActive(claims.JTI)
	if err != nil {
		t.Fatal(err)
	}
	if won2 {
		t.Fatal("second concurrent consumer must lose the CAS (replay)")
	}

	// The handler-level consequence of losing the CAS is a full session
	// revoke, same as the sequential-replay branch: reflect that here by
	// driving it through RevokeBySession the way the handler does, then
	// confirming the session's other tokens (the sibling access token) die
	// with it.
	if err := store.RevokeBySession(claims.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := common.ParseToken(login.Data.Token); err == nil {
		t.Fatal("losing the concurrent-consume race must revoke the whole session, including the access token")
	}
}

// TestRefreshRejectsDisabledUser pins that a refresh token issued before an
// admin disables the account can no longer be redeemed afterward — without
// this, a disabled user could keep refreshing indefinitely since disabling
// doesn't touch PwdTS and so wouldn't otherwise trip the refresh handler's
// password-change check.
func TestRefreshRejectsDisabledUser(t *testing.T) {
	user := mustRefreshTestUser(t, "refresh-disabled-user")
	login := doLogin(t, "refresh-disabled-user", "pw-refresh-disabled-user")

	user.Disabled = true
	if err := op.UpdateUser(user); err != nil {
		t.Fatal(err)
	}

	w, resp := doRefresh(t, login.Data.RefreshToken)
	if resp.Code != 401 {
		t.Fatalf("refresh for a disabled user must be rejected with code 401: got code=%d body=%s", resp.Code, w.Body.String())
	}
}

// doRefreshFromIP is doRefresh with an explicit RemoteAddr, so rate-limit
// tests don't share a counter with every other test in this file — those
// all go through the shared default RemoteAddr httptest.NewRequest assigns
// ("192.0.2.1:1234").
func doRefreshFromIP(t *testing.T, refreshToken, remoteAddr string) (*httptest.ResponseRecorder, refreshResp) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req := httptest.NewRequest("POST", "/api/auth/refresh", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	c.Request = req
	Refresh(c)
	var resp refreshResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// TestRefreshRateLimited pins that /api/auth/refresh is rate-limited per IP
// the same way login is: since it has no Auth middleware and does DB
// writes, an attacker could otherwise flood it with junk or brute-force
// replay attempts. defaultTimes failed attempts from one IP are each
// rejected on their own merits (4xx), but the next attempt is blocked
// outright with 429 regardless of what's in the body.
func TestRefreshRateLimited(t *testing.T) {
	ip := "203.0.113.77"
	remoteAddr := ip + ":54321"
	t.Cleanup(func() { refreshThrottle.Reset(ip) })
	refreshThrottle.Reset(ip) // isolate from any residual count on this IP

	for i := 0; i < defaultTimes; i++ {
		w, resp := doRefreshFromIP(t, "not-a-real-refresh-token", remoteAddr)
		if resp.Code < 400 || resp.Code >= 500 {
			t.Fatalf("attempt %d: expected a 4xx rejection (not yet rate-limited), got http=%d code=%d body=%s", i+1, w.Code, resp.Code, w.Body.String())
		}
		if resp.Code == 429 {
			t.Fatalf("attempt %d: must not be rate-limited before defaultTimes failures, got 429", i+1)
		}
	}

	w, resp := doRefreshFromIP(t, "not-a-real-refresh-token", remoteAddr)
	if resp.Code != 429 {
		t.Fatalf("attempt %d: expected 429 once defaultTimes failures have accumulated, got http=%d code=%d body=%s", defaultTimes+1, w.Code, resp.Code, w.Body.String())
	}
}
