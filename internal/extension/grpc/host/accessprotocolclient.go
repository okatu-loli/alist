package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// AccessProtocolClient is a host-side proxy over a gRPC AccessProtocol
// plugin's TransportHTTP half (W9a). It satisfies ext.AccessProtocol by
// forwarding ProtocolInfo/ServeHTTP over gRPC — mirrors APIRouteClient (the
// APIRoute proxy) one Kind over.
//
// Only TransportHTTP crosses the gRPC boundary (design §5): HandleConn's raw
// net.Conn is not serializable, so this client's HandleConn always returns a
// "native only" error rather than attempting a wire call — a gRPC
// AccessProtocol is structurally HTTP-transport-only.
type AccessProtocolClient struct {
	pc  *plugin.Client
	svc pb.AccessProtocolServiceClient

	manifest ext.Manifest
}

var _ ext.AccessProtocol = (*AccessProtocolClient)(nil)

// LaunchAccessProtocol starts the plugin binary described by cmd, performs
// the go-plugin handshake, dispenses the AccessProtocolService client, and
// reads the manifest. The returned client owns the subprocess; call Shutdown
// to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchAPIRoute (host/apirouteclient.go).
func LaunchAccessProtocol(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*AccessProtocolClient, error) {
	plugins := grpcplugin.AccessProtocolPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostAccessProtocolPluginMap(NewHostServiceServer(hostBuilder()))
	}
	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  grpcplugin.Handshake,
		Plugins:          plugins,
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           hclog.NewNullLogger(),
	})
	rpcClient, err := pc.Client()
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: accessprotocol plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.AccessProtocolPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.AccessProtocolPluginName, err)
	}
	svc, ok := raw.(pb.AccessProtocolServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an AccessProtocolServiceClient", raw)
	}
	c := &AccessProtocolClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *AccessProtocolClient) loadManifest(ctx context.Context) error {
	mr, err := c.svc.GetManifest(ctx, &pb.Empty{})
	if err != nil {
		return fmt.Errorf("host: GetManifest: %w", err)
	}
	if err := json.Unmarshal(mr.ManifestJson, &c.manifest); err != nil {
		return fmt.Errorf("host: decode manifest: %w", err)
	}
	return nil
}

// Exited reports whether the plugin subprocess has already died. See
// Client.Exited (host/client.go) for the rationale — same go-plugin reaper.
func (c *AccessProtocolClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *AccessProtocolClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *AccessProtocolClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *AccessProtocolClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *AccessProtocolClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.AccessProtocol ---

// ProtocolInfo decodes the wire ProtocolInfoReply back into ext.ProtocolInfo.
func (c *AccessProtocolClient) ProtocolInfo() ext.ProtocolInfo {
	ir, err := c.svc.ProtocolInfo(context.Background(), &pb.Empty{})
	if err != nil {
		return ext.ProtocolInfo{}
	}
	return ext.ProtocolInfo{
		Scheme:       ir.Scheme,
		Transport:    ext.Transport(ir.Transport),
		DefaultPort:  int(ir.DefaultPort),
		MountPattern: ir.MountPattern,
	}
}

func (c *AccessProtocolClient) ServeHTTP(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	reply, err := c.svc.ServeHTTP(ctx, &pb.HTTPDoRequest{
		Method: req.Method,
		Url:    req.URL,
		Header: pb.HeaderToProto(req.Header),
		Body:   req.Body,
	})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return &ext.HTTPResponse{
		Status: int(reply.Status),
		Header: pb.HeaderFromProto(reply.Header),
		Body:   reply.Body,
	}, nil
}

// HandleConn always fails: a raw net.Conn does not cross the gRPC boundary
// (design §5, see ext.AccessProtocol.HandleConn's doc comment) — a gRPC
// AccessProtocol is HTTP-transport-only, so a TransportConn plugin never runs
// behind this client. Any caller reaching this (a misconfigured TransportConn
// entry pointed at a gRPC executor) gets a clear "native only" error rather
// than a nonsensical wire call.
func (c *AccessProtocolClient) HandleConn(ctx context.Context, conn net.Conn) error {
	return ext.NewError(ext.ErrCodeNotImplement,
		"AccessProtocol: TransportConn is native/in-process only, not available over gRPC")
}
