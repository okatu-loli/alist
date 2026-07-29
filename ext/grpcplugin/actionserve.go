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

// ActionPlugin is the go-plugin Plugin for a Kind=Action extension (W8b).
// Mirrors APIRoutePlugin one Kind over: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is
// used.
type ActionPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.Action // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*ActionPlugin)(nil)

func (p *ActionPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterActionServiceServer(s, &actionServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *ActionPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewActionServiceClient(c)
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

// ActionPluginMap is the go-plugin plugin set served/dispensed under
// pb.ActionPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func ActionPluginMap(impl ext.Action) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.ActionPluginName: &ActionPlugin{Impl: impl},
	}, impl)
}

// HostActionPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed Action, so the plugin's
// Init gets a working host. serverFactory is host.NewHostServiceServer(theHost)
// — an opaque grpc.Server factory (the SDK cannot import the host's grpc/host
// package).
func HostActionPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.ActionPluginName: &ActionPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName: &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeAction runs p as a go-plugin gRPC plugin. An out-of-tree Action
// binary's main is simply:
// func main() { grpcplugin.ServeAction(myAction) }.
func ServeAction(p ext.Action) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         ActionPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// actionServer adapts an ext.Action to the generated ActionServiceServer.
type actionServer struct {
	pb.UnimplementedActionServiceServer
	impl   ext.Action
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the action via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors apiRouteServer.InitHost (ext/grpcplugin/apirouteserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so an action must not retain ctx for background work started here.
func (s *actionServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *actionServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// Invoke runs the named action, decoding args_json into the map[string]any
// ext.Action.Invoke expects and encoding whatever it returns back to
// result_json — neither shape is known ahead of time, so JSON is the wire
// format rather than a typed message (mirrors the manifest encoding above).
func (s *actionServer) Invoke(ctx context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	var args map[string]any
	if len(req.ArgsJson) > 0 {
		if err := json.Unmarshal(req.ArgsJson, &args); err != nil {
			return &pb.InvokeReply{Error: pb.ErrorToProto(fmt.Errorf("grpcplugin: decode args_json: %w", err))}, nil
		}
	}
	result, err := s.impl.Invoke(ctx, ext.Principal(req.Principal), req.ActionId, args)
	if err != nil {
		return &pb.InvokeReply{Error: pb.ErrorToProto(err)}, nil
	}
	rj, err := json.Marshal(result)
	if err != nil {
		return &pb.InvokeReply{Error: pb.ErrorToProto(fmt.Errorf("grpcplugin: encode result: %w", err))}, nil
	}
	return &pb.InvokeReply{ResultJson: rj}, nil
}
