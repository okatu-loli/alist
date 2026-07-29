// Package webdav implements AList's internal WebDAV server (design W5): a
// golang.org/x/net/webdav.FileSystem adapter over the existing op/fs layer,
// so a WebDAV client gets exactly the same RBAC5 authorization a browser
// user does. op.Authorize is the ONLY access-control boundary in this
// package -- every method below resolves the caller's *model.User from the
// request context, authorizes the specific action against the specific
// path, and only then delegates to internal/fs (which itself resolves
// storage+actualPath and calls the op layer, same as server/handles does for
// the browser fs.* endpoints).
package webdav

import (
	"context"
	"errors"
	"io"
	"os"
	stdpath "path"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/http_range"
	"github.com/alist-org/alist/v3/pkg/utils"
	xwebdav "golang.org/x/net/webdav"
)

// maxWebdavPutBytes bounds a single PUT body -- the abuse guard the design
// calls for (spec §②: "有字节上限(防滥用,复用配置或常量)"). It mirrors
// offline_download's maxOfflineDownloadBytes ceiling: generous enough to
// never truncate a legitimate upload, but enough to stop an adversarial or
// runaway client body from filling the spool disk indefinitely.
//
// A var, not a const: webdav_test.go's setMaxPutBytesForTest temporarily
// lowers it so the oversize-PUT-abort path (writeFile.aborted, see below)
// can be exercised without actually spooling a multi-gigabyte body. Nothing
// else in this package ever assigns it at runtime.
var maxWebdavPutBytes int64 = 100 << 30 // 100 GiB

// errWebdavPutTooLarge is returned (as a Write error, surfaced by the
// stdlib webdav.Handler as a mid-PUT failure) once a PUT body exceeds
// maxWebdavPutBytes.
var errWebdavPutTooLarge = errors.New("webdav: PUT body exceeds the maximum allowed size")

var (
	errNotSeekable    = errors.New("webdav: not seekable")
	errNotADirectory  = errors.New("webdav: not a directory")
	errReadOnlyHandle = errors.New("webdav: read-only handle")
	errWriteOnlyPut   = errors.New("webdav: write-only PUT handle")
)

// userFromCtx resolves the *model.User the Basic Auth middleware (auth.go)
// stored on the request context under ctxkey.User. A nil return means the
// FileSystem was reached without authentication ever running -- it must
// never be trusted to mean "anonymous", the caller always denies it.
func userFromCtx(ctx context.Context) *model.User {
	u, _ := ctx.Value(ctxkey.User).(*model.User)
	return u
}

// joinPath mirrors the JoinPath call every server/handles fs endpoint makes
// before touching op: it's the single normalization point for a raw WebDAV
// path (already stripped of the "/dav" prefix by xwebdav.Handler) into the
// full virtual path op.Authorize and internal/fs expect. model.User.JoinPath
// never actually errors today (it's utils.FixAndCleanPath), but the call is
// kept so this package tracks the real convention if that ever changes.
func joinPath(user *model.User, name string) string {
	p, _ := user.JoinPath(name)
	return p
}

// denyErr marks ctx as having hit an authorization denial (read by
// deniedInterceptWriter in handler.go to force the final HTTP status to 403,
// overriding whatever the stock xwebdav.Handler would otherwise have picked
// for that FileSystem error) and returns the sentinel FileSystem error.
func denyErr(ctx context.Context) error {
	markDenied(ctx)
	return os.ErrPermission
}

// mapOpErr translates an op/fs-layer error into the os.Err* sentinels
// xwebdav.Handler understands (os.IsNotExist gates its 404/409 branches).
// Anything unrecognized passes through unchanged, which the stock handler
// maps to its generic "something went wrong" status per method (404/405).
func mapOpErr(err error) error {
	if err == nil {
		return nil
	}
	if errs.IsObjectNotFound(err) || errs.IsNotFoundError(err) {
		return os.ErrNotExist
	}
	return err
}

// fileInfo is a plain os.FileInfo backing both a resolved model.Obj (Stat,
// Readdir) and an in-progress PUT (writeFile.Stat, before the content is
// actually committed via op.Put in Close).
type fileInfo struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func newFileInfo(obj model.Obj) fileInfo {
	return fileInfo{
		name:    obj.GetName(),
		size:    obj.GetSize(),
		modTime: obj.ModTime(),
		isDir:   obj.IsDir(),
	}
}

func (fi fileInfo) Name() string       { return fi.name }
func (fi fileInfo) Size() int64        { return fi.size }
func (fi fileInfo) ModTime() time.Time { return fi.modTime }
func (fi fileInfo) IsDir() bool        { return fi.isDir }
func (fi fileInfo) Sys() any           { return nil }
func (fi fileInfo) Mode() os.FileMode {
	if fi.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}

// alistFS implements xwebdav.FileSystem over internal/fs + internal/op. It
// is stateless: every method call resolves its own user + path from the ctx
// and args it is handed, so a single package-level instance (see handler.go)
// serves every request concurrently.
type alistFS struct{}

var _ xwebdav.FileSystem = alistFS{}

func (alistFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	user := userFromCtx(ctx)
	if user == nil {
		return nil, denyErr(ctx)
	}
	reqPath := joinPath(user, name)
	if err := op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: reqPath}); err != nil {
		return nil, denyErr(ctx)
	}
	obj, err := fs.Get(ctx, reqPath, &fs.GetArgs{NoLog: true})
	if err != nil {
		return nil, mapOpErr(err)
	}
	return newFileInfo(obj), nil
}

func (fsys alistFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (xwebdav.File, error) {
	user := userFromCtx(ctx)
	if user == nil {
		return nil, denyErr(ctx)
	}
	reqPath := joinPath(user, name)
	if flag&os.O_CREATE != 0 {
		return openForWrite(ctx, user, reqPath)
	}
	return openForRead(ctx, user, reqPath)
}

func openForRead(ctx context.Context, user *model.User, reqPath string) (xwebdav.File, error) {
	if err := op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: reqPath}); err != nil {
		return nil, denyErr(ctx)
	}
	obj, err := fs.Get(ctx, reqPath, &fs.GetArgs{NoLog: true})
	if err != nil {
		return nil, mapOpErr(err)
	}
	if obj.IsDir() {
		return &dirFile{ctx: ctx, path: reqPath, info: newFileInfo(obj)}, nil
	}
	return &readFile{ctx: ctx, path: reqPath, info: newFileInfo(obj)}, nil
}

// openForWrite backs a PUT: it authorizes ActWrite on the destination
// directory (the same boundary server/middlewares.FsUp checks before
// handles.FsStream), then spools the request body to a bounded temp file.
// The actual op.Put only happens in writeFile.Close, once xwebdav.Handler
// has finished copying the whole body in -- see writeFile's doc comment for
// why (Stat must report a size before Close is called).
func openForWrite(ctx context.Context, user *model.User, reqPath string) (xwebdav.File, error) {
	dir, name := stdpath.Split(reqPath)
	if name == "" {
		// PUT to a bare directory path ("/dav/foo/") has no file name.
		return nil, os.ErrInvalid
	}
	if err := op.Authorize(ctx, user, model.ActWrite, op.AuthzRequest{Path: dir}); err != nil {
		return nil, denyErr(ctx)
	}
	tmp, err := os.CreateTemp(conf.Conf.TempDir, "webdav-put-*")
	if err != nil {
		return nil, err
	}
	return &writeFile{ctx: ctx, dir: dir, name: name, tmp: tmp}, nil
}

func (alistFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	user := userFromCtx(ctx)
	if user == nil {
		return denyErr(ctx)
	}
	reqPath := joinPath(user, name)
	if err := op.Authorize(ctx, user, model.ActWrite, op.AuthzRequest{Path: reqPath}); err != nil {
		return denyErr(ctx)
	}
	return mapOpErr(fs.MakeDir(ctx, reqPath))
}

func (alistFS) RemoveAll(ctx context.Context, name string) error {
	user := userFromCtx(ctx)
	if user == nil {
		return denyErr(ctx)
	}
	reqPath := joinPath(user, name)
	if err := op.Authorize(ctx, user, model.ActRemove, op.AuthzRequest{Path: reqPath}); err != nil {
		return denyErr(ctx)
	}
	return mapOpErr(fs.Remove(ctx, reqPath))
}

// Rename backs both WebDAV MOVE cases: a same-directory MOVE is a pure
// op.Rename (ActRename); a cross-directory MOVE is op.Move (ActMove on the
// source + ActWrite on the destination directory, mirroring FsMove), with a
// trailing op.Rename when the destination also carries a new base name (a
// client asking to move-and-rename in one MOVE request).
func (alistFS) Rename(ctx context.Context, oldName, newName string) error {
	user := userFromCtx(ctx)
	if user == nil {
		return denyErr(ctx)
	}
	oldPath := joinPath(user, oldName)
	newPath := joinPath(user, newName)
	if oldPath == newPath {
		return nil
	}
	oldDir, oldBase := stdpath.Split(oldPath)
	newDir, newBase := stdpath.Split(newPath)

	if oldDir == newDir {
		if err := op.Authorize(ctx, user, model.ActRename, op.AuthzRequest{Path: oldPath}); err != nil {
			return denyErr(ctx)
		}
		return mapOpErr(fs.Rename(ctx, oldPath, newBase))
	}

	if err := op.Authorize(ctx, user, model.ActMove, op.AuthzRequest{Path: oldPath}); err != nil {
		return denyErr(ctx)
	}
	if err := op.Authorize(ctx, user, model.ActWrite, op.AuthzRequest{Path: newDir}); err != nil {
		return denyErr(ctx)
	}
	if err := fs.Move(ctx, oldPath, newDir); err != nil {
		return mapOpErr(err)
	}
	if oldBase == newBase {
		return nil
	}
	// The move landed the object at newDir/oldBase (op.Move never renames);
	// finish the rename half of a combined move+rename MOVE request.
	movedPath := stdpath.Join(newDir, oldBase)
	if err := op.Authorize(ctx, user, model.ActRename, op.AuthzRequest{Path: movedPath}); err != nil {
		return denyErr(ctx)
	}
	return mapOpErr(fs.Rename(ctx, movedPath, newBase))
}

// dirFile is the xwebdav.File returned for a directory OpenFile (PROPFIND /
// walkFS calls this, then Readdir(0), per golang.org/x/net/webdav's own
// walkFS implementation). Its listing is authorized once (ActRead on path,
// done by openForRead before this is constructed) and then per-child
// filtered by op.FilterListing inside Readdir itself (internal/fs.List
// returns objs raw -- FilterListing is the sole per-child hide/deny
// enforcement point, same as server/handles/fsread.go and
// internal/extension/hostcap/fs.go): a caller who cannot read a given child
// simply never sees it, rather than getting an error (fail-closed-by-
// omission, same as the browser fs/list endpoint).
type dirFile struct {
	ctx     context.Context
	path    string
	info    fileInfo
	entries []os.FileInfo
	loaded  bool
	pos     int
}

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if !d.loaded {
		objs, err := fs.List(d.ctx, d.path, &fs.ListArgs{})
		if err != nil {
			return nil, mapOpErr(err)
		}
		// internal/fs.List returns objs raw -- op.FilterListing is the sole
		// per-child hide/deny enforcement point (design defect #10 fix), and
		// every other listing entry point (browser fs/list, fsbatch, search,
		// hostcap.List) already runs its result through it. Without this
		// call, PROPFIND/Readdir would leak the name+size+modtime of a child
		// the caller is denied ActRead on, or that hide patterns should have
		// hidden from a user without ActSeeHides.
		objs = op.FilterListing(d.ctx, userFromCtx(d.ctx), d.path, objs)
		d.entries = make([]os.FileInfo, len(objs))
		for i, o := range objs {
			d.entries[i] = newFileInfo(o)
		}
		d.loaded = true
	}
	if count <= 0 {
		out := d.entries[d.pos:]
		d.pos = len(d.entries)
		return out, nil
	}
	if d.pos >= len(d.entries) {
		return nil, io.EOF
	}
	end := d.pos + count
	if end > len(d.entries) {
		end = len(d.entries)
	}
	out := d.entries[d.pos:end]
	d.pos = end
	return out, nil
}

func (d *dirFile) Stat() (os.FileInfo, error)         { return d.info, nil }
func (d *dirFile) Read(_ []byte) (int, error)         { return 0, io.EOF }
func (d *dirFile) Seek(_ int64, _ int) (int64, error) { return 0, errNotSeekable }
func (d *dirFile) Write(_ []byte) (int, error)        { return 0, errReadOnlyHandle }
func (d *dirFile) Close() error                       { return nil }

// readFile is the xwebdav.File returned for a file GET/HEAD. Content is
// opened lazily (on the first Read/Seek) via op.Link, so a plain PROPFIND
// Stat() (which reuses OpenFile+Stat in some walkFS paths) never touches the
// storage backend for bytes it will never read.
type readFile struct {
	ctx    context.Context
	path   string
	info   fileInfo
	rs     io.ReadSeeker
	closer io.Closer
}

func (f *readFile) open() error {
	if f.rs != nil {
		return nil
	}
	link, obj, err := fs.Link(f.ctx, f.path, model.LinkArgs{})
	if err != nil {
		return mapOpErr(err)
	}
	rs, closer, err := newLinkReadSeeker(f.ctx, link, obj.GetSize())
	if err != nil {
		return err
	}
	f.rs, f.closer = rs, closer
	return nil
}

func (f *readFile) Read(p []byte) (int, error) {
	if err := f.open(); err != nil {
		return 0, err
	}
	return f.rs.Read(p)
}

func (f *readFile) Seek(offset int64, whence int) (int64, error) {
	if err := f.open(); err != nil {
		return 0, err
	}
	return f.rs.Seek(offset, whence)
}

func (f *readFile) Stat() (os.FileInfo, error)         { return f.info, nil }
func (f *readFile) Readdir(int) ([]os.FileInfo, error) { return nil, errNotADirectory }
func (f *readFile) Write(_ []byte) (int, error)        { return 0, errReadOnlyHandle }
func (f *readFile) Close() error {
	if f.closer != nil {
		return f.closer.Close()
	}
	return nil
}

// newLinkReadSeeker adapts a resolved model.Link into an io.ReadSeeker+Close
// pair suitable for http.ServeContent (which xwebdav.Handler's GET path
// calls directly). MFile (local/smb-like drivers) already satisfies
// io.Reader+io.Seeker+io.Closer and is returned as-is; a URL-backed link
// (remote drivers) goes through stream.GetRangeReadCloserFromLink -- the
// same helper server/common.ProxyRange uses -- wrapped by linkSeeker below to
// present it as a seekable stream instead of a one-shot RangeReadCloserIF.
func newLinkReadSeeker(ctx context.Context, link *model.Link, size int64) (io.ReadSeeker, io.Closer, error) {
	if link.MFile != nil {
		return link.MFile, link.MFile, nil
	}
	rrc := link.RangeReadCloser
	if rrc == nil {
		if link.URL == "" {
			return nil, nil, errors.New("webdav: link has neither a local file nor a URL to read")
		}
		var err error
		rrc, err = stream.GetRangeReadCloserFromLink(size, link)
		if err != nil {
			return nil, nil, err
		}
	}
	ls := &linkSeeker{ctx: ctx, rrc: rrc, size: size}
	return ls, ls, nil
}

// linkSeeker turns a model.RangeReadCloserIF (one-shot "give me bytes
// [start,start+length)") into an io.ReadSeeker: Seek just records the new
// offset and drops any open range read; the next Read lazily reopens a range
// starting at that offset. This is what lets http.ServeContent -- which
// Seeks to satisfy a Range request -- serve a WebDAV ranged GET against a
// remote driver's URL-based link.
type linkSeeker struct {
	ctx    context.Context
	rrc    model.RangeReadCloserIF
	size   int64
	offset int64
	cur    io.ReadCloser
}

func (l *linkSeeker) Read(p []byte) (int, error) {
	if l.cur == nil {
		rc, err := l.rrc.RangeRead(l.ctx, http_range.Range{Start: l.offset, Length: -1})
		if err != nil {
			return 0, err
		}
		l.cur = rc
	}
	n, err := l.cur.Read(p)
	l.offset += int64(n)
	return n, err
}

func (l *linkSeeker) Seek(offset int64, whence int) (int64, error) {
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = l.offset + offset
	case io.SeekEnd:
		newOffset = l.size + offset
	default:
		return 0, errors.New("webdav: invalid whence")
	}
	if newOffset < 0 {
		return 0, errors.New("webdav: negative seek position")
	}
	if newOffset != l.offset && l.cur != nil {
		_ = l.cur.Close()
		l.cur = nil
	}
	l.offset = newOffset
	return l.offset, nil
}

func (l *linkSeeker) Close() error {
	if l.cur != nil {
		return l.cur.Close()
	}
	return nil
}

// writeFile is the xwebdav.File returned for a PUT. xwebdav.Handler's
// handlePut does, in order: io.Copy(f, body); f.Stat(); f.Close() -- Stat is
// called BEFORE Close, so the final size must already be knowable at Stat
// time even though the content has only been spooled to a local temp file,
// not yet committed via op.Put. Close is therefore where the actual write
// through internal/fs (and so op.Put's own Authorize-free but
// already-authorized-by-openForWrite storage write) happens.
type writeFile struct {
	ctx     context.Context
	dir     string
	name    string
	tmp     *os.File
	written int64
	closed  bool
	// aborted is set by Write once the PUT body has either exceeded
	// maxWebdavPutBytes or hit a real I/O error spooling to the temp file.
	// Close checks it and refuses to commit the (now known-truncated) spool
	// via fs.PutDirectly -- without this, x/net/webdav's vendored handlePut
	// (io.Copy(f, body); f.Stat(); f.Close(), in that order, unconditionally)
	// would still call Close and silently commit a truncated object, since it
	// discards io.Copy's error before ever reaching Close.
	aborted  bool
	abortErr error
}

func (w *writeFile) Write(p []byte) (int, error) {
	if w.written+int64(len(p)) > maxWebdavPutBytes {
		w.aborted = true
		w.abortErr = errWebdavPutTooLarge
		return 0, errWebdavPutTooLarge
	}
	n, err := w.tmp.Write(p)
	w.written += int64(n)
	if err != nil {
		w.aborted = true
		w.abortErr = err
	}
	return n, err
}

func (w *writeFile) Stat() (os.FileInfo, error) {
	return fileInfo{name: w.name, size: w.written, modTime: time.Now()}, nil
}

func (w *writeFile) Read(_ []byte) (int, error)         { return 0, errWriteOnlyPut }
func (w *writeFile) Seek(_ int64, _ int) (int64, error) { return 0, errNotSeekable }
func (w *writeFile) Readdir(int) ([]os.FileInfo, error) { return nil, errNotADirectory }

func (w *writeFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.aborted {
		// The spool is known-truncated (oversize ceiling hit, or a real
		// write error) -- clean it up and refuse to commit, rather than
		// letting x/net/webdav's handlePut (which calls Close unconditionally
		// after io.Copy, even on a copy error it then discards) turn a
		// partial body into a permanent, silently-truncated object.
		_ = w.tmp.Close()
		_ = os.Remove(w.tmp.Name())
		return w.abortErr
	}
	if _, err := w.tmp.Seek(0, io.SeekStart); err != nil {
		_ = os.Remove(w.tmp.Name())
		return err
	}
	fstream := &stream.FileStream{
		Ctx: w.ctx,
		Obj: &model.Object{
			Name:     w.name,
			Size:     w.written,
			Modified: time.Now(),
		},
		Mimetype: utils.GetMimeType(w.name),
	}
	fstream.SetTmpFile(w.tmp)
	// putDirectly's defer file.Close() (op.Put -> stream.FileStream.Close)
	// removes the spooled temp file for us, success or failure -- no
	// separate os.Remove needed here.
	return mapOpErr(fs.PutDirectly(w.ctx, w.dir, fstream, true))
}
