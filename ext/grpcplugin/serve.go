package grpcplugin

import (
	"context"
	"fmt"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// Handshake is the shared go-plugin handshake config. Its values come from
// grpcproto (the frozen small decisions, 9.2). Both Serve (plugin side) and the
// host launcher read it.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  pb.ProtocolVersion,
	MagicCookieKey:   pb.MagicCookieKey,
	MagicCookieValue: pb.MagicCookieValue,
}

// StorageDriverPlugin is the go-plugin Plugin for a Kind=StorageDriver
// extension. The plugin side sets Impl and only GRPCServer is used; the host
// side leaves Impl nil and only GRPCClient is used. It is exported so the host
// launcher (which imports this module) reuses the exact same wiring.
type StorageDriverPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.StorageDriver // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities. nil on the
	// plugin side and for launches with no reverse host (unchanged behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*StorageDriverPlugin)(nil)

func (p *StorageDriverPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterDriverServiceServer(s, &driverServer{drv: p.Impl, broker: broker})
	return nil
}

func (p *StorageDriverPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewDriverServiceClient(c)
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

// PluginMap is the go-plugin plugin set served/dispensed under PluginName.
// impl also gets auto-mounted under pb.AssistPluginName if it implements
// ext.Assist (mountAssist, assistserve.go).
func PluginMap(impl ext.StorageDriver) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.PluginName: &StorageDriverPlugin{Impl: impl},
	}, impl)
}

// HostPluginMap is the host-side plugin set that wires a reverse HostService
// (serverFactory) into the dispensed StorageDriver, so the plugin's Init gets a
// working host. serverFactory is host.NewHostServiceServer(theHost) — an opaque
// grpc.Server factory (the SDK cannot import the host's grpc/host package).
func HostPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.PluginName:       &StorageDriverPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName: &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// Serve runs drv as a go-plugin gRPC plugin. An out-of-tree driver binary's main
// is simply: func main() { grpcplugin.Serve(myDriver) }.
func Serve(drv ext.StorageDriver) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         PluginMap(drv),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}
