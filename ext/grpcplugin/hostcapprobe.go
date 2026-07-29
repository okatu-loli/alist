package grpcplugin

import (
	"context"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// This file wires the F0 reverse-bridge integration fixture: a plugin that
// exercises borrowed Host capabilities over HostService and reports the result.
// It is the mechanism the real protocol/login plugins will reuse — a plugin
// receives a host_broker_id, Dials it to reach HostService, and drives an
// ext.Host — so it lives in the SDK module beside the forward wiring.

// ProbeFunc is the plugin author's capability exercise: given a live ext.Host
// (backed by the reverse bridge) and an op + args, do the borrow and return a
// string result. The host integration test asserts on that result.
type ProbeFunc func(ctx context.Context, host ext.Host, op, arg, arg2 string) (string, error)

// HostCapProbePlugin is the go-plugin Plugin for the probe fixture. Both sides
// capture the GRPCBroker: the plugin side needs it to Dial back to HostService;
// the host side exposes it on the dispensed client so the test can NextId() +
// AcceptAndServe() the HostService before calling Exercise.
type HostCapProbePlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ProbeFunc
}

var _ plugin.GRPCPlugin = (*HostCapProbePlugin)(nil)

func (p *HostCapProbePlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterHostCapProbeServiceServer(s, &probeServer{impl: p.Impl, broker: broker, hosts: map[uint32]ext.Host{}})
	return nil
}

func (p *HostCapProbePlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &ProbeClient{svc: pb.NewHostCapProbeServiceClient(c), Broker: broker}, nil
}

// HostCapProbePluginMap is the go-plugin plugin set served/dispensed under the
// probe dispense name.
func HostCapProbePluginMap(impl ProbeFunc) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.HostCapProbePluginName: &HostCapProbePlugin{Impl: impl},
	}
}

// ServeHostCapProbe runs impl as the probe plugin. A fixture binary's main is
// simply: func main() { grpcplugin.ServeHostCapProbe(myProbe) }.
func ServeHostCapProbe(impl ProbeFunc) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         HostCapProbePluginMap(impl),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// probeServer is the plugin-side HostCapProbeService. Exercise Dials the host's
// HostService on the supplied broker id, wraps it as an ext.Host, and runs the
// author's ProbeFunc against it — driving the real reverse bridge.
//
// A go-plugin broker id is a single-use rendezvous for ONE connection, and the
// host serves one HostService (bound to one plugin grant) per id for the
// plugin's lifetime. So Exercise Dials each id at most once and caches the
// resulting ext.Host, reusing the long-lived connection for every subsequent
// call — the same "inject the Host handle once at startup" lifecycle a real
// plugin follows.
type probeServer struct {
	pb.UnimplementedHostCapProbeServiceServer
	impl   ProbeFunc
	broker *plugin.GRPCBroker

	mu    sync.Mutex
	hosts map[uint32]ext.Host
}

func (s *probeServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	// A minimal manifest; the fixture declares no capabilities of its own.
	return &pb.ManifestReply{ManifestJson: []byte(`{"id":"fixture.hostcap.probe"}`)}, nil
}

func (s *probeServer) hostFor(id uint32) (ext.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.hosts[id]; ok {
		return h, nil
	}
	conn, err := s.broker.Dial(id)
	if err != nil {
		return nil, err
	}
	h := NewHostClient(conn)
	s.hosts[id] = h
	return h, nil
}

func (s *probeServer) Exercise(ctx context.Context, req *pb.ExerciseRequest) (*pb.ExerciseReply, error) {
	host, err := s.hostFor(req.HostBrokerId)
	if err != nil {
		return &pb.ExerciseReply{Error: pb.ErrorToProto(err)}, nil
	}
	result, err := s.impl(ctx, host, req.Op, req.Arg, req.Arg2)
	if err != nil {
		return &pb.ExerciseReply{Result: result, Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ExerciseReply{Result: result}, nil
}

// ProbeClient is the host-side handle to a launched probe plugin. Broker is the
// go-plugin broker the host must NextId()/AcceptAndServe() on to stand up the
// HostService the plugin will Dial.
type ProbeClient struct {
	svc    pb.HostCapProbeServiceClient
	Broker *plugin.GRPCBroker
}

// Exercise asks the plugin to borrow a capability. hostBrokerID is the broker id
// on which the host has already begun serving HostService.
func (c *ProbeClient) Exercise(ctx context.Context, hostBrokerID uint32, op, arg, arg2 string) (string, error) {
	reply, err := c.svc.Exercise(ctx, &pb.ExerciseRequest{HostBrokerId: hostBrokerID, Op: op, Arg: arg, Arg2: arg2})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return reply.Result, e
	}
	return reply.Result, nil
}
