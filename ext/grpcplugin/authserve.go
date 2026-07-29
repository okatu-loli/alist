package grpcplugin

import (
	"context"
	"encoding/json"
	"fmt"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// AuthProviderPlugin is the go-plugin Plugin for a Kind=AuthProvider extension
// (WS-8). Mirrors StorageDriverPlugin: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is
// used.
type AuthProviderPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.AuthProvider // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*AuthProviderPlugin)(nil)

func (p *AuthProviderPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAuthProviderServiceServer(s, &authProviderServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *AuthProviderPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewAuthProviderServiceClient(c)
	if p.HostServerFactory != nil {
		id := broker.NextId()
		go broker.AcceptAndServe(id, p.HostServerFactory)
		reply, err := client.InitHost(ctx, &pb.InitHostRequest{BrokerId: id})
		if err != nil {
			return nil, fmt.Errorf("grpcplugin: InitHost transport failed: %w", err)
		}
		if e := pb.ErrorFromProto(reply.Error); e != nil {
			return nil, fmt.Errorf("grpcplugin: plugin host init failed: %w", e)
		}
	}
	return client, nil
}

// AuthProviderPluginMap is the go-plugin plugin set served/dispensed under
// pb.AuthPluginName. impl also gets auto-mounted under pb.AssistPluginName
// if it implements ext.Assist (mountAssist, assistserve.go).
func AuthProviderPluginMap(impl ext.AuthProvider) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.AuthPluginName: &AuthProviderPlugin{Impl: impl},
	}, impl)
}

// HostAuthProviderPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed AuthProvider, so the plugin's
// Init gets a working host. serverFactory is host.NewHostServiceServer(theHost)
// — an opaque grpc.Server factory (the SDK cannot import the host's grpc/host
// package).
func HostAuthProviderPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.AuthPluginName:   &AuthProviderPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName: &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeAuthProvider runs p as a go-plugin gRPC plugin. An out-of-tree
// AuthProvider binary's main is simply:
// func main() { grpcplugin.ServeAuthProvider(myProvider) }.
func ServeAuthProvider(p ext.AuthProvider) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         AuthProviderPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// authProviderServer adapts an ext.AuthProvider to the generated
// AuthProviderServiceServer.
type authProviderServer struct {
	pb.UnimplementedAuthProviderServiceServer
	impl   ext.AuthProvider
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the provider via Init — this is
// the only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors driverServer.InitHost (ext/grpcplugin/server.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a provider must not retain ctx for background work started here.
func (s *authProviderServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *authProviderServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

func (s *authProviderServer) AuthURL(ctx context.Context, req *pb.AuthURLRequest) (*pb.AuthURLReply, error) {
	url, err := s.impl.AuthURL(ctx, ext.AuthURLRequest{
		State:       req.State,
		RedirectURI: req.RedirectUri,
		Scopes:      req.Scopes,
	})
	if err != nil {
		return &pb.AuthURLReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.AuthURLReply{Url: url}, nil
}

func (s *authProviderServer) Exchange(ctx context.Context, req *pb.ExchangeRequest) (*pb.TokenReply, error) {
	tok, err := s.impl.Exchange(ctx, ext.ExchangeRequest{Code: req.Code, RedirectURI: req.RedirectUri})
	if err != nil {
		return &pb.TokenReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.TokenReply{Token: pb.TokenToProto(tok)}, nil
}

func (s *authProviderServer) Refresh(ctx context.Context, req *pb.RefreshRequest) (*pb.TokenReply, error) {
	tok, err := s.impl.Refresh(ctx, req.RefreshToken)
	if err != nil {
		return &pb.TokenReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.TokenReply{Token: pb.TokenToProto(tok)}, nil
}
