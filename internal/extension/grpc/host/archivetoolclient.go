package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// archiveToolExtsRPCTimeout bounds Exts's RPC: ext.ArchiveTool.Exts takes no
// ctx (the interface's signature is fixed), so this client has no
// caller-supplied deadline to forward and would otherwise block forever
// against a wedged or crashed-but-not-yet-reaped subprocess. Exts is read on
// cold paths only (FindArchiveToolForExt's scan), never on a per-byte hot
// path, so a generous fixed timeout is the pragmatic fix. Mirrors
// SearchIndexerClient's searchIndexerConfigRPCTimeout.
const archiveToolExtsRPCTimeout = 10 * time.Second

// ArchiveToolClient is a host-side proxy over a gRPC ArchiveTool plugin
// (Phase E wave 2). It satisfies ext.ArchiveTool by forwarding every call over
// gRPC -- mirrors SearchIndexerClient/OfflineDownloadToolClient one Kind over.
type ArchiveToolClient struct {
	pc  *plugin.Client
	svc pb.ArchiveToolServiceClient

	manifest ext.Manifest
}

var _ ext.ArchiveTool = (*ArchiveToolClient)(nil)

// LaunchArchiveTool starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the ArchiveToolService client, and reads the
// manifest. The returned client owns the subprocess; call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6). Mirrors
// LaunchSearchIndexer/LaunchOfflineDownloadTool.
func LaunchArchiveTool(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*ArchiveToolClient, error) {
	plugins := grpcplugin.ArchiveToolPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostArchiveToolPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: archive tool plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.ArchiveToolPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.ArchiveToolPluginName, err)
	}
	svc, ok := raw.(pb.ArchiveToolServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an ArchiveToolServiceClient", raw)
	}
	c := &ArchiveToolClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *ArchiveToolClient) loadManifest(ctx context.Context) error {
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
// Client.Exited (host/client.go) for the rationale -- same go-plugin reaper.
func (c *ArchiveToolClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only. Mirrors Client.Pid (host/client.go).
func (c *ArchiveToolClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *ArchiveToolClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the
// subprocess.
func (c *ArchiveToolClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *ArchiveToolClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.ArchiveTool ---

// Exts forwards to the plugin's Exts RPC. Bounded by archiveToolExtsRPCTimeout
// (see its doc comment): the ext.ArchiveTool contract gives Exts no ctx to
// forward.
func (c *ArchiveToolClient) Exts() []string {
	ctx, cancel := context.WithTimeout(context.Background(), archiveToolExtsRPCTimeout)
	defer cancel()
	reply, err := c.svc.Exts(ctx, &pb.Empty{})
	if err != nil {
		return nil
	}
	return reply.Exts
}

func archiveSourceToProto(src ext.ArchiveSource) *pb.ArchiveSourceProto {
	// ReaderAt never crosses the wire -- see ext.ArchiveSource's doc comment;
	// internal/archive's dispatch layer always populates Path/Size (never
	// ReaderAt) when it resolves to a remote (gRPC) tool.
	return &pb.ArchiveSourceProto{Path: src.Path, Size: src.Size}
}

// Meta forwards ext.ArchiveTool.Meta over the wire, decoding the resulting
// ArchiveMetaReply into ext.ArchiveToolMeta.
func (c *ArchiveToolClient) Meta(ctx context.Context, src ext.ArchiveSource, password string) (ext.ArchiveToolMeta, error) {
	reply, err := c.svc.Meta(ctx, &pb.ArchiveMetaRequest{Src: archiveSourceToProto(src), Password: password})
	if err != nil {
		return ext.ArchiveToolMeta{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.ArchiveToolMeta{}, e
	}
	return ext.ArchiveToolMeta{
		Comment:               reply.Comment,
		Encrypted:             reply.Encrypted,
		EntryCount:            int(reply.EntryCount),
		TotalUncompressedSize: reply.TotalUncompressedSize,
	}, nil
}

// List forwards ext.ArchiveTool.List over the wire, decoding the resulting
// ArchiveListReply into a []ext.ArchiveEntry.
func (c *ArchiveToolClient) List(ctx context.Context, src ext.ArchiveSource, innerPath, password string) ([]ext.ArchiveEntry, error) {
	reply, err := c.svc.List(ctx, &pb.ArchiveListRequest{Src: archiveSourceToProto(src), InnerPath: innerPath, Password: password})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	entries := make([]ext.ArchiveEntry, len(reply.Entries))
	for i, e := range reply.Entries {
		entries[i] = ext.ArchiveEntry{
			Name:     e.Name,
			IsDir:    e.IsDir,
			Size:     e.Size,
			Modified: time.Unix(0, e.ModifiedUnixNano),
		}
	}
	return entries, nil
}

// Extract forwards ext.ArchiveTool.Extract, turning Extract's server stream
// into an io.ReadCloser -- mirrors Client.OpenRange (host/client.go)'s
// rangeReader adapter, plus decoding the first chunk's size field.
func (c *ArchiveToolClient) Extract(ctx context.Context, src ext.ArchiveSource, innerPath, password string) (io.ReadCloser, int64, error) {
	stream, err := c.svc.Extract(ctx, &pb.ArchiveExtractRequest{Src: archiveSourceToProto(src), InnerPath: innerPath, Password: password})
	if err != nil {
		return nil, 0, err
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, 0, err
	}
	if e := pb.ErrorFromProto(first.Error); e != nil {
		return nil, 0, e
	}
	r := &archiveExtractReader{stream: stream}
	if len(first.Data) > 0 {
		r.buf = first.Data
	}
	return r, first.Size, nil
}

// archiveExtractReader turns ArchiveToolService.Extract's server stream into
// an io.ReadCloser. Mirrors rangeReader (host/client.go), with an initial buf
// seeded from the first message's data (if any) since that message also
// carried the entry's size.
type archiveExtractReader struct {
	stream pb.ArchiveToolService_ExtractClient
	buf    []byte
	err    error
}

func (r *archiveExtractReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		chunk, err := r.stream.Recv()
		if err != nil { // io.EOF at clean stream end
			r.err = err
			return 0, err
		}
		if e := pb.ErrorFromProto(chunk.Error); e != nil {
			r.err = e
			return 0, e
		}
		r.buf = chunk.Data
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *archiveExtractReader) Close() error { return nil }
