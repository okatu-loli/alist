// This file is the reverse of legacy.go's StorageBridge: NewPluginDriver
// adapts a gRPC host client's ext.StorageDriver into a legacy
// internal/driver.Driver, so op/fs.go can mount a plugin storage exactly like
// any in-process driver (9.3 seam). translateExtErr is the error-code leg of
// that adaptation: once an error has crossed the gRPC wire, errors.Is no
// longer sees through it to op's sentinel errors, so the ext.ErrCode that
// traveled alongside it is mapped back explicitly.
package bridge

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/http_range"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// ProcManager launches (or reuses) a plugin subprocess and hands back its
// shared ext.StorageDriver plus an idempotent release func. Defined here
// (rather than consumed from internal/extension/pluginproc directly) so this
// package does not import pluginproc and create an import cycle;
// pluginproc.Manager satisfies this interface structurally.
type ProcManager interface {
	Acquire(ctx context.Context, pluginID, binPath string) (ext.StorageDriver, func(), error)

	// Restart relaunches pluginID's subprocess after a caller observed
	// staleClient failing because the process died, and returns the fresh
	// client. Implementations must collapse concurrent Restart calls for the
	// same pluginID (from sibling storages sharing it) into a single
	// relaunch — see pluginproc.Manager.Restart.
	Restart(ctx context.Context, pluginID, binPath string, staleClient ext.StorageDriver) (ext.StorageDriver, error)
}

// translateExtErr maps an *ext.Error's cross-boundary code back to op's
// sentinel errors so op/fs.go control flow (errors.Is(err, errs.NotImplement))
// keeps working once an error has crossed the gRPC wire (9.0 §5).
func translateExtErr(err error) error {
	if err == nil {
		return nil
	}
	code, ok := ext.CodeOf(err)
	if !ok {
		return err
	}
	switch code {
	case ext.ErrCodeNotImplement:
		return errs.NotImplement
	case ext.ErrCodeNotSupport:
		return errs.NotSupport
	case ext.ErrCodeObjectNotFound:
		return errs.ObjectNotFound
	case ext.ErrCodePermissionDenied:
		return errs.PermissionDenied
	case ext.ErrCodeStorageNotFound:
		return errs.StorageNotFound
	case ext.ErrCodeWrongArchivePassword:
		return errs.WrongArchivePassword
	default:
		return err
	}
}

// extObjToModel maps an ext.Obj DTO back to a legacy *model.Object, the
// inverse of objToExt in ops.go.
func extObjToModel(o ext.Obj) *model.Object {
	return &model.Object{
		Name:     o.Name,
		Size:     o.Size,
		Modified: time.UnixMilli(o.Modified),
		Ctime:    time.UnixMilli(o.Created),
		IsFolder: o.IsDir,
		ID:       o.ID,
		Path:     o.Path,
		HashInfo: hashesFromMap(o.Hashes),
	}
}

// extConfigToDriver maps an ext.DriverConfig back to a legacy driver.Config,
// the inverse of StorageBridge.DriverConfig's construction in legacy.go.
func extConfigToDriver(c ext.DriverConfig) driver.Config {
	return driver.Config{
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

func hasCap(caps []ext.Capability, target ext.Capability) bool {
	for _, c := range caps {
		if c == target {
			return true
		}
	}
	return false
}

// genericAddition captures a plugin storage's opaque config: root fields for
// compat with the admin "browse for root" UI, plus the raw JSON forwarded
// verbatim to NewInstance — a plugin's Addition shape is unknown to the host,
// which only needs the root_folder_path/root_folder_id subset it already
// understands.
type genericAddition struct {
	driver.RootPath
	driver.RootID
	raw string
}

func (g *genericAddition) UnmarshalJSON(data []byte) error {
	g.raw = string(data)
	type alias struct {
		driver.RootPath
		driver.RootID
	}
	var a alias
	_ = json.Unmarshal(data, &a) // best-effort root capture; plugin-specific fields ride in raw
	g.RootPath, g.RootID = a.RootPath, a.RootID
	return nil
}

// MarshalJSON round-trips the full original addition JSON (raw) rather than
// re-serializing the exported RootPath/RootID fields alone. Without this,
// json.Marshal falls back to the struct's default encoding, which only
// emits root_folder_path/root_folder_id — every plugin-specific field
// (client_id, client_secret, etc.) is unexported (raw) and silently
// dropped. op.saveDriverStorage (internal/op/storage.go) calls
// MarshalToString(driver.GetAddition()) on every initStorage — i.e. every
// LoadStorage/restart, not just on an admin edit — so without this method
// a plugin storage's credentials were being overwritten with a
// root-only JSON on every server restart. See
// TestGenericAdditionMarshalJSONRoundTrip.
func (g *genericAddition) MarshalJSON() ([]byte, error) {
	if g.raw != "" {
		return []byte(g.raw), nil
	}
	return []byte("{}"), nil
}

// pluginDriver adapts a gRPC plugin's ext.StorageDriver into a legacy
// internal/driver.Driver. One pluginDriver instance corresponds to one mounted
// storage; ProcManager multiplexes the underlying subprocess across every
// pluginDriver instance sharing the same pluginID.
type pluginDriver struct {
	model.Storage
	pluginID     string
	binPath      string
	caps         []ext.Capability
	pathAnchored bool
	pm           ProcManager

	add genericAddition
	cfg *ext.DriverConfig

	// mu guards client (and, incidentally, release): Init sets both once, but
	// a crash restart can replace client from any of the driver's own
	// concurrent op calls (List/Link/Get/Put/... on the same mounted storage
	// can and do run concurrently), so every read and write goes through
	// getClient/setClient rather than touching the field directly. This is
	// independent of ProcManager's own locking, which protects the
	// subprocess shared across sibling storages on the same pluginID; this
	// mutex protects this one pluginDriver's cached pointer to it.
	mu      sync.RWMutex
	client  ext.StorageDriver
	release func()
}

func (d *pluginDriver) getClient() ext.StorageDriver {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.client
}

func (d *pluginDriver) setClient(c ext.StorageDriver) {
	d.mu.Lock()
	d.client = c
	d.mu.Unlock()
}

// NewPluginDriver builds a driver.Driver backed by a gRPC plugin subprocess.
// caps is the plugin's declared Manifest.Capabilities (drives which optional
// legacy interfaces the returned driver behaves as if it implements).
// pathAnchored says whether the plugin's ObjRef.Anchor is a filesystem path
// (declares a root_folder_path config field) rather than the ext-contract
// default of an opaque driver-internal ID (root_folder_id): it decides which
// of ID/Path GetRoot's fallback branch populates, see GetRoot for why that
// matters. pm is the shared process manager (pluginproc.Manager in
// production).
func NewPluginDriver(pluginID, binPath string, caps []ext.Capability, pathAnchored bool, pm ProcManager) driver.Driver {
	return &pluginDriver{pluginID: pluginID, binPath: binPath, caps: caps, pathAnchored: pathAnchored, pm: pm}
}

var (
	_ driver.Driver    = (*pluginDriver)(nil)
	_ driver.GetRooter = (*pluginDriver)(nil)
	_ driver.Getter    = (*pluginDriver)(nil)
	_ driver.Mkdir     = (*pluginDriver)(nil)
	_ driver.Move      = (*pluginDriver)(nil)
	_ driver.Rename    = (*pluginDriver)(nil)
	_ driver.Copy      = (*pluginDriver)(nil)
	_ driver.Remove    = (*pluginDriver)(nil)
	_ driver.PutResult = (*pluginDriver)(nil)
	_ driver.PutRapid  = (*pluginDriver)(nil)
)

func (d *pluginDriver) storageID() string { return strconv.FormatUint(uint64(d.Storage.ID), 10) }

func (d *pluginDriver) Config() driver.Config {
	if d.cfg != nil {
		return extConfigToDriver(*d.cfg)
	}
	return driver.Config{Name: d.pluginID} // pre-Init fallback; real config cached at Init
}

func (d *pluginDriver) GetAddition() driver.Additional { return &d.add }

func (d *pluginDriver) Init(ctx context.Context) error {
	c, rel, err := d.pm.Acquire(ctx, d.pluginID, d.binPath)
	if err != nil {
		return err
	}
	d.setClient(c)
	d.release = rel
	cfg := c.DriverConfig()
	d.cfg = &cfg
	if err := c.NewInstance(ctx, d.storageID(), json.RawMessage(d.add.raw)); err != nil {
		d.release()
		d.setClient(nil)
		d.release = nil
		return translateExtErr(err)
	}
	return nil
}

// isCrash reports whether err looks like the plugin subprocess died,
// distinguishing that from an ordinary business error the plugin sent
// deliberately (a *ext.Error the plugin returned — e.g. ObjectNotFound) or a
// genuine transient RPC error unrelated to the process's liveness.
//
// A gRPC call error surfaces two shapes in this codebase (see grpc/host's
// Client methods): a business error is decoded from the reply and comes back
// as an *ext.Error (ext.CodeOf succeeds); everything else — including "the
// process died mid-call" — is a raw transport error from the grpc client
// call itself. isCrash only treats the latter as a possible crash, and only
// confirms it against the client's own Exited() signal (backed by
// go-plugin's process-reaper goroutine) rather than guessing from the error
// text. It polls briefly: Exited() is set by that background goroutine and
// may not have flipped yet at the exact instant the in-flight call's
// connection broke, even though the process is already gone.
func isCrash(err error, client ext.StorageDriver) bool {
	if err == nil {
		return false
	}
	if _, ok := ext.CodeOf(err); ok {
		return false // a business error the plugin sent deliberately, not a crash
	}
	ec, ok := client.(interface{ Exited() bool })
	if !ok {
		return false // client type doesn't expose crash detection (e.g. a test double)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if ec.Exited() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// restart replaces d's cached client with a freshly relaunched plugin
// subprocess (via pm, which collapses concurrent restarts — from this
// driver's own concurrent calls and from sibling storages sharing this
// pluginID alike — into one relaunch, see ProcManager.Restart) and
// re-registers this driver's own storage instance on it: a crash wipes every
// subprocess-side NewInstance registration, including the ones sibling
// storages hold — each self-heals independently the next time one of its own
// calls fails. stale must be the client the caller observed failing (from
// getClient() before invoking the operation that errored), matching pm's own
// compare-and-swap so a losing racer here still ends up with the winner's
// fresh client instead of relaunching a second time.
func (d *pluginDriver) restart(ctx context.Context, stale ext.StorageDriver) error {
	fresh, err := d.pm.Restart(ctx, d.pluginID, d.binPath, stale)
	if err != nil {
		return err
	}
	d.setClient(fresh)
	if err := fresh.NewInstance(ctx, d.storageID(), json.RawMessage(d.add.raw)); err != nil {
		return translateExtErr(err)
	}
	return nil
}

// withRestart calls fn against d's current client; if fn fails because the
// subprocess crashed (isCrash), it restarts once and retries fn exactly once
// more — a bounded single retry, not a loop, so a persistently broken plugin
// (e.g. its binary is gone) surfaces an error instead of hammering the OS
// with relaunch attempts. If the restart itself fails, the caller sees the
// *original* call's error (clearer: "the operation failed" rather than "the
// recovery attempt failed").
func withRestart[T any](ctx context.Context, d *pluginDriver, fn func(ext.StorageDriver) (T, error)) (T, error) {
	c := d.getClient()
	v, err := fn(c)
	if !isCrash(err, c) {
		return v, err
	}
	if rerr := d.restart(ctx, c); rerr != nil {
		return v, err
	}
	return fn(d.getClient())
}

// withRestartErr is withRestart for call sites that only return an error
// (Mkdir/Move/Rename/Copy/Remove all discard the wire Obj today).
func withRestartErr(ctx context.Context, d *pluginDriver, fn func(ext.StorageDriver) error) error {
	_, err := withRestart(ctx, d, func(c ext.StorageDriver) (struct{}, error) {
		return struct{}{}, fn(c)
	})
	return err
}

func (d *pluginDriver) Drop(ctx context.Context) error {
	c := d.getClient()
	if c == nil {
		return nil
	}
	err := c.DropInstance(ctx, d.storageID())
	d.release()
	d.setClient(nil)
	d.release = nil
	return translateExtErr(err)
}

func (d *pluginDriver) GetRoot(ctx context.Context) (model.Obj, error) {
	if _, ok := d.getClient().(ext.GetRooter); ok && hasCap(d.caps, ext.CapGetRooter) {
		o, err := withRestart(ctx, d, func(c ext.StorageDriver) (ext.Obj, error) {
			return c.(ext.GetRooter).GetRoot(ctx, d.storageID())
		})
		if err != nil {
			return nil, translateExtErr(err)
		}
		return extObjToModel(o), nil
	}
	// Set only the anchor field the plugin actually keys off of: an ID-anchored
	// plugin (the ext-contract default) never sees a Path in ObjRef.Anchor, so
	// the root object must carry ID only. Setting both — as a prior version of
	// this fallback did — makes op/fs.go's List "set path" step treat the root
	// as path-anchored: it stamps every child's Path as root+"/"+name (op.List
	// in internal/op/fs.go), and refAnchor below then prefers that stamped Path
	// over the child's real ID for every deeper List/Get/Link call. For an
	// ID-anchored driver (e.g. 123 Open's numeric fileID) that stamped string
	// ("0/subdir") is not a valid anchor and the driver rejects it (e.g.
	// strconv.ParseInt failure) — nested browsing 500s while the root listing,
	// which never goes through this join, works fine.
	//
	// The storage instance's own root_folder_id/root_folder_path (d.add, set
	// from the admin-configured addition JSON) takes priority over the
	// manifest's DefaultRoot: DefaultRoot is only a plugin-wide fallback for
	// storages that never configured a root at all. Without this, a user's
	// root_folder_id config (e.g. 123 Open's numeric fileID) was silently
	// ignored and every browse of the storage fell back to the plugin's
	// built-in default root.
	if d.pathAnchored {
		root := d.add.GetRootPath()
		if root == "" && d.cfg != nil {
			root = d.cfg.DefaultRoot
		}
		return &model.Object{Path: root, Name: "root", IsFolder: true}, nil
	}
	root := d.add.GetRootId()
	if root == "" && d.cfg != nil {
		root = d.cfg.DefaultRoot
	}
	return &model.Object{ID: root, Name: "root", IsFolder: true}, nil
}

// refAnchor recovers the ObjRef.Anchor a plugin expects from a legacy
// model.Obj: prefer Path (path-anchored drivers), fall back to ID
// (ID-anchored drivers), mirroring refToObj's inverse in ops.go.
func refAnchor(o model.Obj) string {
	if p := o.GetPath(); p != "" {
		return p
	}
	return o.GetID()
}

func (d *pluginDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	ref := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(dir)}
	objs, err := withRestart(ctx, d, func(c ext.StorageDriver) ([]ext.Obj, error) {
		return c.List(ctx, ref, ext.ListArgs{ReqPath: args.ReqPath, Refresh: args.Refresh})
	})
	if err != nil {
		return nil, translateExtErr(err)
	}
	out := make([]model.Obj, len(objs))
	for i := range objs {
		out[i] = extObjToModel(objs[i])
	}
	return out, nil
}

func (d *pluginDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	ref := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(file)}
	l, err := withRestart(ctx, d, func(c ext.StorageDriver) (*ext.Link, error) {
		return c.Link(ctx, ref, ext.LinkArgs{IP: args.IP, Header: args.Header, Type: args.Type, Redirect: args.Redirect})
	})
	if err != nil {
		return nil, translateExtErr(err)
	}
	switch l.ByteSource {
	case ext.ByteSourceURL:
		ml := &model.Link{URL: l.URL, Header: l.Header, Concurrency: l.Concurrency, PartSize: l.PartSize}
		if l.Expiration > 0 {
			exp := time.Duration(l.Expiration) * time.Second
			ml.Expiration = &exp
		}
		return ml, nil
	case ext.ByteSourceStream:
		// Stream byte source: demo plugins exercised so far (urlplugin) stay on
		// the URL branch; this path is exercised by the local-driver-backed
		// plugin fixture (internal/extension/grpc/testdata/localplugin) via the
		// 9.2 parity test and just needs to compile + behave correctly here —
		// wire it into the same OpenRange shape parity_test's readStreamLink
		// drives.
		ro, ok := d.getClient().(ext.RangeOpener)
		if !ok {
			return nil, errs.NotSupport
		}
		return newStreamLink(l, ref, ro), nil
	default:
		return nil, errs.NotSupport
	}
}

// newStreamLink wraps a ByteSourceStream ext.Link's OpenRange into a
// model.RangeReadCloserIF, so the legacy stream pipeline (op/fs.go, proxy
// handlers) can read it exactly like any other RangeReadCloser-backed Link.
func newStreamLink(l *ext.Link, ref ext.ObjRef, ro ext.RangeOpener) *model.Link {
	ml := &model.Link{
		Concurrency: l.Concurrency,
		PartSize:    l.PartSize,
		RangeReadCloser: &model.RangeReadCloser{
			RangeReader: func(ctx context.Context, rng http_range.Range) (io.ReadCloser, error) {
				return ro.OpenRange(ctx, ref, l.StreamID, ext.Range{Start: rng.Start, Length: rng.Length})
			},
		},
	}
	if l.Expiration > 0 {
		exp := time.Duration(l.Expiration) * time.Second
		ml.Expiration = &exp
	}
	return ml
}

// Get implements driver.Getter (path-addressed lookup).
func (d *pluginDriver) Get(ctx context.Context, p string) (model.Obj, error) {
	if _, ok := d.getClient().(ext.Getter); !ok {
		return nil, errs.NotImplement
	}
	o, err := withRestart(ctx, d, func(c ext.StorageDriver) (ext.Obj, error) {
		return c.(ext.Getter).Get(ctx, d.storageID(), p)
	})
	if err != nil {
		return nil, translateExtErr(err)
	}
	return extObjToModel(o), nil
}

func (d *pluginDriver) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	if _, ok := d.getClient().(ext.Mkdirer); !ok {
		return errs.NotImplement
	}
	ref := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(parentDir)}
	err := withRestartErr(ctx, d, func(c ext.StorageDriver) error {
		_, err := c.(ext.Mkdirer).Mkdir(ctx, ref, dirName)
		return err
	})
	return translateExtErr(err)
}

func (d *pluginDriver) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	if _, ok := d.getClient().(ext.Mover); !ok {
		return errs.NotImplement
	}
	src := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(srcObj)}
	dst := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(dstDir)}
	err := withRestartErr(ctx, d, func(c ext.StorageDriver) error {
		_, err := c.(ext.Mover).Move(ctx, src, dst)
		return err
	})
	return translateExtErr(err)
}

func (d *pluginDriver) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	if _, ok := d.getClient().(ext.Renamer); !ok {
		return errs.NotImplement
	}
	ref := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(srcObj)}
	err := withRestartErr(ctx, d, func(c ext.StorageDriver) error {
		_, err := c.(ext.Renamer).Rename(ctx, ref, newName)
		return err
	})
	return translateExtErr(err)
}

func (d *pluginDriver) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	if _, ok := d.getClient().(ext.Copier); !ok {
		return errs.NotImplement
	}
	src := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(srcObj)}
	dst := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(dstDir)}
	err := withRestartErr(ctx, d, func(c ext.StorageDriver) error {
		_, err := c.(ext.Copier).Copy(ctx, src, dst)
		return err
	})
	return translateExtErr(err)
}

// TryRapidUpload implements driver.PutRapid over the gRPC leg (SP-H). A
// plugin that lacks CapPutRapid answers NOT_IMPLEMENT which maps back to
// errs.NotImplement — op.PutRapid's caller treats any error as "fall back
// to streaming", so non-rapid plugins keep exactly their old behavior.
func (d *pluginDriver) TryRapidUpload(ctx context.Context, dstDir model.Obj, name string, size int64, hashes utils.HashInfo) (model.Obj, bool, error) {
	if _, ok := d.getClient().(ext.RapidPutter); !ok {
		return nil, false, errs.NotImplement
	}
	ref := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(dstDir)}
	// invariant: err == nil ⟹ hit was written by the LAST fn invocation
	// (withRestart calls fn at least once and returns that call's err).
	var hit bool
	o, err := withRestart(ctx, d, func(c ext.StorageDriver) (ext.Obj, error) {
		obj, h, e := c.(ext.RapidPutter).TryRapidUpload(ctx, ref, name, size, hashesToMap(hashes))
		hit = h
		return obj, e
	})
	if err != nil {
		return nil, false, translateExtErr(err)
	}
	if !hit {
		return nil, false, nil
	}
	if o.Name == "" && o.ID == "" {
		// A hit with no identifying fields at all (the wire-level counterpart
		// of ops.go's StorageBridge.TryRapidUpload hit&&obj==nil case): report
		// the hit but hand back no object, rather than extObjToModel's
		// all-zero-value *model.Object. op.PutRapid (internal/op/fs.go)
		// branches on obj==nil vs non-nil to decide addCacheObj vs
		// ClearCache — a non-nil ghost object here would get cached as if it
		// were a real (if empty) directory entry instead of correctly
		// invalidating the destination dir's cache.
		return nil, true, nil
	}
	return extObjToModel(o), true, nil
}

func (d *pluginDriver) Remove(ctx context.Context, obj model.Obj) error {
	if _, ok := d.getClient().(ext.Remover); !ok {
		return errs.NotImplement
	}
	ref := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(obj)}
	err := withRestartErr(ctx, d, func(c ext.StorageDriver) error {
		return c.(ext.Remover).Remove(ctx, ref)
	})
	return translateExtErr(err)
}

// progressReporter is the optional extra a gRPC host proxy (*grpc/host.Client)
// exposes beyond ext.Putter: a callback for the Progress messages the plugin
// reports over the Put bidi stream (9.2's put.go). It is declared locally
// (rather than imported from the host package) so this package does not
// depend on grpc/host — pluginDriver only needs the capability, and
// ProcManager.Acquire's return type is the frozen ext.StorageDriver.
type progressReporter interface {
	SetProgressHandler(func(float64))
}

// uploadStreamAdapter presents a legacy model.FileStreamer as the pull-model
// ext.UploadStream (9.0 §2) a gRPC plugin's Put reads from: the plugin issues
// ReadRequest calls at arbitrary offsets over the wire (two-pass reads —
// hash then upload, resumable upload), which requires random access the bare
// io.Reader half of FileStreamer cannot provide. newUploadStreamAdapter closes
// that gap the same way built-in drivers needing ReaderAt access already do
// (e.g. drivers/123_open's Put): stream.CacheFullInTempFileAndUpdateProgress
// materializes a model.File (a no-op when the stream is already backed by a
// local temp file — the common case for an HTTP-uploaded FileStream — a real
// buffered copy, progress-reported, otherwise).
type uploadStreamAdapter struct {
	info ext.UploadInfo
	file model.File
}

// newUploadStreamAdapter builds the adapter and materializes the random-access
// source. up receives 0-50 for this materialization phase; the caller wires
// the remaining 50-100 to the plugin's own Put-stream Progress reports (the
// actual network transfer), mirroring the 0-50/50-100 split 123_open's Put
// already establishes for the same two-phase (buffer, then upload) shape.
func newUploadStreamAdapter(fs model.FileStreamer, up driver.UpdateProgress) (*uploadStreamAdapter, error) {
	f, err := stream.CacheFullInTempFileAndUpdateProgress(fs, model.UpdateProgressWithRange(up, 0, 50))
	if err != nil {
		return nil, err
	}
	info := ext.UploadInfo{Name: fs.GetName(), Size: fs.GetSize(), Mimetype: fs.GetMimetype()}
	if exist := fs.GetExist(); exist != nil {
		e := objToExt(exist)
		info.Exist = &e
	}
	return &uploadStreamAdapter{info: info, file: f}, nil
}

func (u *uploadStreamAdapter) Info() ext.UploadInfo { return u.info }

func (u *uploadStreamAdapter) ReadAt(p []byte, off int64) (int, error) { return u.file.ReadAt(p, off) }

// Put implements driver.PutResult (op/fs.go's op.Put dispatches to whichever
// of Put/PutResult the driver implements — a single Go method can only carry
// one of the two signatures, and PutResult is chosen here because the ext
// wire protocol always returns an Obj, even a synthetic Name/Size-only one
// for plugins whose backing driver only implements the legacy plain
// driver.Put, see bridge/ops.go's forward-direction Put). Gating on whether
// the plugin actually declared CapPut happens inside the gRPC host proxy
// (grpc/host.Client.Put's c.has check), exactly like Mkdir/Move/Rename/Copy/
// Remove above — pluginDriver unconditionally satisfies driver.PutResult at
// the Go type level and lets the capability declaration decide at call time.
func (d *pluginDriver) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	if _, ok := d.getClient().(ext.Putter); !ok {
		return nil, errs.NotImplement
	}
	if up == nil {
		up = func(float64) {}
	}
	src, err := newUploadStreamAdapter(file, up)
	if err != nil {
		return nil, err
	}
	ref := ext.ObjRef{StorageID: d.storageID(), Anchor: refAnchor(dstDir)}
	remaining := model.UpdateProgressWithRange(up, 50, 100)
	// src is backed by uploadStreamAdapter.file — a materialized, random-access
	// model.File (see newUploadStreamAdapter) — so it can be read again from
	// scratch by a relaunched subprocess: a crash mid-upload is safe to retry
	// here exactly like every other op, no partial-upload bookkeeping needed.
	o, err := withRestart(ctx, d, func(c ext.StorageDriver) (ext.Obj, error) {
		// The gRPC host proxy is shared across every storage mounted on this
		// pluginID (pluginproc.Manager multiplexes one subprocess per plugin),
		// and SetProgressHandler sets a single field on that shared proxy
		// rather than a per-call callback — a known 9.2 seam (grpc/host/put.go),
		// not something this wiring redesigns. Best-effort wiring: register
		// for this attempt's duration (fresh on each attempt, including a
		// post-crash retry against a relaunched client) and clear on return,
		// which is correct for the common one-upload-at-a-time-per-plugin case
		// and merely cosmetically racy (progress momentarily attributed to the
		// wrong upload) if two Put calls on the same plugin process overlap;
		// ReadRequest/Done routing is unaffected since those travel each
		// call's own bidi stream.
		if pr, ok := c.(progressReporter); ok {
			pr.SetProgressHandler(func(fraction float64) { remaining(fraction * 100) })
			defer pr.SetProgressHandler(nil)
		}
		return c.(ext.Putter).Put(ctx, ref, src)
	})
	if err != nil {
		return nil, translateExtErr(err)
	}
	return extObjToModel(o), nil
}
