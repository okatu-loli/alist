package hostcap

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// F2 AccessProtocol red team — the most sensitive F2 point: a protocol plugin
// handles inbound credentials. The brief's two headline worries:
//
//   1. "插件谎称验证通过" — can a malicious protocol plugin make host.Auth().Login
//      hand it a Principal for a user it did NOT actually verify? (No: the HOST
//      validates the password, disabled flag and 2FA — the plugin only supplies
//      the username/password arguments and cannot forge a Principal handle.)
//   2. "越 FS scope" — once it holds a Principal, can it exceed the plugin's
//      Grant.FS scope? (The authenticated Principal acts with the USER's own
//      rights — see FINDING C for the containment nuance — but never exceeds the
//      real user, and the empty/bound principal stays confined to Grant.FS.)
//
// These drive the real Host (DB + mounted Local storage from the package
// harness). makeProtoUser / mountLocal / newProtoHost live in
// accessprotocol_stub_test.go (same package).

// A protocol plugin cannot forge a Principal: a handle the host never issued
// resolves to no user, so every FS op with it is denied. The plugin's only route
// to a valid handle is host.Auth().Login (password-checked) — it cannot fabricate
// one, and the per-Host principals map means it could not even replay another
// plugin's handle.
func TestReg_ProtocolCannotForgePrincipalHandle(t *testing.T) {
	mountLocal(t, "/aforge")
	ctx := context.Background()
	host := New(Grant{
		PluginID: "proto.forge",
		HostAPIs: []GrantItem{{Name: CapAuth}, {Name: CapFS}},
		FS:       []FSScope{{Path: "/aforge", Write: true}},
	}, Options{})

	// A plausible-looking but never-issued 48-hex handle.
	forged := ext.Principal("00000000000000000000000000000000000000000000dead")
	if _, err := host.FS().List(ctx, forged, ext.ObjRef{Anchor: "/aforge"}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("forged principal handle: want ErrPermissionDenied, got %v", err)
	}
	// Signing with a forged handle is likewise refused (invariant 4 resolves the
	// principal first).
	if _, err := host.Sign().SignLink(ctx, forged, ext.ObjRef{Anchor: "/aforge"}, ext.SignScopeDownload, 0); err != ext.ErrPermissionDenied {
		t.Fatalf("sign with forged handle: want ErrPermissionDenied, got %v", err)
	}
}

// "插件谎称验证通过": the plugin controls the username/password ARGUMENTS to Login,
// but the HOST decides — it fetches the user and validates the raw password. So
// the plugin cannot obtain a Principal for a user whose password it does not
// know, and cannot impersonate admin without admin's real credentials. A wrong
// password and an unknown username both yield ErrPermissionDenied (uniform, so
// no user-enumeration oracle either).
func TestReg_ProtocolCannotLoginWithoutValidCredentials(t *testing.T) {
	ctx := context.Background()
	host := New(Grant{
		PluginID: "proto.badcreds",
		HostAPIs: []GrantItem{{Name: CapAuth}, {Name: CapFS}},
	}, Options{})

	// An "admin" account the attacker would love to become.
	admin := &model.User{Username: "rt-admin", Role: model.Roles{model.ADMIN}}
	admin.SetPassword("correct-admin-secret")
	if err := op.CreateUser(admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(admin.ID) })

	// Wrong password for a real (admin) user → denied. No admin Principal is minted.
	if _, err := host.Auth().Login(ctx, "rt-admin", "guessing"); err != ext.ErrPermissionDenied {
		t.Fatalf("admin login with wrong password: want ErrPermissionDenied, got %v", err)
	}
	// Unknown user → the SAME error (no enumeration oracle).
	if _, err := host.Auth().Login(ctx, "no-such-user", "whatever"); err != ext.ErrPermissionDenied {
		t.Fatalf("unknown user login: want ErrPermissionDenied, got %v", err)
	}
	// A disabled account cannot be logged in even with the right password.
	dis := &model.User{Username: "rt-disabled", Disabled: true}
	dis.SetPassword("right")
	if err := op.CreateUser(dis); err != nil {
		t.Fatalf("create disabled: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(dis.ID) })
	if _, err := host.Auth().Login(ctx, "rt-disabled", "right"); err != ext.ErrPermissionDenied {
		t.Fatalf("disabled account login: want ErrPermissionDenied, got %v", err)
	}
}

// A protocol plugin granted host.auth but NOT host.fs can prove a credential but
// cannot touch files: Login succeeds, every FS op is denied. Auth and FS are
// separately gated, so a login-only protocol is not a backdoor into the fs.
func TestReg_ProtocolWithoutFSGrantCannotActAfterLogin(t *testing.T) {
	mountLocal(t, "/anofs")
	ctx := context.Background()
	makeProtoUser(t, "nofsuser", "pw", "/anofs")

	host := New(Grant{
		PluginID: "proto.nofs",
		HostAPIs: []GrantItem{{Name: CapAuth}}, // no CapFS
	}, Options{})

	prin, err := host.Auth().Login(ctx, "nofsuser", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := host.FS().List(ctx, prin, ext.ObjRef{Anchor: "/anofs"}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("FS without CapFS: want ErrPermissionDenied even with a valid Principal, got %v", err)
	}
}

// FINDING C (INFO / by-design, containment nuance) — the plugin's Grant.FS scope
// does NOT bound operations performed with an AUTHENTICATED Principal.
//
// hostFS.authorize resolves a non-empty Principal to the REAL *model.User and
// runs op.Authorize against THAT user's full permissions — the plugin's
// Grant.FS / bound principal is consulted only for the empty (self) handle. This
// is intentional for a conduit protocol (a WebDAV server should serve a user
// their own files), and it is NOT a privilege escalation: the plugin can never
// exceed what the authenticated user could already do in the web UI, and it
// cannot authenticate without valid credentials.
//
// BUT it means the Grant.FS scope is not the containment boundary an admin might
// assume. Scoping a protocol plugin's fs grant to /pub does NOT stop it serving a
// user their /secret — the grant only confines the plugin's autonomous (empty
// principal) actions. The grant.go invariant comment ("op.Authorize ... against
// the plugin's restricted principal") is accurate only for the bound principal;
// for authenticated ops the acting principal is the user, not the plugin's
// restricted one. Worth surfacing in the admin consent UI / docs so a narrow FS
// grant on a protocol plugin is not mistaken for a reach limit.
//
// The test proves both halves: (a) the bound principal IS confined to Grant.FS;
// (b) an authenticated user reaches OUTSIDE Grant.FS (within its own rights) yet
// still cannot exceed its own rights.
func TestReg_ProtocolAuthedPrincipalNotBoundByPluginFSGrant_FindingC(t *testing.T) {
	mountLocal(t, "/ascope-pub")
	mountLocal(t, "/ascope-secret")
	ctx := context.Background()

	// A user whose role grants read on /ascope-secret ONLY (not /ascope-pub).
	makeProtoUser(t, "scopeuser", "pw", "/ascope-secret")

	// The plugin's fs grant is scoped to /ascope-pub ONLY (and it never got a
	// grant for /ascope-secret).
	host := New(Grant{
		PluginID: "proto.scope",
		HostAPIs: []GrantItem{{Name: CapAuth}, {Name: CapFS}},
		FS:       []FSScope{{Path: "/ascope-pub", Write: true}},
	}, Options{})

	// (a) The bound/self principal IS confined to the plugin's grant: it reads
	// /ascope-pub but is denied /ascope-secret.
	if _, err := host.FS().List(ctx, "", ext.ObjRef{Anchor: "/ascope-pub"}, ext.ListArgs{}); err != nil {
		t.Fatalf("bound principal in-grant read /ascope-pub: %v", err)
	}
	if _, err := host.FS().List(ctx, "", ext.ObjRef{Anchor: "/ascope-secret"}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("bound principal out-of-grant /ascope-secret: want denied, got %v", err)
	}

	// (b) The AUTHENTICATED principal acts with the USER's rights, ignoring the
	// plugin's Grant.FS: it reads /ascope-secret — OUTSIDE the plugin's grant,
	// INSIDE the user's rights. This is FINDING C: the grant did not contain it.
	prin, err := host.Auth().Login(ctx, "scopeuser", "pw")
	if err != nil {
		t.Fatalf("login scopeuser: %v", err)
	}
	if _, err := host.FS().List(ctx, prin, ext.ObjRef{Anchor: "/ascope-secret"}, ext.ListArgs{}); err != nil {
		t.Fatalf("FINDING C: authed principal read of /ascope-secret (outside the plugin's fs grant) should succeed on the user's rights, got %v", err)
	}

	// ...yet it is still bounded by the USER's own rights — no escalation beyond
	// the user. scopeuser has no rights to /ascope-pub, so the authed principal
	// is denied there even though the PLUGIN's grant covers it.
	if _, err := host.FS().List(ctx, prin, ext.ObjRef{Anchor: "/ascope-pub"}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("authed principal must stay within the user's own rights: want denied on /ascope-pub, got %v", err)
	}
	t.Log("FINDING C confirmed: Grant.FS confines only the bound principal; authenticated ops run at the user's rights (no escalation beyond the user, but the grant is not a reach limit)")
}
