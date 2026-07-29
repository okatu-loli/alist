package hostcap

import (
	"context"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
)

// This is the W1 hostcap.VerifyToken security proof: a bearer token minted the
// same way server/common.GenerateToken does resolves to a usable Principal
// (usable meaning host.FS honors it exactly like a real login would), an
// unparsable/unsigned token is refused, and a disabled account's otherwise
// valid token is refused — mirroring the three checks
// server/middlewares.Auth applies on the HTTP path (ParseToken, then
// Disabled).

// mkVerifyTokenUser persists a user, optionally (allowPath != "") bound to an
// allow-read role on allowPath — the same role/permission shape
// makeProtoUser (accessprotocol_stub_test.go) uses — so a token issued for it
// can be proven usable against a real host.FS grant, not just non-empty.
func mkVerifyTokenUser(t *testing.T, username, allowPath string, disabled bool) *model.User {
	t.Helper()
	u := &model.User{Username: username, Disabled: disabled}
	if allowPath != "" {
		role := &model.Role{Name: username + "-role"}
		if err := op.CreateRole(role); err != nil {
			t.Fatalf("create role: %v", err)
		}
		perm := &model.Permission{
			Name: username + "-allow", Path: allowPath,
			Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true,
		}
		if err := db.CreatePermission(perm); err != nil {
			t.Fatalf("create permission: %v", err)
		}
		if err := db.AddRolePermission(role.ID, perm.ID); err != nil {
			t.Fatalf("add role permission: %v", err)
		}
		op.InvalidateRolePolicyForTest(role.ID)
		u.Role = model.Roles{int(role.ID)}
	}
	u.SetPassword("irrelevant-" + username)
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u
}

func TestVerifyTokenGrantGate(t *testing.T) {
	nocap := New(Grant{PluginID: "p.vt.nocap"}, Options{})
	if _, err := nocap.Auth().VerifyToken(context.Background(), "anything"); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyToken without CapAuth: want ErrPermissionDenied, got %v", err)
	}
}

func TestVerifyTokenValidPrincipalUsableWithFS(t *testing.T) {
	mnt := "/vt-valid"
	mountLocal(t, mnt)
	u := mkVerifyTokenUser(t, "vt-valid-user", mnt, false)

	tok, err := common.GenerateToken(u, "vt-valid-session")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	h := New(Grant{PluginID: "p.vt.valid", HostAPIs: []GrantItem{{Name: CapAuth}, {Name: CapFS}}}, Options{})
	prin, err := h.Auth().VerifyToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("VerifyToken valid: %v", err)
	}
	if prin == "" {
		t.Fatal("VerifyToken valid: got empty principal")
	}
	objs, err := h.FS().List(context.Background(), prin, ext.ObjRef{Anchor: mnt}, ext.ListArgs{})
	if err != nil {
		t.Fatalf("FS.List with VerifyToken principal: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 entries (seed.txt, sub), got %d: %+v", len(objs), objs)
	}
}

func TestVerifyTokenInvalidDenied(t *testing.T) {
	h := New(Grant{PluginID: "p.vt.invalid", HostAPIs: []GrantItem{{Name: CapAuth}}}, Options{})
	if _, err := h.Auth().VerifyToken(context.Background(), "not-a-real-token"); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyToken garbage: want ErrPermissionDenied, got %v", err)
	}
}

func TestVerifyTokenExpiredDenied(t *testing.T) {
	u := mkVerifyTokenUser(t, "vt-expired-user", "", false)
	claim := common.UserClaims{
		Username: u.Username,
		PwdTS:    u.PwdTS,
		JTI:      uuid.NewString(),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
	}
	tokStr, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claim).SignedString(common.SecretKey)
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}

	h := New(Grant{PluginID: "p.vt.expired", HostAPIs: []GrantItem{{Name: CapAuth}}}, Options{})
	if _, err := h.Auth().VerifyToken(context.Background(), tokStr); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyToken expired: want ErrPermissionDenied, got %v", err)
	}
}

func TestVerifyTokenDisabledUserDenied(t *testing.T) {
	u := mkVerifyTokenUser(t, "vt-disabled-user", "", true)
	tok, err := common.GenerateToken(u, "vt-disabled-session")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	h := New(Grant{PluginID: "p.vt.disabled", HostAPIs: []GrantItem{{Name: CapAuth}}}, Options{})
	if _, err := h.Auth().VerifyToken(context.Background(), tok); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyToken disabled user: want ErrPermissionDenied, got %v", err)
	}
}
