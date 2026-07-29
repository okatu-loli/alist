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

// AccessProtocolPlugin is the go-plugin Plugin for a Kind=AccessProtocol
// extension's TransportHTTP half (W9a). Mirrors APIRoutePlugin one Kind over:
// the plugin side sets Impl and only GRPCServer is used; the host side leaves
// Impl nil and only GRPCClient is used.
//
// Only TransportHTTP crosses the gRPC boundary (design §5): HandleConn's raw
// net.Conn is not serializable, so a TransportConn (FTP/SFTP) protocol is
// native/in-process only and never wraps this Plugin.
type AccessProtocolPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.AccessProtocol // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*AccessProtocolPlugin)(nil)

func (p *AccessProtocolPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAccessProtocolServiceServer(s, &accessProtocolServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *AccessProtocolPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewAccessProtocolServiceClient(c)
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

// AccessProtocolPluginMap is the go-plugin plugin set served/dispensed under
// pb.AccessProtocolPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func AccessProtocolPluginMap(impl ext.AccessProtocol) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.AccessProtocolPluginName: &AccessProtocolPlugin{Impl: impl},
	}, impl)
}

// HostAccessProtocolPluginMap is the host-side plugin set that wires a
// reverse HostService (serverFactory) into the dispensed AccessProtocol, so
// the plugin's Init gets a working host. serverFactory is
// host.NewHostServiceServer(theHost) — an opaque grpc.Server factory (the SDK
// cannot import the host's grpc/host package).
func HostAccessProtocolPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.AccessProtocolPluginName: &AccessProtocolPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:         &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeAccessProtocol runs p as a go-plugin gRPC plugin. An out-of-tree
// TransportHTTP AccessProtocol binary's main is simply:
// func main() { grpcplugin.ServeAccessProtocol(myProtocol) }.
func ServeAccessProtocol(p ext.AccessProtocol) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         AccessProtocolPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// accessProtocolServer adapts an ext.AccessProtocol to the generated
// AccessProtocolServiceServer.
type accessProtocolServer struct {
	pb.UnimplementedAccessProtocolServiceServer
	impl   ext.AccessProtocol
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the protocol via Init — this is
// the only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors apiRouteServer.InitHost (ext/grpcplugin/apirouteserve.go).
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a protocol must not retain ctx for background work started
// here.
func (s *accessProtocolServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *accessProtocolServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// ProtocolInfo encodes ext.ProtocolInfo field-for-field.
func (s *accessProtocolServer) ProtocolInfo(ctx context.Context, _ *pb.Empty) (*pb.ProtocolInfoReply, error) {
	info := s.impl.ProtocolInfo()
	return &pb.ProtocolInfoReply{
		Scheme:       info.Scheme,
		Transport:    int32(info.Transport),
		DefaultPort:  int32(info.DefaultPort),
		MountPattern: info.MountPattern,
	}, nil
}

func (s *accessProtocolServer) ServeHTTP(ctx context.Context, req *pb.HTTPDoRequest) (*pb.HTTPDoReply, error) {
	resp, err := s.impl.ServeHTTP(ctx, &ext.HTTPRequest{
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
