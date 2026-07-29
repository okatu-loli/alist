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

// OfflineDownloadToolPlugin is the go-plugin Plugin for a
// Kind=OfflineDownloadTool extension (Phase E ring 1): an external download
// daemon backend (aria2/qBittorrent/Transmission-alike). Mirrors
// NotificationPlugin one Kind over: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is
// used.
type OfflineDownloadToolPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.OfflineDownloadTool // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*OfflineDownloadToolPlugin)(nil)

func (p *OfflineDownloadToolPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterOfflineDownloadToolServiceServer(s, &offlineDownloadToolServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *OfflineDownloadToolPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewOfflineDownloadToolServiceClient(c)
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

// OfflineDownloadToolPluginMap is the go-plugin plugin set served/dispensed
// under pb.OfflineDownloadToolPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func OfflineDownloadToolPluginMap(impl ext.OfflineDownloadTool) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.OfflineDownloadToolPluginName: &OfflineDownloadToolPlugin{Impl: impl},
	}, impl)
}

// HostOfflineDownloadToolPluginMap is the host-side plugin set that wires a
// reverse HostService (serverFactory) into the dispensed OfflineDownloadTool,
// so the plugin's Init gets a working host. serverFactory is
// host.NewHostServiceServer(theHost) — an opaque grpc.Server factory (the SDK
// cannot import the host's grpc/host package).
func HostOfflineDownloadToolPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.OfflineDownloadToolPluginName: &OfflineDownloadToolPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:              &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeOfflineDownloadTool runs p as a go-plugin gRPC plugin. An out-of-tree
// OfflineDownloadTool binary's main is simply:
// func main() { grpcplugin.ServeOfflineDownloadTool(myTool) }.
func ServeOfflineDownloadTool(p ext.OfflineDownloadTool) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         OfflineDownloadToolPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// offlineDownloadToolServer adapts an ext.OfflineDownloadTool to the
// generated OfflineDownloadToolServiceServer.
type offlineDownloadToolServer struct {
	pb.UnimplementedOfflineDownloadToolServiceServer
	impl   ext.OfflineDownloadTool
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the tool via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors notificationServer.InitHost (ext/grpcplugin/notificationserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a tool must not retain ctx for background work started here.
func (s *offlineDownloadToolServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *offlineDownloadToolServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// Config forwards ext.OfflineDownloadTool.Config, the tool's self-reported
// identity (the Name the offline_download_tool setting / a per-task Toolname
// selects on).
func (s *offlineDownloadToolServer) Config(ctx context.Context, _ *pb.Empty) (*pb.ToolConfigReply, error) {
	cfg := s.impl.Config()
	return &pb.ToolConfigReply{Name: cfg.Name}, nil
}

// AddURL decodes AddArgsProto into ext.AddArgs and forwards it to the plugin
// (ext.OfflineDownloadTool.AddURL). Its error, if any, crosses back as an
// ErrorReply field on AddURLReply rather than a transport error, mirroring
// every other business-method leg in this SDK.
func (s *offlineDownloadToolServer) AddURL(ctx context.Context, req *pb.AddArgsProto) (*pb.AddURLReply, error) {
	gid, err := s.impl.AddURL(ctx, ext.AddArgs{
		URL:     req.Url,
		TempDir: req.TempDir,
		UID:     req.Uid,
	})
	if err != nil {
		return &pb.AddURLReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.AddURLReply{Gid: gid}, nil
}

// Remove forwards ext.OfflineDownloadTool.Remove.
func (s *offlineDownloadToolServer) Remove(ctx context.Context, req *pb.ToolRemoveRequest) (*pb.ErrorReply, error) {
	if err := s.impl.Remove(ctx, req.Gid); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}

// Status forwards ext.OfflineDownloadTool.Status, encoding the resulting
// ext.ToolStatus as a ToolStatusProto.
func (s *offlineDownloadToolServer) Status(ctx context.Context, req *pb.ToolStatusRequest) (*pb.ToolStatusReply, error) {
	st, err := s.impl.Status(ctx, req.Gid)
	if err != nil {
		return &pb.ToolStatusReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ToolStatusReply{Status: &pb.ToolStatusProto{
		TotalBytes: st.TotalBytes,
		Progress:   st.Progress,
		NewGid:     st.NewGID,
		Completed:  st.Completed,
		Status:     st.Status,
		Err:        st.Err,
	}}, nil
}
