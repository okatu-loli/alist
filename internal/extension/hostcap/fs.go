package hostcap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// validateSingleSegment rejects a name that is not a single, non-traversing path
// segment. Mkdir/Rename take a NAME, not a path: the caller authorized a parent
// (Mkdir) or a source (Rename), and the created/renamed object must stay inside
// that authorized location. Without this a name like "../escape" makes the
// post-Clean target leave the authorized subtree (or, for a Local-backed
// storage, leave the storage root on disk) while only the parent/source was ever
// checked — the VULN 1 / VULN 2 traversal the F0 red team demonstrated. Reject
// empty, ".", "..", and any name carrying a path separator.
func validateSingleSegment(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return ext.ErrPermissionDenied
	}
	return nil
}

// hostFS is the host filesystem borrowed by one plugin. Invariant 1: every
// method resolves the acting principal, runs op.Authorize against it for the
// requested path+action, and only then touches storage via the fs/op layer.
// Borrowing FS is never a way around authorization.
//
// ObjRef semantics for a borrowed FS: the Anchor carries the object's virtual
// (mount) path. The plugin only ever receives refs it got from Resolve/List
// (it does not fabricate them), and every op re-runs Authorize, so a forged
// Anchor is still gated — Authorize is the backstop, the ObjRef is a
// convenience. (Storage-driver ObjRefs, by contrast, anchor on a driver-internal
// id; the two ref worlds never mix because a borrowed FS ref has an empty
// StorageID.)
type hostFS struct {
	grant Grant
	prin  *principals
	bound *model.User
	// delegatedOnly refuses the empty (self/bound) principal: an unauthenticated
	// public handler may act only through an explicitly delegated principal (see
	// Options.DelegatedOnly).
	delegatedOnly bool
}

func newHostFS(grant Grant, prin *principals, delegatedOnly bool) *hostFS {
	return &hostFS{grant: grant, prin: prin, bound: boundPrincipal(grant.PluginID, grant.FS), delegatedOnly: delegatedOnly}
}

// principal resolves an ext.Principal handle to the *model.User to authorize as.
// The empty handle means the plugin's own bound (restricted) principal — except
// in a delegated-only (public) Host, where the self principal is refused so a
// public handler cannot borrow the plugin's own fs grant.
func (f *hostFS) principal(p ext.Principal) (*model.User, error) {
	if p == "" {
		if f.delegatedOnly {
			return nil, ext.ErrPermissionDenied
		}
		return f.bound, nil
	}
	if u := f.prin.lookup(string(p)); u != nil {
		return u, nil
	}
	return nil, ext.ErrPermissionDenied
}

// authorize is the single choke point: check the capability grant, resolve the
// principal, and run core Authorize. A denial always surfaces as
// ext.ErrPermissionDenied so the boundary maps it uniformly.
func (f *hostFS) authorize(ctx context.Context, p ext.Principal, act model.Action, vpath string) (*model.User, string, error) {
	if !f.grant.HasAPI(CapFS) {
		return nil, "", ext.ErrPermissionDenied
	}
	user, err := f.principal(p)
	if err != nil {
		return nil, "", err
	}
	vpath = utils.FixAndCleanPath(vpath)
	if err := op.Authorize(ctx, user, act, op.AuthzRequest{Path: vpath}); err != nil {
		return nil, "", ext.ErrPermissionDenied
	}
	return user, vpath, nil
}

// PreAuthorizePut runs the SAME write authorization Put runs (capability grant +
// principal resolution + core Authorize WRITE on the destination dir) WITHOUT
// touching any bytes. The reverse-bridge FSPut receiver calls it on the Init
// frame so an ungranted or unauthorized-destination upload is refused before a
// single chunk is spooled to host temp disk (HIGH-2: the spool was a pre-auth
// DoS). Put still re-runs the full check, so this is a cheap early gate, not the
// authoritative one.
func (f *hostFS) PreAuthorizePut(ctx context.Context, p ext.Principal, dstDir ext.ObjRef) error {
	_, _, err := f.authorize(ctx, p, model.ActWrite, refPath(dstDir))
	return err
}

func (f *hostFS) Resolve(ctx context.Context, p ext.Principal, p2 string) (ext.ObjRef, error) {
	_, vpath, err := f.authorize(ctx, p, model.ActRead, p2)
	if err != nil {
		return ext.ObjRef{}, err
	}
	return ext.ObjRef{Anchor: vpath}, nil
}

func (f *hostFS) List(ctx context.Context, p ext.Principal, dir ext.ObjRef, a ext.ListArgs) ([]ext.Obj, error) {
	user, vpath, err := f.authorize(ctx, p, model.ActRead, refPath(dir))
	if err != nil {
		return nil, err
	}
	objs, err := fs.List(ctx, vpath, &fs.ListArgs{Refresh: a.Refresh, NoLog: true})
	if err != nil {
		return nil, mapFSErr(err)
	}
	// Strip children the principal may not see/read (per-child deny + hide),
	// exactly as the browse API does — a borrowed listing is not a way to
	// enumerate objects the principal is denied.
	objs = op.FilterListing(ctx, user, vpath, objs)
	return modelObjsToExt(objs), nil
}

func (f *hostFS) Get(ctx context.Context, p ext.Principal, ref ext.ObjRef) (ext.Obj, error) {
	_, vpath, err := f.authorize(ctx, p, model.ActRead, refPath(ref))
	if err != nil {
		return ext.Obj{}, err
	}
	o, err := fs.Get(ctx, vpath, &fs.GetArgs{NoLog: true})
	if err != nil {
		return ext.Obj{}, mapFSErr(err)
	}
	return modelObjToExt(o), nil
}

func (f *hostFS) Link(ctx context.Context, p ext.Principal, ref ext.ObjRef, a ext.LinkArgs) (*ext.Link, error) {
	_, vpath, err := f.authorize(ctx, p, model.ActRead, refPath(ref))
	if err != nil {
		return nil, err
	}
	l, _, err := fs.Link(ctx, vpath, model.LinkArgs{IP: a.IP, Header: a.Header, Type: a.Type, Redirect: a.Redirect})
	if err != nil {
		return nil, mapFSErr(err)
	}
	// Only the URL byte source survives the borrowed-FS boundary as-is; the
	// live stream/handle forms of model.Link are not serializable and are a
	// later seam (protocol plugins that need byte streaming use host.FS on the
	// native path in the meantime).
	el := &ext.Link{ByteSource: ext.ByteSourceURL, URL: l.URL, Header: l.Header, Concurrency: l.Concurrency, PartSize: l.PartSize}
	if l.Expiration != nil {
		el.Expiration = int64(l.Expiration.Seconds())
	}
	return el, nil
}

func (f *hostFS) Mkdir(ctx context.Context, p ext.Principal, parent ext.ObjRef, name string) (ext.Obj, error) {
	// name must be a single segment: otherwise "../x" escapes the authorized
	// parent after the Clean below (VULN 1).
	if err := validateSingleSegment(name); err != nil {
		return ext.Obj{}, err
	}
	// Authorize write on the PARENT: the plugin must be permitted to create
	// under the parent dir.
	_, parentPath, err := f.authorize(ctx, p, model.ActWrite, refPath(parent))
	if err != nil {
		return ext.Obj{}, err
	}
	full := utils.FixAndCleanPath(path.Join(parentPath, name))
	if err := fs.MakeDir(ctx, full); err != nil {
		return ext.Obj{}, mapFSErr(err)
	}
	o, err := fs.Get(ctx, full, &fs.GetArgs{NoLog: true})
	if err != nil {
		// The dir was created; a follow-up Get failure should not mask success.
		return ext.Obj{Name: name, Path: full, IsDir: true}, nil
	}
	return modelObjToExt(o), nil
}

func (f *hostFS) Move(ctx context.Context, p ext.Principal, src, dstDir ext.ObjRef) error {
	user, srcPath, err := f.authorize(ctx, p, model.ActMove, refPath(src))
	if err != nil {
		return err
	}
	dstPath := utils.FixAndCleanPath(refPath(dstDir))
	// The destination must also be writable by the same principal.
	if err := op.Authorize(ctx, user, model.ActWrite, op.AuthzRequest{Path: dstPath}); err != nil {
		return ext.ErrPermissionDenied
	}
	return mapFSErr(fs.Move(ctx, srcPath, dstPath))
}

func (f *hostFS) Rename(ctx context.Context, p ext.Principal, src ext.ObjRef, name string) error {
	// A rename changes the last segment only; a name with a separator or ".."
	// would let the Local driver os.Rename the object to an arbitrary on-disk
	// location, escaping the grant with no destination authorization (VULN 2).
	if err := validateSingleSegment(name); err != nil {
		return err
	}
	_, srcPath, err := f.authorize(ctx, p, model.ActRename, refPath(src))
	if err != nil {
		return err
	}
	return mapFSErr(fs.Rename(ctx, srcPath, name))
}

func (f *hostFS) Copy(ctx context.Context, p ext.Principal, src, dstDir ext.ObjRef) error {
	user, srcPath, err := f.authorize(ctx, p, model.ActCopy, refPath(src))
	if err != nil {
		return err
	}
	dstPath := utils.FixAndCleanPath(refPath(dstDir))
	if err := op.Authorize(ctx, user, model.ActWrite, op.AuthzRequest{Path: dstPath}); err != nil {
		return ext.ErrPermissionDenied
	}
	if _, err := fs.Copy(ctx, srcPath, dstPath, false); err != nil {
		return mapFSErr(err)
	}
	return nil
}

func (f *hostFS) Remove(ctx context.Context, p ext.Principal, ref ext.ObjRef) error {
	_, vpath, err := f.authorize(ctx, p, model.ActRemove, refPath(ref))
	if err != nil {
		return err
	}
	return mapFSErr(fs.Remove(ctx, vpath))
}

// Put writes an incoming plugin-held upload into the host filesystem. It
// authorizes write on the DESTINATION DIR first (same choke point Mkdir uses),
// then rejects a non-single-segment name (the same VULN 1 guard: info.Name is
// a leaf filename, not a path, and must not be able to escape the authorized
// directory). The bytes themselves flow through fs.PutDirectly exactly as a v3
// HTTP upload would (server/handles.FsPut), via the same uploadStreamReader
// adapter internal/extension/bridge uses for the forward (driver-side) Put.
func (f *hostFS) Put(ctx context.Context, p ext.Principal, dstDir ext.ObjRef, up ext.UploadStream) (ext.Obj, error) {
	_, vpath, err := f.authorize(ctx, p, model.ActWrite, refPath(dstDir))
	if err != nil {
		return ext.Obj{}, err
	}
	info := up.Info()
	if err := validateSingleSegment(info.Name); err != nil {
		return ext.Obj{}, err
	}
	fileStream := &stream.FileStream{
		Ctx: ctx,
		Obj: &model.Object{
			Name:     info.Name,
			Size:     info.Size,
			Modified: time.Now(),
			HashInfo: hashInfoFromMap(info.Hashes),
		},
		Reader:   &uploadStreamReader{up: up, size: info.Size},
		Mimetype: info.Mimetype,
	}
	defer fileStream.Close()
	if info.Exist != nil {
		fileStream.SetExist(&model.Object{Name: info.Exist.Name, Size: info.Exist.Size})
	}
	if err := fs.PutDirectly(ctx, vpath, fileStream, true); err != nil {
		return ext.Obj{}, mapFSErr(err)
	}
	full := utils.FixAndCleanPath(path.Join(vpath, info.Name))
	o, err := fs.Get(ctx, full, &fs.GetArgs{NoLog: true})
	if err != nil {
		// The write succeeded; a follow-up Get failure should not mask that.
		return ext.Obj{Name: info.Name, Size: info.Size}, nil
	}
	return modelObjToExt(o), nil
}

// uploadStreamReader adapts the pull-model ext.UploadStream (ReadAt) into the
// sequential io.Reader fs.PutDirectly expects — the same shape
// internal/extension/bridge.uploadStreamReader gives the forward (driver-side)
// Put bridge.
type uploadStreamReader struct {
	up   ext.UploadStream
	off  int64
	size int64
}

func (r *uploadStreamReader) Read(p []byte) (int, error) {
	if r.size >= 0 && r.off >= r.size {
		return 0, io.EOF
	}
	n, err := r.up.ReadAt(p, r.off)
	r.off += int64(n)
	return n, err
}

// hashInfoFromMap converts the wire's algorithm-name -> hex-digest map into a
// utils.HashInfo, reusing utils.FromString's name lookup (utils.name2hash is
// unexported, so a JSON round-trip is the supported way in).
func hashInfoFromMap(h map[string]string) utils.HashInfo {
	if len(h) == 0 {
		return utils.HashInfo{}
	}
	b, err := json.Marshal(h)
	if err != nil {
		return utils.HashInfo{}
	}
	return utils.FromString(string(b))
}

// refPath extracts the virtual path an ObjRef anchors, defaulting to root.
func refPath(ref ext.ObjRef) string {
	if ref.Anchor == "" {
		return "/"
	}
	return ref.Anchor
}

// mapFSErr normalizes core fs/op errors into ext error codes so the boundary can
// reconstruct the right sentinel. A permission denial from deeper in fs/op (not
// just the pre-check) is surfaced as ErrPermissionDenied too.
func mapFSErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errs.IsObjectNotFound(err):
		return ext.NewError(ext.ErrCodeObjectNotFound, err.Error())
	case errors.Is(err, errs.PermissionDenied):
		return ext.ErrPermissionDenied
	case errs.IsNotImplement(err):
		return ext.NewError(ext.ErrCodeNotImplement, err.Error())
	case errs.IsNotSupportError(err):
		return ext.NewError(ext.ErrCodeNotSupport, err.Error())
	default:
		return err
	}
}

func modelObjToExt(o model.Obj) ext.Obj {
	return ext.Obj{
		Name:     o.GetName(),
		Size:     o.GetSize(),
		Modified: o.ModTime().UnixMilli(),
		Created:  o.CreateTime().UnixMilli(),
		IsDir:    o.IsDir(),
		ID:       o.GetID(),
		Path:     o.GetPath(),
	}
}

func modelObjsToExt(objs []model.Obj) []ext.Obj {
	out := make([]ext.Obj, 0, len(objs))
	for _, o := range objs {
		out = append(out, modelObjToExt(o))
	}
	return out
}
