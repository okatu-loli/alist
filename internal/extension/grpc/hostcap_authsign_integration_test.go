package grpc

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
)

// This is the W1 reverse-bridge closer: a real plugin subprocess drives
// Host.Auth(), Host.Sign() and Host.FS().Put over the wire — the three legs
// that were notImplAuth{}/notImplSigner{}/NOT_IMPLEMENT before this change —
// proving the FULL ext.Host is now reachable from a subprocess, not just
// FS/Store/HTTP (see TestReverseBridgeFSAuthorize et al. in
// hostcap_integration_test.go, whose harness — launchProbe/serveHost/
// mountHostcapLocal — this file reuses).

// TestReverseBridgeAuthLogin: Login succeeds and mints a usable Principal when
// CapAuth is granted; a wrong password is denied by the host's own credential
// check, and the same call is denied without the grant (invariant 5).
func TestReverseBridgeAuthLogin(t *testing.T) {
	probe := launchProbe(t)
	ctx := context.Background()

	u := &model.User{Username: "rb-auth-user"}
	u.SetPassword("rb-auth-pass")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	granted := hostcap.New(hostcap.Grant{
		PluginID: "it.authlogin",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapAuth}},
	}, hostcap.Options{})
	gid := serveHost(probe, granted)

	prin, err := probe.Exercise(ctx, gid, "auth.login", "rb-auth-user", "rb-auth-pass")
	if err != nil {
		t.Fatalf("auth.login over the bridge: %v", err)
	}
	if prin == "" {
		t.Fatal("auth.login: got empty principal")
	}

	// Wrong password: the host's own check denies it, over the wire.
	if _, err := probe.Exercise(ctx, gid, "auth.login", "rb-auth-user", "wrong-pass"); err == nil {
		t.Fatal("auth.login wrong password: expected permission error, got nil")
	}

	// No CapAuth grant: denied even with the right credentials (invariant 5).
	nocap := hostcap.New(hostcap.Grant{PluginID: "it.authlogin.nocap"}, hostcap.Options{})
	nid := serveHost(probe, nocap)
	if _, err := probe.Exercise(ctx, nid, "auth.login", "rb-auth-user", "rb-auth-pass"); err == nil {
		t.Fatal("auth.login without CapAuth: expected permission error, got nil")
	}
}

// TestReverseBridgeAuthVerifyTokenUsableWithFS: a token minted the same way
// server/common.GenerateToken does resolves, over the bridge, to a Principal
// that is then usable with host.FS().List — proving hostcap's real VerifyToken
// wiring (not just the transport leg) is reachable from a subprocess.
func TestReverseBridgeAuthVerifyTokenUsableWithFS(t *testing.T) {
	mnt := "/rb-vt"
	mountHostcapLocal(t, mnt)
	probe := launchProbe(t)
	ctx := context.Background()

	role := &model.Role{Name: "rb-vt-role"}
	if err := op.CreateRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	perm := &model.Permission{Name: "rb-vt-allow", Path: mnt, Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	if err := db.CreatePermission(perm); err != nil {
		t.Fatalf("create permission: %v", err)
	}
	if err := db.AddRolePermission(role.ID, perm.ID); err != nil {
		t.Fatalf("attach role permission: %v", err)
	}
	op.InvalidateRolePolicyForTest(role.ID)

	u := &model.User{Username: "rb-vt-user", Role: model.Roles{int(role.ID)}}
	u.SetPassword("irrelevant")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	tok, err := common.GenerateToken(u, "rb-vt-session")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	h := hostcap.New(hostcap.Grant{
		PluginID: "it.verifytoken",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapAuth}, {Name: hostcap.CapFS}},
	}, hostcap.Options{})
	id := serveHost(probe, h)

	got, err := probe.Exercise(ctx, id, "auth.verifytoken.fs.list", tok, mnt)
	if err != nil {
		t.Fatalf("auth.verifytoken.fs.list: %v", err)
	}
	if got != "seed.txt,sub" {
		t.Fatalf("fs.list with verified-token principal = %q, want %q", got, "seed.txt,sub")
	}

	// An invalid token never yields a usable principal.
	if _, err := probe.Exercise(ctx, id, "auth.verifytoken.fs.list", "not-a-token", mnt); err == nil {
		t.Fatal("auth.verifytoken.fs.list with garbage token: expected permission error, got nil")
	}
}

// TestReverseBridgeSignRoundTrip: SignLink mints a token over the bridge, and
// VerifyLink round-trips it back to the same ref+scope — also over the bridge,
// in a fresh Exercise call, proving the token itself (not process state)
// carries the information.
func TestReverseBridgeSignRoundTrip(t *testing.T) {
	mnt := "/rb-sign"
	mountHostcapLocal(t, mnt)
	probe := launchProbe(t)
	ctx := context.Background()

	h := hostcap.New(hostcap.Grant{
		PluginID: "it.sign",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapSign}},
		FS:       []hostcap.FSScope{{Path: mnt}},
	}, hostcap.Options{})
	id := serveHost(probe, h)

	tok, err := probe.Exercise(ctx, id, "sign.link", mnt, "")
	if err != nil {
		t.Fatalf("sign.link: %v", err)
	}
	if tok == "" {
		t.Fatal("sign.link: got empty token")
	}

	got, err := probe.Exercise(ctx, id, "sign.verify", tok, "")
	if err != nil {
		t.Fatalf("sign.verify: %v", err)
	}
	want := mnt + ":download"
	if got != want {
		t.Fatalf("sign round trip = %q, want %q", got, want)
	}

	// No CapSign: denied.
	nosign := hostcap.New(hostcap.Grant{PluginID: "it.sign.nocap", FS: []hostcap.FSScope{{Path: mnt}}}, hostcap.Options{})
	nid := serveHost(probe, nosign)
	if _, err := probe.Exercise(ctx, nid, "sign.link", mnt, ""); err == nil {
		t.Fatal("sign.link without CapSign: expected permission error, got nil")
	}
}

// TestReverseBridgeFSPutStreamsBytes: a plugin subprocess streams a file's
// bytes to the host over FSPut (the plugin already holds them — the inverse of
// driver.proto's pull-model Put); the host writes them, and the returned Obj
// plus a follow-up listing both reflect the upload.
func TestReverseBridgeFSPutStreamsBytes(t *testing.T) {
	mnt := "/rb-put"
	mountHostcapLocal(t, mnt)
	probe := launchProbe(t)
	ctx := context.Background()

	rw := hostcap.New(hostcap.Grant{
		PluginID: "it.put",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: mnt, Write: true}},
	}, hostcap.Options{})
	id := serveHost(probe, rw)

	content := strings.Repeat("payload-from-a-plugin-upload-", 5000) // > one ChunkSize, forces multiple Chunk frames
	got, err := probe.Exercise(ctx, id, "fs.put", mnt, content)
	if err != nil {
		t.Fatalf("fs.put: %v", err)
	}
	wantReply := "put-by-plugin.txt:" + strconv.Itoa(len(content))
	if got != wantReply {
		t.Fatalf("fs.put reply = %q, want %q", got, wantReply)
	}

	// The listing (over the bridge) reflects the new file.
	listing, err := probe.Exercise(ctx, id, "fs.list", mnt, "")
	if err != nil {
		t.Fatalf("fs.list after put: %v", err)
	}
	if !strings.Contains(listing, "put-by-plugin.txt") {
		t.Fatalf("fs.list after put = %q, missing put-by-plugin.txt", listing)
	}

	// Read-only grant: fs.put is denied and nothing is written.
	ro := hostcap.New(hostcap.Grant{
		PluginID: "it.put.ro",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: mnt}},
	}, hostcap.Options{})
	roid := serveHost(probe, ro)
	if _, err := probe.Exercise(ctx, roid, "fs.put", mnt, "short"); err == nil {
		t.Fatal("fs.put with read-only grant: expected permission error, got nil")
	}
}
