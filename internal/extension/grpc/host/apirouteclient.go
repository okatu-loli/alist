package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// APIRouteClient is a host-side proxy over a gRPC APIRoute plugin (W8a). It
// satisfies ext.APIRoute by forwarding every call over gRPC — mirrors
// AuthClient (the AuthProvider proxy) one Kind over.
type APIRouteClient struct {
	pc  *plugin.Client
	svc pb.APIRouteServiceClient

	manifest ext.Manifest
}

var _ ext.APIRoute = (*APIRouteClient)(nil)

// LaunchAPIRoute starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the APIRouteService client, and reads the
// manifest. The returned client owns the subprocess; call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchAuthProvider (host/authclient.go).
func LaunchAPIRoute(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*APIRouteClient, error) {
	plugins := grpcplugin.APIRoutePluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostAPIRoutePluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: apiroute plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.APIRoutePluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.APIRoutePluginName, err)
	}
	svc, ok := raw.(pb.APIRouteServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an APIRouteServiceClient", raw)
	}
	c := &APIRouteClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *APIRouteClient) loadManifest(ctx context.Context) error {
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
func (c *APIRouteClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *APIRouteClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *APIRouteClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *APIRouteClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *APIRouteClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.APIRoute ---

func (c *APIRouteClient) Routes() []ext.RouteDecl {
	rr, err := c.svc.Routes(context.Background(), &pb.Empty{})
	if err != nil {
		return nil
	}
	out := make([]ext.RouteDecl, len(rr.Routes))
	for i, d := range rr.Routes {
		out[i] = ext.RouteDecl{Prefix: d.Prefix, Public: d.Public}
	}
	return out
}

func (c *APIRouteClient) Handle(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	reply, err := c.svc.Handle(ctx, &pb.HTTPDoRequest{
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
