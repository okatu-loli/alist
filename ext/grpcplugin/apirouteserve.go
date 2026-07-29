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

// APIRoutePlugin is the go-plugin Plugin for a Kind=APIRoute extension (W8a).
// Mirrors AuthProviderPlugin one Kind over: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is
// used.
type APIRoutePlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.APIRoute // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*APIRoutePlugin)(nil)

func (p *APIRoutePlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAPIRouteServiceServer(s, &apiRouteServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *APIRoutePlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewAPIRouteServiceClient(c)
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

// APIRoutePluginMap is the go-plugin plugin set served/dispensed under
// pb.APIRoutePluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func APIRoutePluginMap(impl ext.APIRoute) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.APIRoutePluginName: &APIRoutePlugin{Impl: impl},
	}, impl)
}

// HostAPIRoutePluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed APIRoute, so the plugin's
// Init gets a working host. serverFactory is host.NewHostServiceServer(theHost)
// — an opaque grpc.Server factory (the SDK cannot import the host's grpc/host
// package).
func HostAPIRoutePluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.APIRoutePluginName: &APIRoutePlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:   &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeAPIRoute runs p as a go-plugin gRPC plugin. An out-of-tree APIRoute
// binary's main is simply:
// func main() { grpcplugin.ServeAPIRoute(myRoute) }.
func ServeAPIRoute(p ext.APIRoute) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         APIRoutePluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// apiRouteServer adapts an ext.APIRoute to the generated APIRouteServiceServer.
type apiRouteServer struct {
	pb.UnimplementedAPIRouteServiceServer
	impl   ext.APIRoute
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the route via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors authProviderServer.InitHost (ext/grpcplugin/authserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a route must not retain ctx for background work started here.
func (s *apiRouteServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *apiRouteServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

func (s *apiRouteServer) Routes(ctx context.Context, _ *pb.Empty) (*pb.RoutesReply, error) {
	decls := s.impl.Routes()
	out := make([]*pb.RouteDecl, len(decls))
	for i, d := range decls {
		out[i] = &pb.RouteDecl{Prefix: d.Prefix, Public: d.Public}
	}
	return &pb.RoutesReply{Routes: out}, nil
}

func (s *apiRouteServer) Handle(ctx context.Context, req *pb.HTTPDoRequest) (*pb.HTTPDoReply, error) {
	resp, err := s.impl.Handle(ctx, &ext.HTTPRequest{
		Method: req.Method,
		URL:    req.Url,
		Header: pb.HeaderFromProto(req.Header),
		Body:   req.Body,
	})
	if err != nil {
		return &pb.HTTPDoReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.HTTPDoReply{
		Status: int32(resp.Status),
		Header: pb.HeaderToProto(resp.Header),
		Body:   resp.Body,
	}, nil
}
