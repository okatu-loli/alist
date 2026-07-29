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

// ScheduledJobPlugin is the go-plugin Plugin for a Kind=ScheduledJob
// extension (W7). Mirrors EventHookPlugin one Kind over: the plugin side
// sets Impl and only GRPCServer is used; the host side leaves Impl nil and
// only GRPCClient is used.
type ScheduledJobPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.ScheduledJob // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*ScheduledJobPlugin)(nil)

func (p *ScheduledJobPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterScheduledJobServiceServer(s, &scheduledJobServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *ScheduledJobPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewScheduledJobServiceClient(c)
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

// ScheduledJobPluginMap is the go-plugin plugin set served/dispensed under
// pb.ScheduledJobPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func ScheduledJobPluginMap(impl ext.ScheduledJob) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.ScheduledJobPluginName: &ScheduledJobPlugin{Impl: impl},
	}, impl)
}

// HostScheduledJobPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed ScheduledJob, so the
// plugin's Init gets a working host. serverFactory is
// host.NewHostServiceServer(theHost) — an opaque grpc.Server factory (the SDK
// cannot import the host's grpc/host package).
func HostScheduledJobPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.ScheduledJobPluginName: &ScheduledJobPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:       &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeScheduledJob runs p as a go-plugin gRPC plugin. An out-of-tree
// ScheduledJob binary's main is simply:
// func main() { grpcplugin.ServeScheduledJob(myJob) }.
func ServeScheduledJob(p ext.ScheduledJob) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         ScheduledJobPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// scheduledJobServer adapts an ext.ScheduledJob to the generated
// ScheduledJobServiceServer.
type scheduledJobServer struct {
	pb.UnimplementedScheduledJobServiceServer
	impl   ext.ScheduledJob
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the job via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors eventHookServer.InitHost (ext/grpcplugin/eventhookserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a job must not retain ctx for background work started here.
func (s *scheduledJobServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *scheduledJobServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// Schedule forwards ext.ScheduledJob.Schedule's cadence spec. Unlike
// APIRoute/Action's methods, neither Schedule nor Run take a
// plugin-supplied argument — the host calls Schedule once (at scheduler
// start) and Run once per tick, both with Empty.
func (s *scheduledJobServer) Schedule(ctx context.Context, _ *pb.Empty) (*pb.ScheduleReply, error) {
	return &pb.ScheduleReply{Spec: s.impl.Schedule()}, nil
}

// Run forwards ext.ScheduledJob.Run. Its error, if any, crosses back as an
// ErrorReply rather than a transport error — the host logs it and continues
// (see ext.ScheduledJob's doc comment: a Run error does not stop future
// ticks), mirroring eventHookServer.OnEvent's error handling.
func (s *scheduledJobServer) Run(ctx context.Context, _ *pb.Empty) (*pb.ErrorReply, error) {
	if err := s.impl.Run(ctx); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}
