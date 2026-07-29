package grpcplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// ArchiveToolPlugin is the go-plugin Plugin for a Kind=ArchiveTool extension
// (Phase E wave 2): an archive format internal/archive's dispatch layer looks
// inside alongside the built-in zip tool (rar/7z/...). Mirrors
// OfflineDownloadToolPlugin one Kind over: the plugin side sets Impl and only
// GRPCServer is used; the host side leaves Impl nil and only GRPCClient is
// used.
type ArchiveToolPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl ext.ArchiveTool // plugin side

	// HostServerFactory, when set (HOST side), makes GRPCClient stand up a
	// HostService for this subprocess on a broker id and hand that id to the
	// plugin via InitHost, so the plugin can borrow host capabilities (F0/SP6).
	// nil on the plugin side and for launches with no reverse host (unchanged
	// behavior).
	HostServerFactory func([]grpc.ServerOption) *grpc.Server
}

var _ plugin.GRPCPlugin = (*ArchiveToolPlugin)(nil)

func (p *ArchiveToolPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterArchiveToolServiceServer(s, &archiveToolServer{impl: p.Impl, broker: broker})
	return nil
}

func (p *ArchiveToolPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	client := pb.NewArchiveToolServiceClient(c)
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

// ArchiveToolPluginMap is the go-plugin plugin set served/dispensed under
// pb.ArchiveToolPluginName. impl also gets auto-mounted under
// pb.AssistPluginName if it implements ext.Assist (mountAssist,
// assistserve.go).
func ArchiveToolPluginMap(impl ext.ArchiveTool) map[string]plugin.Plugin {
	return mountAssist(map[string]plugin.Plugin{
		pb.ArchiveToolPluginName: &ArchiveToolPlugin{Impl: impl},
	}, impl)
}

// HostArchiveToolPluginMap is the host-side plugin set that wires a reverse
// HostService (serverFactory) into the dispensed ArchiveTool, so the plugin's
// Init gets a working host. serverFactory is host.NewHostServiceServer(theHost)
// -- an opaque grpc.Server factory (the SDK cannot import the host's
// grpc/host package).
func HostArchiveToolPluginMap(serverFactory func([]grpc.ServerOption) *grpc.Server) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		pb.ArchiveToolPluginName: &ArchiveToolPlugin{HostServerFactory: serverFactory},
		pb.AssistPluginName:      &AssistPlugin{HostServerFactory: serverFactory},
	}
}

// ServeArchiveTool runs p as a go-plugin gRPC plugin. An out-of-tree
// ArchiveTool binary's main is simply:
// func main() { grpcplugin.ServeArchiveTool(myTool) }.
func ServeArchiveTool(p ext.ArchiveTool) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         ArchiveToolPluginMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// archiveToolServer adapts an ext.ArchiveTool to the generated
// ArchiveToolServiceServer.
type archiveToolServer struct {
	pb.UnimplementedArchiveToolServiceServer
	impl   ext.ArchiveTool
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the tool via Init -- this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
// Mirrors offlineDownloadToolServer.InitHost.
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a tool must not retain ctx for background work started here.
func (s *archiveToolServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.impl, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *archiveToolServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.impl.Manifest())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj}, nil
}

// Exts forwards ext.ArchiveTool.Exts, the extensions this tool claims (what
// the host dispatch layer matches a requested archive's extension against).
func (s *archiveToolServer) Exts(ctx context.Context, _ *pb.Empty) (*pb.ArchiveExtsReply, error) {
	return &pb.ArchiveExtsReply{Exts: s.impl.Exts()}, nil
}

func archiveSourceFromProto(src *pb.ArchiveSourceProto) ext.ArchiveSource {
	if src == nil {
		return ext.ArchiveSource{}
	}
	// ReaderAt is deliberately left nil: a gRPC ArchiveTool always receives a
	// host-localized temp file (Path/Size), never a live in-process ReaderAt
	// (see ext.ArchiveSource's doc comment).
	return ext.ArchiveSource{Path: src.Path, Size: src.Size}
}

// Meta decodes ArchiveMetaRequest and forwards it to the plugin
// (ext.ArchiveTool.Meta). Its error, if any, crosses back as an ErrorProto
// field on ArchiveMetaReply rather than a transport error, mirroring every
// other business-method leg in this SDK.
func (s *archiveToolServer) Meta(ctx context.Context, req *pb.ArchiveMetaRequest) (*pb.ArchiveMetaReply, error) {
	m, err := s.impl.Meta(ctx, archiveSourceFromProto(req.Src), req.Password)
	if err != nil {
		return &pb.ArchiveMetaReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ArchiveMetaReply{
		Comment:               m.Comment,
		Encrypted:             m.Encrypted,
		EntryCount:            int32(m.EntryCount),
		TotalUncompressedSize: m.TotalUncompressedSize,
	}, nil
}

// List forwards ext.ArchiveTool.List, encoding the resulting []ext.ArchiveEntry
// as ArchiveEntryProto messages.
func (s *archiveToolServer) List(ctx context.Context, req *pb.ArchiveListRequest) (*pb.ArchiveListReply, error) {
	entries, err := s.impl.List(ctx, archiveSourceFromProto(req.Src), req.InnerPath, req.Password)
	if err != nil {
		return &pb.ArchiveListReply{Error: pb.ErrorToProto(err)}, nil
	}
	protoEntries := make([]*pb.ArchiveEntryProto, len(entries))
	for i, e := range entries {
		protoEntries[i] = &pb.ArchiveEntryProto{
			Name:             e.Name,
			IsDir:            e.IsDir,
			Size:             e.Size,
			ModifiedUnixNano: e.Modified.UnixNano(),
		}
	}
	return &pb.ArchiveListReply{Entries: protoEntries}, nil
}

// Extract forwards ext.ArchiveTool.Extract and server-streams the resulting
// io.ReadCloser as ArchiveChunk messages -- the first chunk carries the
// entry's total size, mirroring driverServer.OpenRange's streaming shape.
func (s *archiveToolServer) Extract(req *pb.ArchiveExtractRequest, stream pb.ArchiveToolService_ExtractServer) error {
	rc, size, err := s.impl.Extract(stream.Context(), archiveSourceFromProto(req.Src), req.InnerPath, req.Password)
	if err != nil {
		return stream.Send(&pb.ArchiveChunk{Error: pb.ErrorToProto(err)})
	}
	defer rc.Close()

	if err := stream.Send(&pb.ArchiveChunk{Size: size}); err != nil {
		return err
	}
	buf := make([]byte, pb.ChunkSize)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.ArchiveChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return stream.Send(&pb.ArchiveChunk{Error: pb.ErrorToProto(rerr)})
		}
	}
}
