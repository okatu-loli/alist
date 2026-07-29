// Package bridge adapts a legacy internal/driver.Driver into an
// ext.StorageDriver (9.1 bridge). This is the mechanism by which the built-in
// local driver "exercises the ext types end-to-end" without rewriting every
// driver into ext-native form: the server keeps using the legacy interface on
// its hot path, while this adapter presents the same driver through the frozen
// ext contract (ObjRef/Obj/Link/UploadStream).
//
// This is explicitly the 9.2 bridge seam. When the gRPC executor lands (9.2),
// ext-native drivers implement ext.StorageDriver directly and the translation
// here is replaced by the wire codec; until then this adapter is the single
// place model.* <-> ext.* conversions live.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
)

// StorageBridge wraps a legacy storage driver constructor and presents it as an
// ext.StorageDriver. Instances are keyed by storageID (ext routes ops by
// ObjRef.StorageID, 9.0 §3).
type StorageBridge struct {
	constructor func() driver.Driver
	manifest    ext.Manifest

	mu        sync.Mutex
	instances map[string]driver.Driver

	// streams holds live ByteSourceStream sessions keyed by StreamID: a Link
	// returns a StreamID, OpenRange later serves bytes from the stored session.
	streams   sync.Map // streamID -> *streamSession
	streamSeq atomic.Uint64
}

type streamSession struct {
	link *model.Link
	obj  model.Obj
	size int64 // total byte length of the source, resolved at Link time
}

// NewStorageBridge builds a bridge over a legacy driver constructor with the
// given synthesized manifest.
//
// A StorageBridge always implements ext.RangeOpener — it is the host-side server
// for the stream-source links it emits — so its advertised manifest declares
// CapRangeOpener even though the 9.1 legacy capability probe (which inspects
// legacy driver interfaces) cannot see it. Without this, the gRPC host proxy,
// which dispatches on declared capabilities (9.0 §4), would gate OpenRange out
// and stream-source reads would fail over the wire while working in-process.
func NewStorageBridge(constructor func() driver.Driver, manifest ext.Manifest) *StorageBridge {
	manifest.Capabilities = withRangeOpener(manifest.Capabilities)
	return &StorageBridge{
		constructor: constructor,
		manifest:    manifest,
		instances:   make(map[string]driver.Driver),
	}
}

func withRangeOpener(caps []ext.Capability) []ext.Capability {
	for _, c := range caps {
		if c == ext.CapRangeOpener {
			return caps
		}
	}
	out := make([]ext.Capability, len(caps), len(caps)+1)
	copy(out, caps)
	return append(out, ext.CapRangeOpener)
}

var (
	_ ext.StorageDriver = (*StorageBridge)(nil)
	_ ext.RangeOpener   = (*StorageBridge)(nil)
)

// --- Extension (process-level) ---

func (b *StorageBridge) Manifest() ext.Manifest { return b.manifest }

func (b *StorageBridge) Init(ctx context.Context, host ext.Host) error { return nil }

func (b *StorageBridge) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, d := range b.instances {
		_ = d.Drop(ctx)
		delete(b.instances, id)
	}
	return nil
}

func (b *StorageBridge) DriverConfig() ext.DriverConfig {
	c := b.constructor().Config()
	return ext.DriverConfig{
		Name:              c.Name,
		LocalSort:         c.LocalSort,
		OnlyLocal:         c.OnlyLocal,
		OnlyProxy:         c.OnlyProxy,
		NoCache:           c.NoCache,
		NoUpload:          c.NoUpload,
		DefaultRoot:       c.DefaultRoot,
		CheckStatus:       c.CheckStatus,
		Alert:             c.Alert,
		NoOverwriteUpload: c.NoOverwriteUpload,
		ProxyRangeOption:  c.ProxyRangeOption,
	}
}

// --- instance lifecycle ---

func (b *StorageBridge) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	d := b.constructor()
	storage := model.Storage{Driver: b.manifest.ID, Addition: string(config)}
	d.SetStorage(storage)
	if err := utilsUnmarshalAddition(d, config); err != nil {
		return err
	}
	if err := d.Init(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	b.instances[storageID] = d
	b.mu.Unlock()
	return nil
}

func (b *StorageBridge) DropInstance(ctx context.Context, storageID string) error {
	b.mu.Lock()
	d, ok := b.instances[storageID]
	delete(b.instances, storageID)
	b.mu.Unlock()
	if !ok {
		return nil
	}
	return d.Drop(ctx)
}

func (b *StorageBridge) instance(storageID string) (driver.Driver, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.instances[storageID]
	if !ok {
		return nil, ext.NewError(ext.ErrCodeStorageNotFound, "no instance for storage "+storageID)
	}
	return d, nil
}

// --- required ops ---

func (b *StorageBridge) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	d, err := b.instance(dir.StorageID)
	if err != nil {
		return nil, err
	}
	objs, err := d.List(ctx, refToObj(dir), model.ListArgs{ReqPath: args.ReqPath, Refresh: args.Refresh})
	if err != nil {
		return nil, err
	}
	out := make([]ext.Obj, 0, len(objs))
	for _, o := range objs {
		out = append(out, objToExt(o))
	}
	return out, nil
}

func (b *StorageBridge) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	d, err := b.instance(file.StorageID)
	if err != nil {
		return nil, err
	}
	obj := refToObj(file)
	link, err := d.Link(ctx, obj, model.LinkArgs{IP: args.IP, Header: args.Header, Type: args.Type, Redirect: args.Redirect})
	if err != nil {
		return nil, err
	}
	return b.legacyLinkToExt(file, obj, link), nil
}

// legacyLinkToExt maps a legacy model.Link into an ext.Link. A URL link becomes
// ByteSourceURL; anything with live handles (MFile/RangeReadCloser) becomes
// ByteSourceStream with a StreamID the host echoes back to OpenRange (9.0 §1).
func (b *StorageBridge) legacyLinkToExt(ref ext.ObjRef, obj model.Obj, link *model.Link) *ext.Link {
	out := &ext.Link{
		Concurrency: link.Concurrency,
		PartSize:    link.PartSize,
		IPCacheKey:  link.IPCacheKey,
	}
	if link.Expiration != nil {
		out.Expiration = int64(link.Expiration.Seconds())
	}
	if link.URL != "" {
		out.ByteSource = ext.ByteSourceURL
		out.URL = link.URL
		out.Header = link.Header
		out.RangeSupport = true
		return out
	}
	// stream byte source: MFile / RangeReadCloser
	id := fmt.Sprintf("%s#%d", ref.StorageID, b.streamSeq.Add(1))
	b.streams.Store(id, &streamSession{link: link, obj: obj, size: streamSize(link, obj)})
	out.ByteSource = ext.ByteSourceStream
	out.StreamID = id
	out.RangeSupport = link.MFile != nil || link.RangeReadCloser != nil
	return out
}

// streamSize resolves the total byte length of a stream link. For an MFile it
// seeks to the end (ReadAt used later is offset-independent, so restoring the
// offset is a courtesy); otherwise it falls back to the object's declared size.
func streamSize(link *model.Link, obj model.Obj) int64 {
	if link.MFile != nil {
		if end, err := link.MFile.Seek(0, io.SeekEnd); err == nil {
			_, _ = link.MFile.Seek(0, io.SeekStart)
			return end
		}
	}
	if obj != nil {
		return obj.GetSize()
	}
	return 0
}

// OpenRange serves bytes for a ByteSourceStream link (ext.RangeOpener). It reads
// the requested range from the legacy MFile/RangeReadCloser stored under
// streamID (9.0 §1).
func (b *StorageBridge) OpenRange(ctx context.Context, ref ext.ObjRef, streamID string, rng ext.Range) (io.ReadCloser, error) {
	v, ok := b.streams.Load(streamID)
	if !ok {
		return nil, ext.NewError(ext.ErrCodeObjectNotFound, "no stream session "+streamID)
	}
	sess := v.(*streamSession)
	length := rng.Length
	if length < 0 {
		length = sess.size - rng.Start
	}
	if sess.link.MFile != nil {
		sr := io.NewSectionReader(sess.link.MFile, rng.Start, length)
		return io.NopCloser(sr), nil
	}
	if sess.link.RangeReadCloser != nil {
		return sess.link.RangeReadCloser.RangeRead(ctx, httpRange(rng.Start, length))
	}
	return nil, ext.NewError(ext.ErrCodeNotSupport, "link has no stream byte source")
}

// CloseStream releases a stream session (called by the host once the bytes are
// consumed). Best-effort: closes the underlying handle if any.
func (b *StorageBridge) CloseStream(streamID string) {
	if v, ok := b.streams.LoadAndDelete(streamID); ok {
		sess := v.(*streamSession)
		if sess.link.MFile != nil {
			_ = sess.link.MFile.Close()
		}
		if sess.link.RangeReadCloser != nil {
			_ = sess.link.RangeReadCloser.Close()
		}
	}
}
