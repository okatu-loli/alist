package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// searchIndexerConfigRPCTimeout bounds Config's RPC: ext.SearchIndexer.Config
// takes no ctx (the interface's signature is fixed — it is also called
// in-process with no context to thread through), so this client has no
// caller-supplied deadline to forward and would otherwise block forever
// against a wedged or crashed-but-not-yet-reaped subprocess. Config is read
// on cold paths only (FindSearchIndexerByName's scan, the settings page's
// SearchIndexerGRPCNames enumeration), never on a request hot path, so a
// generous fixed timeout is the pragmatic fix rather than plumbing ctx
// through the whole ext.SearchIndexer contract for this one method. Mirrors
// OfflineDownloadToolClient's configRPCTimeout.
const searchIndexerConfigRPCTimeout = 10 * time.Second

// SearchIndexerClient is a host-side proxy over a gRPC SearchIndexer plugin
// (Phase E ring 1b). It satisfies ext.SearchIndexer by forwarding every call
// over gRPC — mirrors OfflineDownloadToolClient one Kind over.
type SearchIndexerClient struct {
	pc  *plugin.Client
	svc pb.SearchIndexerServiceClient

	manifest ext.Manifest
}

var _ ext.SearchIndexer = (*SearchIndexerClient)(nil)

// LaunchSearchIndexer starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the SearchIndexerService client, and reads
// the manifest. The returned client owns the subprocess; call Shutdown to
// stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchOfflineDownloadTool (host/
// offlinedownloadtoolclient.go).
func LaunchSearchIndexer(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*SearchIndexerClient, error) {
	plugins := grpcplugin.SearchIndexerPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostSearchIndexerPluginMap(NewHostServiceServer(hostBuilder()))
	}
	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  grpcplugin.Handshake,
		Plugins:          plugins,
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           hclog.NewNullLogger(),
	})
	rpcClient, err := pc.Client()
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: search indexer plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.SearchIndexerPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.SearchIndexerPluginName, err)
	}
	svc, ok := raw.(pb.SearchIndexerServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not a SearchIndexerServiceClient", raw)
	}
	c := &SearchIndexerClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *SearchIndexerClient) loadManifest(ctx context.Context) error {
	mr, err := c.svc.GetManifest(ctx, &pb.Empty{})
	if err != nil {
		return fmt.Errorf("host: GetManifest: %w", err)
	}
	if err := json.Unmarshal(mr.ManifestJson, &c.manifest); err != nil {
		return fmt.Errorf("host: decode manifest: %w", err)
	}
	return nil
}

// Exited reports whether the plugin subprocess has already died. See
// Client.Exited (host/client.go) for the rationale — same go-plugin reaper.
func (c *SearchIndexerClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *SearchIndexerClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *SearchIndexerClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the
// subprocess.
func (c *SearchIndexerClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *SearchIndexerClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.SearchIndexer ---

// Config forwards to the plugin's Config RPC, decoding SearchIndexerConfigReply
// into ext.SearchIndexerConfig. Bounded by searchIndexerConfigRPCTimeout (see
// its doc comment): the ext.SearchIndexer contract gives Config no ctx to
// forward, so this client cannot inherit a caller deadline and must set its
// own rather than risk blocking forever on a wedged subprocess.
func (c *SearchIndexerClient) Config() ext.SearchIndexerConfig {
	ctx, cancel := context.WithTimeout(context.Background(), searchIndexerConfigRPCTimeout)
	defer cancel()
	reply, err := c.svc.Config(ctx, &pb.Empty{})
	if err != nil {
		return ext.SearchIndexerConfig{}
	}
	return ext.SearchIndexerConfig{Name: reply.Name, AutoUpdate: reply.AutoUpdate}
}

func searchNodeFromProto(n *pb.SearchNodeProto) ext.SearchNode {
	if n == nil {
		return ext.SearchNode{}
	}
	return ext.SearchNode{Parent: n.Parent, Name: n.Name, IsDir: n.IsDir, Size: n.Size}
}

func searchNodeToProto(n ext.SearchNode) *pb.SearchNodeProto {
	return &pb.SearchNodeProto{Parent: n.Parent, Name: n.Name, IsDir: n.IsDir, Size: n.Size}
}

// Search forwards ext.SearchIndexer.Search over the wire, decoding the
// resulting SearchReply into the hits slice plus total count.
func (c *SearchIndexerClient) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	reply, err := c.svc.Search(ctx, &pb.SearchReqProto{
		Parent:   req.Parent,
		Keywords: req.Keywords,
		Scope:    int32(req.Scope),
		Page:     int32(req.Page),
		PerPage:  int32(req.PerPage),
	})
	if err != nil {
		return nil, 0, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, 0, e
	}
	nodes := make([]ext.SearchNode, len(reply.Nodes))
	for i, n := range reply.Nodes {
		nodes[i] = searchNodeFromProto(n)
	}
	return nodes, reply.Total, nil
}

// Index encodes node as a SearchNodeProto and forwards it over the wire.
func (c *SearchIndexerClient) Index(ctx context.Context, node ext.SearchNode) error {
	reply, err := c.svc.Index(ctx, &pb.IndexRequest{Node: searchNodeToProto(node)})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

// BatchIndex encodes the whole node batch into a single BatchIndexRequest and
// forwards it over the wire in one RPC.
func (c *SearchIndexerClient) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	protoNodes := make([]*pb.SearchNodeProto, len(nodes))
	for i, n := range nodes {
		protoNodes[i] = searchNodeToProto(n)
	}
	reply, err := c.svc.BatchIndex(ctx, &pb.BatchIndexRequest{Nodes: protoNodes})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

// Get forwards ext.SearchIndexer.Get over the wire, decoding the resulting
// IndexGetReply into a SearchNode slice.
func (c *SearchIndexerClient) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	reply, err := c.svc.Get(ctx, &pb.IndexGetRequest{Parent: parent})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	nodes := make([]ext.SearchNode, len(reply.Nodes))
	for i, n := range reply.Nodes {
		nodes[i] = searchNodeFromProto(n)
	}
	return nodes, nil
}

// Del forwards ext.SearchIndexer.Del over the wire.
func (c *SearchIndexerClient) Del(ctx context.Context, prefix string) error {
	reply, err := c.svc.Del(ctx, &pb.IndexDelRequest{Prefix: prefix})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

// Clear forwards ext.SearchIndexer.Clear over the wire.
func (c *SearchIndexerClient) Clear(ctx context.Context) error {
	reply, err := c.svc.Clear(ctx, &pb.Empty{})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}
