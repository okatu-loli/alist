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

// AuthNMethodPlugin is the go-plugin Plugin for a Kind=AuthNMethod extension
// (design §2.2). Mirrors AuthProviderPlugin: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is used.
type AuthNMethodPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.AuthNMethod // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*AuthNMethodPlugin)(nil)

func (p *AuthNMethodPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAuthNMethodServiceServer(s, &authNMethodServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *AuthNMethodPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewAuthNMethodServiceClient(c)
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

// AuthNMethodPluginMap is the go-plugin plugin set served/dispensed under
// pb.AuthNPluginName. impl also gets auto-mounted under pb.AssistPluginName
// if it implements ext.Assist (mountAssist, assistserve.go).
func AuthNMethodPluginMap(impl ext.AuthNMethod) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.AuthNPluginName: &AuthNMethodPlugin{Impl: impl},
	}, impl)
}

// HostAuthNMethodPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed AuthNMethod, so the plugin's
// Init gets a working host. serverFactory is host.NewHostServiceServer(theHost)
// — an opaque grpc.Server factory (the SDK cannot import the host's grpc/host
// package).
func HostAuthNMethodPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.AuthNPluginName:  &AuthNMethodPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName: &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeAuthNMethod runs m as a go-plugin gRPC plugin. An out-of-tree
// AuthNMethod binary's main is simply:
// func main() { grpcplugin.ServeAuthNMethod(myMethod) }.
func ServeAuthNMethod(m ext.AuthNMethod) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         AuthNMethodPluginMap(m),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// authNMethodServer adapts an ext.AuthNMethod to the generated
// AuthNMethodServiceServer.
type authNMethodServer struct {
	pb.UnimplementedAuthNMethodServiceServer
	impl   ext.AuthNMethod
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the method via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors driverServer.InitHost (ext/grpcplugin/server.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a method must not retain ctx for background work started here.
func (s *authNMethodServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *authNMethodServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

func (s *authNMethodServer) MethodInfo(ctx context.Context, _ *pb.Empty) (*pb.MethodInfoReply, error) {
	return pb.MethodInfoToProto(s.impl.MethodInfo()), nil
}

func (s *authNMethodServer) Begin(ctx context.Context, req *pb.BeginRequest) (*pb.BeginReply, error) {
	principal, r := pb.AuthNRequestFromProto(req)
	chal, err := s.impl.Begin(ctx, principal, r)
	if err != nil {
		return &pb.BeginReply{Error: pb.ErrorToProto(err)}, nil
	}
	return pb.AuthNChallengeToProto(chal), nil
}

func (s *authNMethodServer) Finish(ctx context.Context, req *pb.FinishRequest) (*pb.FinishReply, error) {
	res, err := s.impl.Finish(ctx, pb.AuthNResponseFromProto(req))
	if err != nil {
		return &pb.FinishReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.FinishReply{Result: pb.AuthNResultToProto(res)}, nil
}
