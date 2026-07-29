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

// AuthClient is a host-side proxy over a gRPC AuthProvider plugin (WS-8). It
// satisfies ext.AuthProvider by forwarding every call over gRPC, so the wizard
// engine can treat a subprocess AuthProvider exactly like an in-process one —
// mirrors Client (the StorageDriver proxy) one Kind over.
type AuthClient struct {
	pc  *plugin.Client
	svc pb.AuthProviderServiceClient

	manifest ext.Manifest
}

var _ ext.AuthProvider = (*AuthClient)(nil)

// LaunchAuthProvider starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the AuthProviderService client, and reads the
// manifest. The returned client owns the subprocess; call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors Launch (host/client.go).
func LaunchAuthProvider(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*AuthClient, error) {
	plugins := grpcplugin.AuthProviderPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostAuthProviderPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: auth plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.AuthPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.AuthPluginName, err)
	}
	svc, ok := raw.(pb.AuthProviderServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an AuthProviderServiceClient", raw)
	}
	c := &AuthClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *AuthClient) loadManifest(ctx context.Context) error {
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
func (c *AuthClient) Exited() bool { return c.pc.Exited() }

// --- Extension (process-level) ---

func (c *AuthClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *AuthClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *AuthClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.AuthProvider ---

func (c *AuthClient) AuthURL(ctx context.Context, req ext.AuthURLRequest) (string, error) {
	reply, err := c.svc.AuthURL(ctx, &pb.AuthURLRequest{
		State: req.State, RedirectUri: req.RedirectURI, Scopes: req.Scopes,
	})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return "", e
	}
	return reply.Url, nil
}

func (c *AuthClient) Exchange(ctx context.Context, req ext.ExchangeRequest) (ext.Token, error) {
	reply, err := c.svc.Exchange(ctx, &pb.ExchangeRequest{Code: req.Code, RedirectUri: req.RedirectURI})
	return tokenFromReply(reply, err)
}

func (c *AuthClient) Refresh(ctx context.Context, refreshToken string) (ext.Token, error) {
	reply, err := c.svc.Refresh(ctx, &pb.RefreshRequest{RefreshToken: refreshToken})
	return tokenFromReply(reply, err)
}

func tokenFromReply(reply *pb.TokenReply, err error) (ext.Token, error) {
	if err != nil {
		return ext.Token{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.Token{}, e
	}
	return pb.TokenFromProto(reply.Token), nil
}
