package extension

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// stubHTTPProtocol is an in-process TransportHTTP AccessProtocol that echoes the
// request URL, standing in for WebDAV/S3/MCP.
type stubHTTPProtocol struct{}

func (stubHTTPProtocol) Manifest() ext.Manifest               { return ext.Manifest{} }
func (stubHTTPProtocol) Init(context.Context, ext.Host) error { return nil }
func (stubHTTPProtocol) Shutdown(context.Context) error       { return nil }
func (stubHTTPProtocol) ProtocolInfo() ext.ProtocolInfo {
	return ext.ProtocolInfo{Scheme: "webdav", Transport: ext.TransportHTTP, MountPattern: "/dav"}
}
func (stubHTTPProtocol) ServeHTTP(_ context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{Status: 207, Body: []byte("dav:" + req.URL)}, nil
}
func (stubHTTPProtocol) HandleConn(context.Context, net.Conn) error {
	return ext.NewError(ext.ErrCodeNotImplement, "http transport")
}

// stubConnProtocol is a TransportConn AccessProtocol (FTP/SFTP shape): it signals
// each accepted connection on got and echoes a byte so the test can confirm the
// host handed it a live conn.
type stubConnProtocol struct {
	got chan net.Conn
}

func (stubConnProtocol) Manifest() ext.Manifest               { return ext.Manifest{} }
func (stubConnProtocol) Init(context.Context, ext.Host) error { return nil }
func (stubConnProtocol) Shutdown(context.Context) error       { return nil }
func (stubConnProtocol) ProtocolInfo() ext.ProtocolInfo {
	// DefaultPort 0 → OS-assigned; the test reads the bound addr via Addr().
	return ext.ProtocolInfo{Scheme: "ftp", Transport: ext.TransportConn, DefaultPort: 0}
}
func (stubConnProtocol) ServeHTTP(context.Context, *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return nil, ext.NewError(ext.ErrCodeNotImplement, "conn transport")
}
func (s stubConnProtocol) HandleConn(_ context.Context, conn net.Conn) error {
	s.got <- conn
	_, _ = conn.Write([]byte("ok"))
	return nil
}

func accessProtoEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindAccessProtocol}},
		Kind:     ext.KindAccessProtocol,
		enabled:  true,
	}
}

func TestDispatchAccessProtocolHTTP(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(accessProtoEntry("proto.dav")); err != nil {
		t.Fatal(err)
	}
	RegisterAccessProtocolInstance("proto.dav", stubHTTPProtocol{})
	t.Cleanup(func() { RegisterAccessProtocolInstance("proto.dav", nil) })

	resp, err := DispatchAccessProtocolHTTP(context.Background(), r, "proto.dav",
		&ext.HTTPRequest{Method: "PROPFIND", URL: "/dav/dir"})
	if err != nil {
		t.Fatalf("DispatchAccessProtocolHTTP: %v", err)
	}
	if resp.Status != 207 || string(resp.Body) != "dav:/dav/dir" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
}

func TestDispatchAccessProtocolHTTPDisabledOrUnknown(t *testing.T) {
	r := NewRegistry()
	// Unknown id.
	if _, err := DispatchAccessProtocolHTTP(context.Background(), r, "nope",
		&ext.HTTPRequest{URL: "/x"}); err != ErrRouteNotDeclared {
		t.Fatalf("unknown protocol: want ErrRouteNotDeclared, got %v", err)
	}
	// Disabled entry is not reachable.
	e := accessProtoEntry("proto.off")
	e.enabled = false
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterAccessProtocolInstance("proto.off", stubHTTPProtocol{})
	t.Cleanup(func() { RegisterAccessProtocolInstance("proto.off", nil) })
	if _, err := DispatchAccessProtocolHTTP(context.Background(), r, "proto.off",
		&ext.HTTPRequest{URL: "/x"}); err != ErrRouteNotDeclared {
		t.Fatalf("disabled protocol: want ErrRouteNotDeclared, got %v", err)
	}
}

// TestDispatchAccessProtocolHTTPMultiKindManifestUsesManifestTruthSource
// mirrors TestInvokeActionMultiKindManifestUsesManifestTruthSource: an entry
// registered under a different primary Kind (Kind=StorageDriver) whose
// Manifest additionally declares AccessProtocol (Kinds: [StorageDriver,
// AccessProtocol]) never gets an extraKinds(AccessProtocol) entry —
// AccessProtocol has no FeatureBundle expansion path — so e.HasKind alone
// would wrongly 404 it. DeclaresKind must let it through.
func TestDispatchAccessProtocolHTTPMultiKindManifestUsesManifestTruthSource(t *testing.T) {
	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{ID: "proto.multikind", Kinds: []ext.Kind{ext.KindStorageDriver, ext.KindAccessProtocol}},
		Kind:     ext.KindStorageDriver, // registered/indexed under its primary Kind
		enabled:  true,
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterAccessProtocolInstance("proto.multikind", stubHTTPProtocol{})
	t.Cleanup(func() { RegisterAccessProtocolInstance("proto.multikind", nil) })

	resp, err := DispatchAccessProtocolHTTP(context.Background(), r, "proto.multikind",
		&ext.HTTPRequest{Method: "PROPFIND", URL: "/dav/dir"})
	if err != nil {
		t.Fatalf("DispatchAccessProtocolHTTP to multi-Kind manifest: %v", err)
	}
	if resp.Status != 207 || string(resp.Body) != "dav:/dav/dir" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
}

// A TransportConn protocol must not be reachable through the HTTP dispatch path.
func TestDispatchAccessProtocolHTTPRejectsConnTransport(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(accessProtoEntry("proto.ftp")); err != nil {
		t.Fatal(err)
	}
	RegisterAccessProtocolInstance("proto.ftp", stubConnProtocol{got: make(chan net.Conn, 1)})
	t.Cleanup(func() { RegisterAccessProtocolInstance("proto.ftp", nil) })
	_, err := DispatchAccessProtocolHTTP(context.Background(), r, "proto.ftp", &ext.HTTPRequest{URL: "/x"})
	if code, ok := ext.CodeOf(err); !ok || code != ext.ErrCodeNotSupport {
		t.Fatalf("conn protocol via HTTP dispatch: want NOT_SUPPORT, got %v", err)
	}
}

// The host owns the listener for a TransportConn protocol and hands each accepted
// conn to the plugin's HandleConn.
func TestProtocolManagerListensAndHandsConn(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(accessProtoEntry("proto.ftp2")); err != nil {
		t.Fatal(err)
	}
	stub := stubConnProtocol{got: make(chan net.Conn, 1)}
	RegisterAccessProtocolInstance("proto.ftp2", stub)
	t.Cleanup(func() { RegisterAccessProtocolInstance("proto.ftp2", nil) })

	m := NewProtocolManager(r)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })

	addr := m.Addr("proto.ftp2")
	if addr == nil {
		t.Fatal("protocol listener not bound")
	}
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial protocol listener: %v", err)
	}
	defer conn.Close()

	select {
	case c := <-stub.got:
		if c == nil {
			t.Fatal("HandleConn received a nil conn")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleConn was not invoked with the accepted conn")
	}
	// The plugin wrote back on the conn.
	buf := make([]byte, 2)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read back from HandleConn: buf=%q err=%v", buf, err)
	}
}

// A panic in HandleConn is isolated: the listener keeps accepting and a second
// connection is still served.
func TestProtocolManagerHandleConnPanicIsolated(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(accessProtoEntry("proto.panic")); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	served := make(chan struct{}, 2)
	stub := panicOnceProtocol{served: served, once: &once}
	RegisterAccessProtocolInstance("proto.panic", stub)
	t.Cleanup(func() { RegisterAccessProtocolInstance("proto.panic", nil) })

	m := NewProtocolManager(r)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })

	addr := m.Addr("proto.panic").String()
	// First conn panics; second must still be served (isolation).
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_ = c.Close()
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("second connection was not served after the first panicked")
	}
}

type panicOnceProtocol struct {
	served chan struct{}
	once   *sync.Once
}

func (panicOnceProtocol) Manifest() ext.Manifest               { return ext.Manifest{} }
func (panicOnceProtocol) Init(context.Context, ext.Host) error { return nil }
func (panicOnceProtocol) Shutdown(context.Context) error       { return nil }
func (panicOnceProtocol) ProtocolInfo() ext.ProtocolInfo {
	return ext.ProtocolInfo{Scheme: "ftp", Transport: ext.TransportConn}
}
func (panicOnceProtocol) ServeHTTP(context.Context, *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return nil, ext.NewError(ext.ErrCodeNotImplement, "conn transport")
}
func (p panicOnceProtocol) HandleConn(_ context.Context, _ net.Conn) error {
	first := false
	p.once.Do(func() { first = true })
	if first {
		panic("boom")
	}
	p.served <- struct{}{}
	return nil
}

// grpcAccessProtoEntry builds a declared, enabled, ExecGRPC Kind=AccessProtocol
// entry — the shape resolveAccessProtocol requires to even attempt the
// factory branch. Mirrors grpcHookEntry (eventhook_test.go) one Kind over.
func grpcAccessProtoEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindAccessProtocol}},
		Kind:     ext.KindAccessProtocol,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-accessprotocol",
	}
}

// TestResolveAccessProtocolUsesFactory pins the gRPC-dispatch seam: an
// ExecGRPC entry with a binPath and no in-process instance must resolve
// through the installed accessProtocolFactory (the plugins.go init wires this
// to pluginproc.Default.AcquireAccessProtocol). Without a factory the same
// entry resolves to nil (no host-side handler). Mirrors
// TestResolveEventHookUsesFactory one Kind over.
func TestResolveAccessProtocolUsesFactory(t *testing.T) {
	saved := accessProtocolFactory
	t.Cleanup(func() { accessProtocolFactory = saved })

	e := grpcAccessProtoEntry("proto.grpc")

	// No factory wired → no host-side handler → nil.
	accessProtocolFactory = nil
	if p, _ := resolveAccessProtocol(e); p != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", p)
	}

	// Factory wired → the ExecGRPC branch is hit and its protocol is returned.
	stub := stubHTTPProtocol{}
	var released bool
	accessProtocolFactory = func(got *Entry) (ext.AccessProtocol, func(), error) {
		if got.ID() != "proto.grpc" || got.BinPath() != "/bin/example-accessprotocol" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	p, release := resolveAccessProtocol(e)
	if p != stub {
		t.Fatalf("resolveAccessProtocol must return the factory's protocol, got %T", p)
	}
	release()
	if !released {
		t.Fatal("resolveAccessProtocol must return the factory's release func")
	}
}

// TestDispatchAccessProtocolHTTPUsesGRPCResolvedProtocol proves
// DispatchAccessProtocolHTTP's factory branch is wired end to end: a
// declared, ExecGRPC entry with no in-process instance still serves the
// request, through accessProtocolFactory.
func TestDispatchAccessProtocolHTTPUsesGRPCResolvedProtocol(t *testing.T) {
	saved := accessProtocolFactory
	t.Cleanup(func() { accessProtocolFactory = saved })

	r := NewRegistry()
	e := grpcAccessProtoEntry("proto.grpc.dispatch")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	var released bool
	accessProtocolFactory = func(got *Entry) (ext.AccessProtocol, func(), error) {
		return stubHTTPProtocol{}, func() { released = true }, nil
	}

	resp, err := DispatchAccessProtocolHTTP(context.Background(), r, "proto.grpc.dispatch",
		&ext.HTTPRequest{Method: "PROPFIND", URL: "/dav/z"})
	if err != nil {
		t.Fatalf("DispatchAccessProtocolHTTP: %v", err)
	}
	if resp.Status != 207 || string(resp.Body) != "dav:/dav/z" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if !released {
		t.Fatal("DispatchAccessProtocolHTTP must call the factory's release func")
	}
}
