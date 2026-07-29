// Package host is the host-side half of the Step 9.2 gRPC executor. Client
// launches a storage-driver plugin subprocess (via hashicorp/go-plugin) and
// implements ext.StorageDriver by forwarding every call over gRPC, so the rest
// of the core can treat a subprocess plugin exactly like an in-process ext
// driver. It is additive: nothing wires it into op dispatch yet (9.3).
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// Client is a host-side proxy over a gRPC storage-driver plugin. It satisfies
// ext.StorageDriver plus every optional ext capability interface; each optional
// method is gated on the plugin's declared Manifest.Capabilities (9.0 §4: a
// single proxy struct cannot express "has Move but not Copy" through Go type
// assertions, so dispatch reads the declaration).
type Client struct {
	pc  *plugin.Client
	svc pb.DriverServiceClient

	manifest ext.Manifest
	config   ext.DriverConfig
	caps     map[ext.Capability]struct{}

	// onProgress, if set, receives Put progress fractions the plugin reports
	// over the Put stream (9.2 folds ReportProgress into that stream).
	onProgress func(float64)
}

var (
	_ ext.StorageDriver = (*Client)(nil)
	_ ext.Getter        = (*Client)(nil)
	_ ext.Mkdirer       = (*Client)(nil)
	_ ext.Mover         = (*Client)(nil)
	_ ext.Renamer       = (*Client)(nil)
	_ ext.Copier        = (*Client)(nil)
	_ ext.Remover       = (*Client)(nil)
	_ ext.Putter        = (*Client)(nil)
	_ ext.Other         = (*Client)(nil)
	_ ext.RangeOpener   = (*Client)(nil)
	_ ext.RapidPutter   = (*Client)(nil)
)

// Launch starts the plugin binary described by cmd, performs the go-plugin
// handshake, dispenses the DriverService client, and reads the manifest. The
// returned Client owns the subprocess; call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0): the host side
// serves a HostService backed by hostBuilder() on a broker id, and the plugin
// dials it and calls impl.Init with a working host. nil (the default) keeps
// pre-bridge behavior — no reverse host is served and the plugin's Init is
// never called.
func Launch(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*Client, error) {
	plugins := grpcplugin.PluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.PluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.PluginName, err)
	}
	svc, ok := raw.(pb.DriverServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not a DriverServiceClient", raw)
	}
	c := &Client{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *Client) loadManifest(ctx context.Context) error {
	mr, err := c.svc.GetManifest(ctx, &pb.Empty{})
	if err != nil {
		return fmt.Errorf("host: GetManifest: %w", err)
	}
	if err := json.Unmarshal(mr.ManifestJson, &c.manifest); err != nil {
		return fmt.Errorf("host: decode manifest: %w", err)
	}
	if len(mr.DriverConfigJson) > 0 {
		if err := json.Unmarshal(mr.DriverConfigJson, &c.config); err != nil {
			return fmt.Errorf("host: decode driver config: %w", err)
		}
	}
	c.caps = make(map[ext.Capability]struct{}, len(c.manifest.Capabilities))
	for _, cap := range c.manifest.Capabilities {
		c.caps[cap] = struct{}{}
	}
	return nil
}

// SetProgressHandler registers a callback for Put progress reports.
func (c *Client) SetProgressHandler(fn func(float64)) { c.onProgress = fn }

// Exited reports whether the plugin subprocess has already died (crashed or
// exited on its own). go-plugin sets this from a background goroutine that
// reaps the process as soon as it's gone, so this is cheap (a mutex + bool
// read) and safe to call on every gRPC call failure to distinguish "the
// subprocess crashed" from an ordinary business/transport error.
func (c *Client) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9).
func (c *Client) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

func (c *Client) has(cap ext.Capability) bool {
	_, ok := c.caps[cap]
	return ok
}

// --- Extension (process-level) ---

func (c *Client) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
// The Host callback surface over gRPC (beyond Put progress) is a 9.2+ seam.
func (c *Client) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *Client) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

func (c *Client) DriverConfig() ext.DriverConfig { return c.config }

// --- instance lifecycle ---

func (c *Client) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	reply, err := c.svc.NewInstance(ctx, &pb.NewInstanceRequest{StorageId: storageID, Config: config})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

func (c *Client) DropInstance(ctx context.Context, storageID string) error {
	reply, err := c.svc.DropInstance(ctx, &pb.DropInstanceRequest{StorageId: storageID})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

// --- required ops ---

func (c *Client) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	reply, err := c.svc.List(ctx, &pb.ListRequest{Dir: pb.RefToProto(dir), Args: pb.ListArgsToProto(args)})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return pb.ObjsFromProto(reply.Objs), nil
}

func (c *Client) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	reply, err := c.svc.Link(ctx, &pb.LinkRequest{File: pb.RefToProto(file), Args: pb.LinkArgsToProto(args)})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return pb.LinkFromProto(reply.Link), nil
}

// --- optional-capability ops (gated on declared Manifest.Capabilities) ---

func (c *Client) Get(ctx context.Context, storageID, path string) (ext.Obj, error) {
	if !c.has(ext.CapGetter) {
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Get")
	}
	reply, err := c.svc.Get(ctx, &pb.GetRequest{StorageId: storageID, Path: path})
	return objFrom(reply, err)
}

func (c *Client) Mkdir(ctx context.Context, parent ext.ObjRef, name string) (ext.Obj, error) {
	if !c.has(ext.CapMkdir) {
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Mkdir")
	}
	reply, err := c.svc.MakeDir(ctx, &pb.MakeDirRequest{Parent: pb.RefToProto(parent), Name: name})
	return objFrom(reply, err)
}

func (c *Client) Rename(ctx context.Context, src ext.ObjRef, newName string) (ext.Obj, error) {
	if !c.has(ext.CapRename) {
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Rename")
	}
	reply, err := c.svc.Rename(ctx, &pb.RenameRequest{Src: pb.RefToProto(src), NewName: newName})
	return objFrom(reply, err)
}

func (c *Client) Move(ctx context.Context, src, dstDir ext.ObjRef) (ext.Obj, error) {
	if !c.has(ext.CapMove) {
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Move")
	}
	reply, err := c.svc.Move(ctx, &pb.MoveRequest{Src: pb.RefToProto(src), DstDir: pb.RefToProto(dstDir)})
	return objFrom(reply, err)
}

func (c *Client) Copy(ctx context.Context, src, dstDir ext.ObjRef) (ext.Obj, error) {
	if !c.has(ext.CapCopy) {
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Copy")
	}
	reply, err := c.svc.Copy(ctx, &pb.CopyRequest{Src: pb.RefToProto(src), DstDir: pb.RefToProto(dstDir)})
	return objFrom(reply, err)
}

// TryRapidUpload forwards a hash-based instant-upload attempt over the wire
// (SP-H). Gated on CapPutRapid exactly like the other optional ops: a plugin
// that never declared it answers NOT_IMPLEMENT without a round trip.
func (c *Client) TryRapidUpload(ctx context.Context, dstDir ext.ObjRef, name string, size int64, hashes map[string]string) (ext.Obj, bool, error) {
	if !c.has(ext.CapPutRapid) {
		return ext.Obj{}, false, ext.NewError(ext.ErrCodeNotImplement, "TryRapidUpload")
	}
	reply, err := c.svc.TryRapidUpload(ctx, &pb.TryRapidUploadRequest{
		DstDir: pb.RefToProto(dstDir), Name: name, Size: size, Hashes: hashes,
	})
	if err != nil {
		return ext.Obj{}, false, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.Obj{}, false, e
	}
	if !reply.Hit {
		return ext.Obj{}, false, nil
	}
	return pb.ObjFromProto(reply.Obj), true, nil
}

func (c *Client) Remove(ctx context.Context, obj ext.ObjRef) error {
	if !c.has(ext.CapRemove) {
		return ext.NewError(ext.ErrCodeNotImplement, "Remove")
	}
	reply, err := c.svc.Remove(ctx, &pb.RemoveRequest{Obj: pb.RefToProto(obj)})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

func (c *Client) Other(ctx context.Context, obj ext.ObjRef, method string, data json.RawMessage) (json.RawMessage, error) {
	if !c.has(ext.CapOther) {
		return nil, ext.NewError(ext.ErrCodeNotImplement, "Other")
	}
	reply, err := c.svc.Other(ctx, &pb.OtherRequest{Obj: pb.RefToProto(obj), Method: method, Data: data})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return reply.Data, nil
}

// OpenRange server-streams the bytes of a ByteSource==stream Link and presents
// them as an io.ReadCloser (9.0 §1).
func (c *Client) OpenRange(ctx context.Context, ref ext.ObjRef, streamID string, rng ext.Range) (io.ReadCloser, error) {
	if !c.has(ext.CapRangeOpener) {
		return nil, ext.NewError(ext.ErrCodeNotImplement, "OpenRange")
	}
	stream, err := c.svc.OpenRange(ctx, &pb.OpenRangeRequest{
		Ref: pb.RefToProto(ref), StreamId: streamID, Range: pb.RangeToProto(rng),
	})
	if err != nil {
		return nil, err
	}
	return &rangeReader{stream: stream}, nil
}

func objFrom(reply *pb.ObjReply, err error) (ext.Obj, error) {
	if err != nil {
		return ext.Obj{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.Obj{}, e
	}
	return pb.ObjFromProto(reply.Obj), nil
}

// rangeReader turns OpenRange's server stream into an io.ReadCloser.
type rangeReader struct {
	stream pb.DriverService_OpenRangeClient
	buf    []byte
	err    error
}

func (r *rangeReader) Read(p []byte) (int, error) {
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

func (r *rangeReader) Close() error { return nil }
