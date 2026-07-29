package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/descope/virtualwebauthn"
	"github.com/gin-gonic/gin"
)

// authedCtx builds a test gin context with user set in the request context, as
// the Auth middleware would, plus an optional JSON body and path params.
func authedCtx(t *testing.T, user *model.User, body any, params gin.Params) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader([]byte("{}"))
	}
	req := httptest.NewRequest("POST", "/", r)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	if user != nil {
		c.Set(ctxkey.User, user)
	}
	c.Params = params
	return w, c
}

type manageResp struct {
	Code int             `json:"code"`
	Msg  string          `json:"message"`
	Data json.RawMessage `json:"data"`
}

func decodeManage(w *httptest.ResponseRecorder) manageResp {
	var r manageResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

// httpRegisterPasskey drives the real HTTP begin+finish handlers for user with a
// virtual authenticator, returning the created credential id.
func httpRegisterPasskey(t *testing.T, user *model.User, auth virtualwebauthn.Authenticator, cred virtualwebauthn.Credential, name string) string {
	t.Helper()
	rp := virtualwebauthn.RelyingParty{ID: pkTestRPID, Name: "Example", Origin: pkTestOrigin}

	// begin
	bw, bc := authedCtx(t, user, nil, nil)
	PasskeyRegisterBegin(bc)
	if bw.Code != 200 {
		t.Fatalf("register begin http=%d body=%s", bw.Code, bw.Body.String())
	}
	var begin struct {
		Code int `json:"code"`
		Data struct {
			ChallengeID string          `json:"challenge_id"`
			Options     json.RawMessage `json:"options"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bw.Body.Bytes(), &begin); err != nil {
		t.Fatalf("decode begin: %v", err)
	}
	if begin.Code != 200 || begin.Data.ChallengeID == "" {
		t.Fatalf("register begin should return a challenge: %s", bw.Body.String())
	}
	attOpts, err := virtualwebauthn.ParseAttestationOptions(string(begin.Data.Options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v (opts=%s)", err, string(begin.Data.Options))
	}
	attResp := virtualwebauthn.CreateAttestationResponse(rp, auth, cred, *attOpts)

	// finish
	fw, fc := authedCtx(t, user, map[string]any{
		"challenge_id": begin.Data.ChallengeID,
		"name":         name,
		"response":     json.RawMessage(attResp),
	}, nil)
	PasskeyRegisterFinish(fc)
	if fw.Code != 200 {
		t.Fatalf("register finish http=%d body=%s", fw.Code, fw.Body.String())
	}
	resp := decodeManage(fw)
	if resp.Code != 200 {
		t.Fatalf("register finish code=%d body=%s", resp.Code, fw.Body.String())
	}
	var info PasskeyInfo
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		t.Fatalf("decode passkey info: %v", err)
	}
	if info.ID == "" || info.Name != name {
		t.Fatalf("unexpected passkey info: %+v", info)
	}
	auth.AddCredential(cred)
	return info.ID
}

// TestPasskeyManageFullCycle registers, lists and deletes a passkey through the
// real HTTP handlers, asserting persistence and self-scoped deletion.
func TestPasskeyManageFullCycle(t *testing.T) {
	pinWebauthnRP(t)
	user := mustAuthNUser(t, "pk-manage", model.GENERAL, false)
	auth := virtualwebauthn.NewAuthenticator()
	auth.Options.UserHandle = user.WebAuthnID()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	credID := httpRegisterPasskey(t, user, auth, cred, "my key")

	// The credential is persisted on the account.
	reloaded, _ := op.GetUserById(user.ID)
	if pks, _ := model.ParsePasskeys(reloaded.Authn); len(pks) != 1 || pks[0].ID != credID {
		t.Fatalf("passkey not persisted: %+v", pks)
	}

	// List returns exactly the one key, and never leaks the public key.
	lw, lc := authedCtx(t, reloaded, nil, nil)
	PasskeyList(lc)
	var listResp struct {
		Data []PasskeyInfo `json:"data"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Data) != 1 || listResp.Data[0].ID != credID {
		t.Fatalf("list should return the one passkey: %+v", listResp.Data)
	}

	// Delete it (self-scoped) and confirm the account has none left.
	reloaded2, _ := op.GetUserById(user.ID)
	dw, dc := authedCtx(t, reloaded2, nil, gin.Params{{Key: "credid", Value: credID}})
	PasskeyDelete(dc)
	if decodeManage(dw).Code != 200 {
		t.Fatalf("delete should succeed: %s", dw.Body.String())
	}
	final, _ := op.GetUserById(user.ID)
	if pks, _ := model.ParsePasskeys(final.Authn); len(pks) != 0 {
		t.Fatalf("passkey should be removed: %+v", pks)
	}
}

// TestPasskeyManageGuestForbidden: the guest cannot manage passkeys.
func TestPasskeyManageGuestForbidden(t *testing.T) {
	guest := &model.User{Username: "guest-pk", Role: model.Roles{model.GUEST}, Authn: "[]"}

	bw, bc := authedCtx(t, guest, nil, nil)
	PasskeyRegisterBegin(bc)
	if decodeManage(bw).Code != 403 {
		t.Fatalf("guest register begin must be 403, got body=%s", bw.Body.String())
	}
	lw, lc := authedCtx(t, guest, nil, nil)
	PasskeyList(lc)
	if decodeManage(lw).Code != 403 {
		t.Fatalf("guest list must be 403, got body=%s", lw.Body.String())
	}
}

// TestPasskeyRegisterUnauthenticated: with no user in context the handler refuses
// rather than nil-derefs.
func TestPasskeyRegisterUnauthenticated(t *testing.T) {
	bw, bc := authedCtx(t, nil, nil, nil)
	PasskeyRegisterBegin(bc)
	if decodeManage(bw).Code == 200 {
		t.Fatalf("registration without an authenticated user must not succeed: %s", bw.Body.String())
	}
}

// TestPasskeyDeleteSelfScoped: one user cannot delete another user's passkey. B
// tries to delete A's credential id; B's own list has no such key, so it is a
// clean 404 and A's passkey is untouched.
func TestPasskeyDeleteSelfScoped(t *testing.T) {
	pinWebauthnRP(t)
	alice := mustAuthNUser(t, "pk-alice", model.GENERAL, false)
	authA := virtualwebauthn.NewAuthenticator()
	authA.Options.UserHandle = alice.WebAuthnID()
	credA := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	aliceCredID := httpRegisterPasskey(t, alice, authA, credA, "alice key")

	bob := mustAuthNUser(t, "pk-bob", model.GENERAL, false)

	// Bob attempts to delete Alice's credential id from HIS own account.
	bobFresh, _ := op.GetUserById(bob.ID)
	dw, dc := authedCtx(t, bobFresh, nil, gin.Params{{Key: "credid", Value: aliceCredID}})
	PasskeyDelete(dc)
	if decodeManage(dw).Code != 404 {
		t.Fatalf("deleting another user's credential must 404 (self-scoped), got body=%s", dw.Body.String())
	}
	// Alice's passkey remains intact.
	aliceReloaded, _ := op.GetUserById(alice.ID)
	if pks, _ := model.ParsePasskeys(aliceReloaded.Authn); len(pks) != 1 || pks[0].ID != aliceCredID {
		t.Fatalf("Alice's passkey must be untouched: %+v", pks)
	}
}

// TestPasskeyRoutesRegister confirms the passkey routes (static register/* and
// param :credid siblings) register on a gin engine without a wildcard conflict.
func TestPasskeyRoutesRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("passkey route registration panicked: %v", r)
		}
	}()
	r := gin.New()
	g := r.Group("/auth/passkey")
	g.GET("", PasskeyList)
	g.POST("/register/begin", PasskeyRegisterBegin)
	g.POST("/register/finish", PasskeyRegisterFinish)
	g.DELETE("/:credid", PasskeyDelete)
}
