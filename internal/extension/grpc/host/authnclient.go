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

// AuthNClient is a host-side proxy over a gRPC AuthNMethod plugin (design §2.2).
// It satisfies ext.AuthNMethod by forwarding every call over gRPC, so the login
// orchestrator can treat a subprocess method exactly like an in-process one —
// mirrors AuthClient (the AuthProvider proxy) one Kind over.
type AuthNClient struct {
	pc  *plugin.Client
	svc pb.AuthNMethodServiceClient

	manifest ext.Manifest
}

var _ ext.AuthNMethod = (*AuthNClient)(nil)

// LaunchAuthNMethod starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the AuthNMethodService client, and reads the
// manifest. The returned client owns the subprocess; call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors Launch (host/client.go).
func LaunchAuthNMethod(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*AuthNClient, error) {
	plugins := grpcplugin.AuthNMethodPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostAuthNMethodPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: authn plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.AuthNPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.AuthNPluginName, err)
	}
	svc, ok := raw.(pb.AuthNMethodServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an AuthNMethodServiceClient", raw)
	}
	c := &AuthNClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *AuthNClient) loadManifest(ctx context.Context) error {
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
func (c *AuthNClient) Exited() bool { return c.pc.Exited() }

// --- Extension (process-level) ---

func (c *AuthNClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *AuthNClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *AuthNClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.AuthNMethod ---

func (c *AuthNClient) MethodInfo() ext.AuthMethodInfo {
	reply, err := c.svc.MethodInfo(context.Background(), &pb.Empty{})
	if err != nil {
		return ext.AuthMethodInfo{}
	}
	return pb.MethodInfoFromProto(reply)
}

func (c *AuthNClient) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	reply, err := c.svc.Begin(ctx, pb.AuthNRequestToProto(principal, req))
	if err != nil {
		return ext.AuthNChallenge{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.AuthNChallenge{}, e
	}
	return pb.AuthNChallengeFromProto(reply), nil
}

func (c *AuthNClient) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	reply, err := c.svc.Finish(ctx, pb.AuthNResponseToProto(req))
	if err != nil {
		return ext.AuthNResult{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.AuthNResult{}, e
	}
	return pb.AuthNResultFromProto(reply.Result), nil
}
