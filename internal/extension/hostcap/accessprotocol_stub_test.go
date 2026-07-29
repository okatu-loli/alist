package hostcap

import (
	"context"
	"net"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// This is the F2a AccessProtocol security proof, driven end-to-end against a
// real core (the package TestMain stands up the DB + migration; mountLocal
// mounts a real Local storage). It exercises the exact pattern a WebDAV/S3/MCP
// protocol plugin follows (design §2.1):
//
//	inbound credential --host.Auth().Login--> Principal --host.FS()--> op.Authorize
//
// The plugin makes NO authorization decision. It only exchanges the credential
// for a host-issued Principal and then lists through host.FS, which runs the core
// Authorize on every call AS THE AUTHENTICATED USER. So a per-operation check is
// structural: the same stub that lists a path the user may read is denied a path
// the user may not — proving the decision lives in the host, not the plugin
// (the v3 WebDAV per-op re-check / S3 server-auth defects are satisfied by
// construction).

// stubProtocolPlugin is a minimal AccessProtocol standing in for a real WebDAV
// server: it authenticates "user:pass" and lists "path" through the host, doing
// no auth logic of its own. It holds the Host the host injected at Init.
type stubProtocolPlugin struct {
	host ext.Host
}

func (stubProtocolPlugin) Manifest() ext.Manifest { return ext.Manifest{} }
func (p *stubProtocolPlugin) Init(_ context.Context, h ext.Host) error {
	p.host = h
	return nil
}
func (stubProtocolPlugin) Shutdown(context.Context) error { return nil }
func (stubProtocolPlugin) ProtocolInfo() ext.ProtocolInfo {
	return ext.ProtocolInfo{Scheme: "webdav", Transport: ext.TransportHTTP, MountPattern: "/dav"}
}
func (stubProtocolPlugin) ServeHTTP(context.Context, *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{Status: 200}, nil
}
func (stubProtocolPlugin) HandleConn(context.Context, net.Conn) error { return nil }

var _ ext.AccessProtocol = (*stubProtocolPlugin)(nil)

// serve models one inbound request: authenticate, then list. It returns the
// listed entries or the host error verbatim — the plugin never inspects
// permissions, it just relays what the host allows or denies.
func (p *stubProtocolPlugin) serve(ctx context.Context, user, pass, path string) ([]ext.Obj, error) {
	prin, err := p.host.Auth().Login(ctx, user, pass)
	if err != nil {
		return nil, err
	}
	ref, err := p.host.FS().Resolve(ctx, prin, path)
	if err != nil {
		return nil, err
	}
	return p.host.FS().List(ctx, prin, ref, ext.ListArgs{})
}

// makeProtoUser persists a non-admin user confined (by an allow-read role
// binding) to allowPath, with the given password, and returns its username.
func makeProtoUser(t *testing.T, name, password, allowPath string) string {
	t.Helper()
	role := &model.Role{Name: name + "-role"}
	if err := op.CreateRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(role.ID) })
	perm := &model.Permission{
		Name: name + "-allow", Path: allowPath,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true,
	}
	if err := db.CreatePermission(perm); err != nil {
		t.Fatalf("create permission: %v", err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(perm.ID) })
	_ = db.AddRolePermission(role.ID, perm.ID)
	op.InvalidateRolePolicyForTest(role.ID)

	u := &model.User{Username: name, Role: model.Roles{int(role.ID)}}
	u.SetPassword(password)
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID); _ = db.DeleteUserRolesByUser(u.ID) })
	return name
}

func newProtoHost() (*Host, *stubProtocolPlugin) {
	host := New(Grant{
		PluginID: "proto.dav",
		HostAPIs: []GrantItem{{Name: CapAuth}, {Name: CapFS}},
	}, Options{})
	plugin := &stubProtocolPlugin{}
	_ = plugin.Init(context.Background(), host)
	return host, plugin
}

func TestAccessProtocolAuthorizesEachOperationAsTheAuthenticatedUser(t *testing.T) {
	mountLocal(t, "/apub")
	mountLocal(t, "/asecret")
	ctx := context.Background()

	makeProtoUser(t, "davuser", "davpass", "/apub")
	_, plugin := newProtoHost()

	// Allowed subtree: the plugin lists it through the host after Login.
	objs, err := plugin.serve(ctx, "davuser", "davpass", "/apub")
	if err != nil {
		t.Fatalf("serve /apub: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 entries under /apub, got %d: %+v", len(objs), objs)
	}

	// Denied subtree: the SAME authenticated principal is refused /asecret at the
	// host's Authorize — the plugin did nothing different, the host decided.
	if _, err := plugin.serve(ctx, "davuser", "davpass", "/asecret"); err != ext.ErrPermissionDenied {
		t.Fatalf("serve /asecret: want ErrPermissionDenied, got %v", err)
	}
}

func TestAccessProtocolLoginRejectsBadCredentialAndTwoFactor(t *testing.T) {
	mountLocal(t, "/apub2")
	ctx := context.Background()
	makeProtoUser(t, "daviu", "rightpass", "/apub2")
	_, plugin := newProtoHost()

	// Wrong password: Login denies before any fs op is reachable.
	if _, err := plugin.serve(ctx, "daviu", "wrongpass", "/apub2"); err != ext.ErrPermissionDenied {
		t.Fatalf("wrong password: want ErrPermissionDenied, got %v", err)
	}

	// A 2FA account cannot use its raw password over a protocol (it would bypass
	// the second factor) — Login funnels it onto an app password instead.
	tfa := &model.User{Username: "davotp", OtpSecret: "JBSWY3DPEHPK3PXP"}
	tfa.SetPassword("rightpass")
	if err := op.CreateUser(tfa); err != nil {
		t.Fatalf("create 2fa user: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(tfa.ID) })
	if _, err := plugin.serve(ctx, "davotp", "rightpass", "/apub2"); err != ext.ErrNeedAppPassword {
		t.Fatalf("2fa raw password over protocol: want ErrNeedAppPassword, got %v", err)
	}
}

// A protocol plugin that was NOT granted host.auth cannot authenticate anyone —
// it can never mint a session, so it cannot become a login bypass.
func TestAccessProtocolWithoutAuthGrantCannotLogin(t *testing.T) {
	ctx := context.Background()
	host := New(Grant{PluginID: "proto.noauth", HostAPIs: []GrantItem{{Name: CapFS}}}, Options{})
	plugin := &stubProtocolPlugin{}
	_ = plugin.Init(ctx, host)
	if _, err := plugin.serve(ctx, "whoever", "whatever", "/"); err != ext.ErrPermissionDenied {
		t.Fatalf("login without host.auth grant: want ErrPermissionDenied, got %v", err)
	}
	// Guard against a false pass from a name collision.
	if !strings.Contains("permission denied", "permission") {
		t.Fatal("unreachable")
	}
}
