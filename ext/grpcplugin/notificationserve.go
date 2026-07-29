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

// NotificationPlugin is the go-plugin Plugin for a Kind=NotificationChannel
// extension (W7b). Mirrors EventHookPlugin one Kind over: the plugin side
// sets Impl and only GRPCServer is used; the host side leaves Impl nil and
// only GRPCClient is used.
type NotificationPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.Notification // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*NotificationPlugin)(nil)

func (p *NotificationPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterNotificationServiceServer(s, &notificationServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *NotificationPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewNotificationServiceClient(c)
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

// NotificationPluginMap is the go-plugin plugin set served/dispensed under
// pb.NotificationPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func NotificationPluginMap(impl ext.Notification) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.NotificationPluginName: &NotificationPlugin{Impl: impl},
	}, impl)
}

// HostNotificationPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed Notification, so the
// plugin's Init gets a working host. serverFactory is
// host.NewHostServiceServer(theHost) — an opaque grpc.Server factory (the SDK
// cannot import the host's grpc/host package).
func HostNotificationPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.NotificationPluginName: &NotificationPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:       &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeNotification runs p as a go-plugin gRPC plugin. An out-of-tree
// Notification binary's main is simply:
// func main() { grpcplugin.ServeNotification(myChannel) }.
func ServeNotification(p ext.Notification) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         NotificationPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// notificationServer adapts an ext.Notification to the generated
// NotificationServiceServer.
type notificationServer struct {
	pb.UnimplementedNotificationServiceServer
	impl   ext.Notification
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the channel via Init — this is
// the only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors eventHookServer.InitHost (ext/grpcplugin/eventhookserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a channel must not retain ctx for background work started here.
func (s *notificationServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *notificationServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// Notify decodes the wire NoticeProto into ext.Notice and forwards it to the
// plugin (ext.Notification.Notify). Its error, if any, crosses back as an
// ErrorReply rather than a transport error — the host logs it and swallows it
// (see ext.Notification's doc comment: a channel is a notification sink,
// never a veto), mirroring eventHookServer.OnEvent's error handling.
func (s *notificationServer) Notify(ctx context.Context, req *pb.NoticeProto) (*pb.ErrorReply, error) {
	n := ext.Notice{
		Level:     ext.NoticeLevel(req.Level),
		Title:     req.Title,
		Body:      req.Body,
		Source:    req.Source,
		UnixMilli: req.UnixMilli,
	}
	if err := s.impl.Notify(ctx, n); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}
