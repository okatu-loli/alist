package grpcplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// BackgroundServicePlugin is the go-plugin Plugin for a Kind=BackgroundService
// extension: a long-lived host-managed task (FRP tunnel, scheduled sync,
// watcher). Mirrors AuthProviderPlugin: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is
// used.
type BackgroundServicePlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.BackgroundService // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*BackgroundServicePlugin)(nil)

func (p *BackgroundServicePlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterBackgroundServiceServiceServer(s, &backgroundServiceServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *BackgroundServicePlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewBackgroundServiceServiceClient(c)
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

// BackgroundServicePluginMap is the go-plugin plugin set served/dispensed under
// pb.BgServicePluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func BackgroundServicePluginMap(impl ext.BackgroundService) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.BgServicePluginName: &BackgroundServicePlugin{Impl: impl},
	}, impl)
}

// HostBackgroundServicePluginMap is the host-side plugin set that wires a
// reverse HostService (serverFactory) into the dispensed BackgroundService, so
// the plugin's Init gets a working host. serverFactory is
// host.NewHostServiceServer(theHost) — an opaque grpc.Server factory (the SDK
// cannot import the host's grpc/host package).
func HostBackgroundServicePluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.BgServicePluginName: &BackgroundServicePlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:    &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeBackgroundService runs p as a go-plugin gRPC plugin. An out-of-tree
// BackgroundService binary's main is simply:
// func main() { grpcplugin.ServeBackgroundService(myService) }.
func ServeBackgroundService(p ext.BackgroundService) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         BackgroundServicePluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// backgroundServiceServer adapts an ext.BackgroundService to the generated
// BackgroundServiceServiceServer.
//
// The adaptation bridges two shapes: ext.BackgroundService.Run is BLOCKING (it
// returns only when its context is cancelled), whereas the Start RPC must
// return immediately so the host is not held for the service's whole lifetime.
// So Start owns a per-run context: it spawns impl.Run(runCtx) in a goroutine
// and returns at once; Stop cancels runCtx (unblocking Run, which tears the
// service down — e.g. kills the child frpc); Status just forwards to
// impl.Status. running/cancel are mutex-guarded so concurrent Start/Stop/Status
// RPCs are race-safe, and Start is idempotent (a second Start while running is
// a no-op, matching the proto's "idempotent when already running").
type backgroundServiceServer struct {
	pb.UnimplementedBackgroundServiceServiceServer
	impl   ext.BackgroundService
	broker *plugin.GRPCBroker

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the service via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors driverServer.InitHost (ext/grpcplugin/server.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a service must not retain ctx for background work started here.
func (s *backgroundServiceServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *backgroundServiceServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

func (s *backgroundServiceServer) Start(ctx context.Context, _ *pb.Empty) (*pb.ErrorReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return &pb.ErrorReply{}, nil // idempotent: already running
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true
	go func() {
		// Run blocks until runCtx is cancelled (Stop) or it returns early; the
		// host supervisor owns restart policy, so a Run error just ends this
		// session — Status will report it.
		_ = s.impl.Run(runCtx)
	}()
	return &pb.ErrorReply{}, nil
}

func (s *backgroundServiceServer) Stop(ctx context.Context, _ *pb.Empty) (*pb.ErrorReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return &pb.ErrorReply{}, nil // idempotent: already stopped
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running = false
	return &pb.ErrorReply{}, nil
}

func (s *backgroundServiceServer) Status(ctx context.Context, _ *pb.Empty) (*pb.StatusReply, error) {
	st, err := s.impl.Status(ctx)
	if err != nil {
		return &pb.StatusReply{Error: pb.ErrorToProto(err)}, nil
	}
	var metrics []byte
	if len(st.Metrics) > 0 {
		metrics, _ = json.Marshal(st.Metrics) // best-effort: a marshal failure yields empty metrics, not a lost status
	}
	return &pb.StatusReply{
		State:       st.State,
		Detail:      st.Detail,
		MetricsJson: metrics,
	}, nil
}
