package hostcap

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/op"
)

// This is the W3 hostcap.VerifyAppPassword security proof: a real app
// password minted through op.CreateAppPassword resolves to a usable Principal
// (usable meaning host.FS honors it exactly like a real login would), the
// grant gate refuses without CapAuth, and a wrong/unknown credential is
// refused — mirroring the shape of the VerifyToken proof
// (auth_verifytoken_test.go).

func TestVerifyAppPasswordGrantGate(t *testing.T) {
	nocap := New(Grant{PluginID: "p.vap.nocap"}, Options{})
	if _, err := nocap.Auth().VerifyAppPassword(context.Background(), "anyone", "anything"); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyAppPassword without CapAuth: want ErrPermissionDenied, got %v", err)
	}
}

func TestVerifyAppPasswordValidPrincipalUsableWithFS(t *testing.T) {
	mnt := "/vap-valid"
	mountLocal(t, mnt)
	u := mkVerifyTokenUser(t, "vap-valid-user", mnt, false)

	plaintext, _, err := op.CreateAppPassword(u.ID, "test client")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}

	h := New(Grant{PluginID: "p.vap.valid", HostAPIs: []GrantItem{{Name: CapAuth}, {Name: CapFS}}}, Options{})
	prin, err := h.Auth().VerifyAppPassword(context.Background(), u.Username, plaintext)
	if err != nil {
		t.Fatalf("VerifyAppPassword valid: %v", err)
	}
	if prin == "" {
		t.Fatal("VerifyAppPassword valid: got empty principal")
	}
	objs, err := h.FS().List(context.Background(), prin, ext.ObjRef{Anchor: mnt}, ext.ListArgs{})
	if err != nil {
		t.Fatalf("FS.List with VerifyAppPassword principal: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 entries (seed.txt, sub), got %d: %+v", len(objs), objs)
	}
}

func TestVerifyAppPasswordWrongDenied(t *testing.T) {
	u := mkVerifyTokenUser(t, "vap-wrong-user", "", false)
	if _, _, err := op.CreateAppPassword(u.ID, "unused"); err != nil {
		t.Fatalf("create app password: %v", err)
	}

	h := New(Grant{PluginID: "p.vap.wrong", HostAPIs: []GrantItem{{Name: CapAuth}}}, Options{})
	if _, err := h.Auth().VerifyAppPassword(context.Background(), u.Username, "alp_wrong-plaintext"); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyAppPassword wrong plaintext: want ErrPermissionDenied, got %v", err)
	}
}

func TestVerifyAppPasswordUnknownUserDenied(t *testing.T) {
	h := New(Grant{PluginID: "p.vap.unknown", HostAPIs: []GrantItem{{Name: CapAuth}}}, Options{})
	if _, err := h.Auth().VerifyAppPassword(context.Background(), "no-such-user-at-all", "alp_anything"); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyAppPassword unknown user: want ErrPermissionDenied, got %v", err)
	}
}

func TestVerifyAppPasswordDisabledUserDenied(t *testing.T) {
	u := mkVerifyTokenUser(t, "vap-disabled-user", "", true)
	plaintext, _, err := op.CreateAppPassword(u.ID, "unused")
	if err != nil {
		t.Fatalf("create app password: %v", err)
	}

	h := New(Grant{PluginID: "p.vap.disabled", HostAPIs: []GrantItem{{Name: CapAuth}}}, Options{})
	if _, err := h.Auth().VerifyAppPassword(context.Background(), u.Username, plaintext); err != ext.ErrPermissionDenied {
		t.Fatalf("VerifyAppPassword disabled user: want ErrPermissionDenied, got %v", err)
	}
}
