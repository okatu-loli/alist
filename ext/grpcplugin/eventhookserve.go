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

// EventHookPlugin is the go-plugin Plugin for a Kind=EventHook extension (W7).
// Mirrors ActionPlugin one Kind over: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is
// used.
type EventHookPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.EventHook // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*EventHookPlugin)(nil)

func (p *EventHookPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterEventHookServiceServer(s, &eventHookServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *EventHookPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewEventHookServiceClient(c)
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

// EventHookPluginMap is the go-plugin plugin set served/dispensed under
// pb.EventHookPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func EventHookPluginMap(impl ext.EventHook) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.EventHookPluginName: &EventHookPlugin{Impl: impl},
	}, impl)
}

// HostEventHookPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed EventHook, so the plugin's
// Init gets a working host. serverFactory is host.NewHostServiceServer(theHost)
// — an opaque grpc.Server factory (the SDK cannot import the host's grpc/host
// package).
func HostEventHookPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.EventHookPluginName: &EventHookPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:    &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeEventHook runs p as a go-plugin gRPC plugin. An out-of-tree EventHook
// binary's main is simply:
// func main() { grpcplugin.ServeEventHook(myHook) }.
func ServeEventHook(p ext.EventHook) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         EventHookPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// eventHookServer adapts an ext.EventHook to the generated EventHookServiceServer.
type eventHookServer struct {
	pb.UnimplementedEventHookServiceServer
	impl   ext.EventHook
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the hook via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors actionServer.InitHost (ext/grpcplugin/actionserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a hook must not retain ctx for background work started here.
func (s *eventHookServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *eventHookServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// OnEvent decodes the wire EventProto into ext.Event and forwards it to the
// plugin (ext.EventHook.OnEvent). Its error, if any, crosses back as an
// ErrorReply rather than a transport error — the host logs it and swallows it
// (see ext.EventHook's doc comment: a hook is a notification sink, never a
// veto), mirroring actionServer.Invoke's error handling.
func (s *eventHookServer) OnEvent(ctx context.Context, req *pb.EventProto) (*pb.ErrorReply, error) {
	ev := ext.Event{
		Type:      ext.EventType(req.Type),
		UnixMilli: req.UnixMilli,
		Path:      req.Path,
		DstPath:   req.DstPath,
		Actor:     req.Actor,
	}
	if err := s.impl.OnEvent(ctx, ev); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}
