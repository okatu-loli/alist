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

// FileDecoratorPlugin is the go-plugin Plugin for a Kind=FileDecorator
// extension (W9a). Mirrors EventHookPlugin one Kind over: the plugin side sets
// Impl and only GRPCServer is used; the host side leaves Impl nil and only
// GRPCClient is used.
type FileDecoratorPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.FileDecorator // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*FileDecoratorPlugin)(nil)

func (p *FileDecoratorPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterFileDecoratorServiceServer(s, &fileDecoratorServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *FileDecoratorPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewFileDecoratorServiceClient(c)
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

// FileDecoratorPluginMap is the go-plugin plugin set served/dispensed under
// pb.FileDecoratorPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func FileDecoratorPluginMap(impl ext.FileDecorator) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.FileDecoratorPluginName: &FileDecoratorPlugin{Impl: impl},
	}, impl)
}

// HostFileDecoratorPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed FileDecorator, so the
// plugin's Init gets a working host. serverFactory is
// host.NewHostServiceServer(theHost) — an opaque grpc.Server factory (the SDK
// cannot import the host's grpc/host package).
func HostFileDecoratorPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.FileDecoratorPluginName: &FileDecoratorPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:        &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeFileDecorator runs p as a go-plugin gRPC plugin. An out-of-tree
// FileDecorator binary's main is simply:
// func main() { grpcplugin.ServeFileDecorator(myDecorator) }.
func ServeFileDecorator(p ext.FileDecorator) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         FileDecoratorPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// fileDecoratorServer adapts an ext.FileDecorator to the generated
// FileDecoratorServiceServer.
type fileDecoratorServer struct {
	pb.UnimplementedFileDecoratorServiceServer
	impl   ext.FileDecorator
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the decorator via Init — this is
// the only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors eventHookServer.InitHost (ext/grpcplugin/eventhookserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a decorator must not retain ctx for background work started
// here.
func (s *fileDecoratorServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *fileDecoratorServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// Decorate decodes the wire ObjRef batch into []ext.ObjRef, forwards to the
// plugin (ext.FileDecorator.Decorate), and encodes the returned
// map[string]any as values_json — neither the ref batch's meaning nor the
// result's value shape is known ahead of time on this side, so JSON carries
// the result (mirrors actionServer.Invoke's args_json/result_json).
func (s *fileDecoratorServer) Decorate(ctx context.Context, req *pb.DecorateRequest) (*pb.DecorateReply, error) {
	refs := make([]ext.ObjRef, len(req.Refs))
	for i, r := range req.Refs {
		refs[i] = pb.RefFromProto(r)
	}
	values, err := s.impl.Decorate(ctx, req.Group, refs)
	if err != nil {
		return &pb.DecorateReply{Error: pb.ErrorToProto(err)}, nil
	}
	vj, err := json.Marshal(values)
	if err != nil {
		return &pb.DecorateReply{Error: pb.ErrorToProto(fmt.Errorf("grpcplugin: encode values: %w", err))}, nil
	}
	return &pb.DecorateReply{ValuesJson: vj}, nil
}
