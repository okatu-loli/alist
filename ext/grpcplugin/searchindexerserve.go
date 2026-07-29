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

// SearchIndexerPlugin is the go-plugin Plugin for a Kind=SearchIndexer
// extension (Phase E ring 1b): a real-index file-search backend (database/
// bleve/meilisearch-alike). Mirrors OfflineDownloadToolPlugin one Kind over:
// the plugin side sets Impl and only GRPCServer is used; the host side leaves
// Impl nil and only GRPCClient is used.
type SearchIndexerPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.SearchIndexer // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*SearchIndexerPlugin)(nil)

func (p *SearchIndexerPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterSearchIndexerServiceServer(s, &searchIndexerServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *SearchIndexerPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewSearchIndexerServiceClient(c)
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

// SearchIndexerPluginMap is the go-plugin plugin set served/dispensed under
// pb.SearchIndexerPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func SearchIndexerPluginMap(impl ext.SearchIndexer) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.SearchIndexerPluginName: &SearchIndexerPlugin{Impl: impl},
	}, impl)
}

// HostSearchIndexerPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed SearchIndexer, so the
// plugin's Init gets a working host. serverFactory is
// host.NewHostServiceServer(theHost) — an opaque grpc.Server factory (the SDK
// cannot import the host's grpc/host package).
func HostSearchIndexerPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.SearchIndexerPluginName: &SearchIndexerPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:        &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeSearchIndexer runs p as a go-plugin gRPC plugin. An out-of-tree
// SearchIndexer binary's main is simply:
// func main() { grpcplugin.ServeSearchIndexer(myIndexer) }.
func ServeSearchIndexer(p ext.SearchIndexer) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         SearchIndexerPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// searchIndexerServer adapts an ext.SearchIndexer to the generated
// SearchIndexerServiceServer.
type searchIndexerServer struct {
	pb.UnimplementedSearchIndexerServiceServer
	impl   ext.SearchIndexer
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the indexer via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors offlineDownloadToolServer.InitHost.
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so an indexer must not retain ctx for background work started
// here.
func (s *searchIndexerServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *searchIndexerServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// Config forwards ext.SearchIndexer.Config, the backend's self-reported
// identity (the Name the search_index setting selects on, plus whether it
// supports incremental auto-update).
func (s *searchIndexerServer) Config(ctx context.Context, _ *pb.Empty) (*pb.SearchIndexerConfigReply, error) {
	cfg := s.impl.Config()
	return &pb.SearchIndexerConfigReply{Name: cfg.Name, AutoUpdate: cfg.AutoUpdate}, nil
}

func searchNodeToProto(n ext.SearchNode) *pb.SearchNodeProto {
	return &pb.SearchNodeProto{Parent: n.Parent, Name: n.Name, IsDir: n.IsDir, Size: n.Size}
}

func searchNodeFromProto(n *pb.SearchNodeProto) ext.SearchNode {
	if n == nil {
		return ext.SearchNode{}
	}
	return ext.SearchNode{Parent: n.Parent, Name: n.Name, IsDir: n.IsDir, Size: n.Size}
}

// Search forwards ext.SearchIndexer.Search, encoding the resulting hits and
// total count into a SearchReply. Its error, if any, crosses back as an
// ErrorReply field rather than a transport error, mirroring every other
// business-method leg in this SDK.
func (s *searchIndexerServer) Search(ctx context.Context, req *pb.SearchReqProto) (*pb.SearchReply, error) {
	nodes, total, err := s.impl.Search(ctx, ext.SearchReq{
		Parent:   req.Parent,
		Keywords: req.Keywords,
		Scope:    int(req.Scope),
		Page:     int(req.Page),
		PerPage:  int(req.PerPage),
	})
	if err != nil {
		return &pb.SearchReply{Error: pb.ErrorToProto(err)}, nil
	}
	out := make([]*pb.SearchNodeProto, len(nodes))
	for i, n := range nodes {
		out[i] = searchNodeToProto(n)
	}
	return &pb.SearchReply{Nodes: out, Total: total}, nil
}

// Index forwards ext.SearchIndexer.Index.
func (s *searchIndexerServer) Index(ctx context.Context, req *pb.IndexRequest) (*pb.ErrorReply, error) {
	if err := s.impl.Index(ctx, searchNodeFromProto(req.Node)); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}

// BatchIndex forwards ext.SearchIndexer.BatchIndex, decoding the whole node
// batch from one RPC.
func (s *searchIndexerServer) BatchIndex(ctx context.Context, req *pb.BatchIndexRequest) (*pb.ErrorReply, error) {
	nodes := make([]ext.SearchNode, len(req.Nodes))
	for i, n := range req.Nodes {
		nodes[i] = searchNodeFromProto(n)
	}
	if err := s.impl.BatchIndex(ctx, nodes); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}

// Get forwards ext.SearchIndexer.Get.
func (s *searchIndexerServer) Get(ctx context.Context, req *pb.IndexGetRequest) (*pb.IndexGetReply, error) {
	nodes, err := s.impl.Get(ctx, req.Parent)
	if err != nil {
		return &pb.IndexGetReply{Error: pb.ErrorToProto(err)}, nil
	}
	out := make([]*pb.SearchNodeProto, len(nodes))
	for i, n := range nodes {
		out[i] = searchNodeToProto(n)
	}
	return &pb.IndexGetReply{Nodes: out}, nil
}

// Del forwards ext.SearchIndexer.Del.
func (s *searchIndexerServer) Del(ctx context.Context, req *pb.IndexDelRequest) (*pb.ErrorReply, error) {
	if err := s.impl.Del(ctx, req.Prefix); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}

// Clear forwards ext.SearchIndexer.Clear.
func (s *searchIndexerServer) Clear(ctx context.Context, _ *pb.Empty) (*pb.ErrorReply, error) {
	if err := s.impl.Clear(ctx); err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ErrorReply{}, nil
}
