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

// AssistPlugin is the go-plugin Plugin for the assist auxiliary service: any
// Kind's plugin whose impl also implements ext.Assist rides this dispense
// name alongside its primary Kind's dispense name (mountAssist below).
// Mirrors ActionPlugin: the plugin side sets Impl and only GRPCServer is
// used; the host side leaves Impl nil and only GRPCClient is used.
//
// Unlike a plain in-process auxiliary, Task 2's host-side pluginproc may
// launch a DEDICATED subprocess that only ever dispenses "assist" (an assist
// form submitted for an extension whose primary Kind never got a subprocess
// launched this session) — in that process the impl has never been Init'd by
// anyone, so InitHost here must do the full reverse-host handshake itself,
// exactly like every other Kind, rather than assume some other dispense
// already delivered a Host (see assistServer.InitHost).
type AssistPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.Assist // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the impl can borrow host capabilities (F0/SP6)
	// from within Assist — e.g. host.HTTP()/host.Store() for a "test
	// connection"/"read credential" assist form. nil on the plugin side and
	// for launches with no reverse host (unchanged behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*AssistPlugin)(nil)

func (p *AssistPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAssistServiceServer(s, &assistServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *AssistPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewAssistServiceClient(c)
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

// AssistPluginMap is the go-plugin plugin set served/dispensed under
// pb.AssistPluginName, for a standalone ext.Assist impl.
func AssistPluginMap(impl ext.Assist) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.AssistPluginName: &AssistPlugin{Impl: impl},
	}
}

// HostAssistPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed assist client, so a
// dedicated assist-only subprocess (Task 2 pluginproc) gets its impl fully
// Init'd — the same serverFactory used for every other Host*PluginMap
// (host.NewHostServiceServer(theHost), an opaque grpc.Server factory the SDK
// cannot import).
func HostAssistPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.AssistPluginName: &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// mountAssist adds the assist dispense name to m when impl also implements
// ext.Assist, so any Serve* entry point auto-exposes it for the same
// underlying value it already serves under its primary Kind's dispense name.
// A plugin that does not implement ext.Assist is unaffected (old-plugin
// compatibility: behavior is unchanged for impls that never opted in).
func mountAssist(m map[string]plugin.Plugin, impl any) map[string]plugin.Plugin {
	if a, ok := impl.(ext.Assist); ok {
		m[pb.AssistPluginName] = &AssistPlugin{Impl: a}
	}
	return m
}

// assistServer adapts an ext.Assist to the generated AssistServiceServer.
type assistServer struct {
	pb.UnimplementedAssistServiceServer
	impl   ext.Assist
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. A dedicated assist-only
// subprocess (Task 2's LaunchAssist) never dispenses the impl's primary
// Kind, so nothing else ever Init's it — this handler does the full reverse-
// host handshake itself, exactly like actionServer.InitHost (and every other
// Kind), so an assist form can borrow host capabilities (host.HTTP() to test
// a connection, host.Store() to read a saved credential, ...).
//
// ext.Assist does not embed Extension (it always arrives alongside whichever
// Kind an impl primarily implements, so it cannot require one), so the
// impl.Init call itself is conditional: if impl also implements
// ext.Extension (the common case — impl is the same value as the plugin's
// primary Kind), Init runs and receives the live host; if not, this is a
// no-op and Assist runs without ever having been handed a Host.
//
// initOnce is what makes this safe to call even when the same impl's
// primary Kind ALSO shares this process (multi-Kind single-process
// dispense): whichever of the two InitHost calls — this one or the primary
// Kind's — reaches initOnce first does the real Init; the other reuses that
// outcome instead of layering a second Init on top.
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once
// InitHost returns, so an assist impl must not retain ctx for background
// work started here.
func (s *assistServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	e, ok := s.impl.(ext.Extension)
	if !ok {
		return &pb.ErrorReply{}, nil
	}
	err := initOnce(ctx, e, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

// Assist runs the named form, decoding input_json into the map[string]any
// ext.Assist.Assist expects and encoding whatever it returns back to
// result_json — neither shape is known ahead of time, so JSON is the wire
// format rather than a typed message (mirrors actionServer.Invoke).
func (s *assistServer) Assist(ctx context.Context, req *pb.AssistRequest) (*pb.AssistReply, error) {
	var input map[string]any
	if len(req.InputJson) > 0 {
		if err := json.Unmarshal(req.InputJson, &input); err != nil {
			return &pb.AssistReply{Error: pb.ErrorToProto(fmt.Errorf("grpcplugin: decode input_json: %w", err))}, nil
		}
	}
	result, err := s.impl.Assist(ctx, ext.Principal(req.Principal), req.FormId, input)
	if err != nil {
		return &pb.AssistReply{Error: pb.ErrorToProto(err)}, nil
	}
	rj, err := json.Marshal(result)
	if err != nil {
		return &pb.AssistReply{Error: pb.ErrorToProto(fmt.Errorf("grpcplugin: encode result: %w", err))}, nil
	}
	return &pb.AssistReply{ResultJson: rj}, nil
}
