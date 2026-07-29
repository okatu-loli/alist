// Package thumbnail generates a resized JPEG preview for an image file the
// caller may read, reusing the exact same authorized read path WebDAV and
// server/handles/fsread.go use to fetch a file's bytes (internal/fs.Get +
// internal/fs.Link + internal/stream.GetRangeReadCloserFromLink/link.MFile).
// op.Authorize(ActRead) is the ONLY access-control boundary here: a caller
// only ever thumbnails a path they could otherwise browse/download.
//
// Video thumbnails (ffmpeg) are explicitly out of scope -- only the standard
// still-image formats disintegration/imaging can decode are supported.
//
// Since Phase E wave 3 the actual decode/resize/encode is no longer
// hardcoded here: Generate authorizes and resolves the source's stream
// exactly as before, then dispatches to an ext.Thumbnailer registered for
// the source's file extension (internal/extension.FindThumbnailerForExt) --
// the built-in disintegration/imaging renderer below (builtin.thumbnail.image)
// is itself wired as one such extension, so a third-party plugin could add a
// format imaging cannot decode alongside it through the same dispatch path.
// Caching, the max-dimension clamp, and the OUTPUT byte-size cap
// (maxThumbOutputBytes) stay host concerns in this package's dispatch layer,
// applied uniformly to a plugin's returned bytes exactly like the built-in
// renderer's own bytes -- a Thumbnailer is never trusted to have already
// bounded its own output. See ext/thumbnailer.go's doc comment for the full
// security model.
package thumbnail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/http_range"
	"github.com/disintegration/imaging"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultMaxDim is used when the caller supplies no w= query param.
	DefaultMaxDim = 256
	// MinMaxDim/MaxMaxDim clamp the caller-supplied max dimension to a sane
	// range -- large enough for a browsable-listing thumbnail, small enough
	// that a caller can't ask the server to "resize" (i.e. just re-encode) an
	// arbitrarily large source image as a way to burn CPU/RAM.
	MinMaxDim = 32
	MaxMaxDim = 512

	// jpegQuality is the fixed encode quality for every generated thumbnail.
	jpegQuality = 80
)

// maxThumbSourceBytes bounds how much of the SOURCE file this package will
// ever decode. Without this, a caller could point the endpoint at a
// multi-gigabyte file with an image extension and have the server try to
// decode the whole thing into memory (OOM/DoS) before ever getting to resize
// it down. 50 MiB comfortably covers any legitimate photo while refusing to
// even attempt anything wildly larger.
//
// A var, not a const -- mirrors internal/webdav's maxWebdavPutBytes: tests
// temporarily lower it so the oversize-source refusal path can be exercised
// without actually generating a multi-megabyte fixture image.
var maxThumbSourceBytes int64 = 50 << 20

// maxThumbPixels bounds the DECODED pixel count. The source-byte cap alone does
// not prevent a decompression bomb: a few-KB PNG can declare enormous
// dimensions that expand to gigabytes of RGBA on full decode (OOM/DoS). Before
// decoding we read the cheap header via image.DecodeConfig and refuse anything
// whose width*height exceeds this. 50 megapixels comfortably covers any real
// photo (a 50 MP source is already an exceptional camera) while rejecting the
// giga-pixel dimensions a bomb declares. A var so tests can lower it.
var maxThumbPixels int64 = 50 << 20

// Sentinel errors. The handler maps these (via errors.Is) to HTTP status --
// see server/handles/thumbnail.go.
var (
	// ErrNotImageThumbnailable is returned for a directory, or a file whose
	// extension is not one of the supported still-image formats.
	ErrNotImageThumbnailable = errors.New("thumbnail: not a thumbnailable image file")
	// ErrSourceTooLarge is returned when the source file exceeds
	// maxThumbSourceBytes -- the bounded-read refusal, not a partial decode.
	ErrSourceTooLarge = errors.New("thumbnail: source file exceeds the maximum thumbnailable size")
	// ErrDecodeFailed wraps any error imaging.Decode/imaging.Encode returns --
	// a corrupt file, a truncated download, or a file that merely has an
	// image extension but isn't actually decodable image data.
	ErrDecodeFailed = errors.New("thumbnail: failed to decode source image")
	// ErrRenderedThumbnailTooLarge is returned when a dispatched
	// ext.Thumbnailer's Render call succeeds but hands back more than
	// maxThumbOutputBytes -- the host-side output cap, enforced identically
	// for the built-in renderer and any third-party Thumbnailer plugin, so a
	// misbehaving (or malicious) plugin can never have its oversized bytes
	// cached or served.
	ErrRenderedThumbnailTooLarge = errors.New("thumbnail: rendered thumbnail exceeds the maximum output size")
)

// maxThumbOutputBytes bounds how many bytes ANY dispatched ext.Thumbnailer's
// Render call may hand back, checked by Generate right after the call
// returns and before the bytes are cached or served. This is an OUTPUT-side
// cap, distinct from maxThumbSourceBytes/maxThumbPixels (which bound what the
// built-in renderer itself reads/decodes on the INPUT side): without it, a
// Thumbnailer that does not itself bound its output (a buggy or malicious
// third-party plugin) could hand back an unbounded response for the host to
// happily cache/serve. 16 MiB comfortably covers any real JPEG/PNG thumbnail
// this package ever clamps maxDim to (<=512x512), even in an incompressible
// worst case, while still refusing a wildly oversized response. A var, not a
// const, so a dispatch test can lower it without a multi-megabyte fixture.
var maxThumbOutputBytes int64 = 16 << 20

// imageExtensions is the fixed, case-insensitive allowlist of extensions this
// package will attempt to thumbnail -- exactly the still-image formats
// disintegration/imaging reliably decodes. Deliberately independent of the
// admin-configurable conf.ImageTypes setting (used elsewhere for the file-type
// badge/icon): that list is display-only and may include formats imaging
// cannot decode, whereas this one gates an actual decode attempt.
var imageExtensions = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".webp": {}, ".bmp": {}, ".tiff": {},
}

// isThumbnailableExt reports whether path's extension is one imaging can
// decode, case-insensitively.
func isThumbnailableExt(path string) bool {
	_, ok := imageExtensions[strings.ToLower(filepath.Ext(path))]
	return ok
}

// ClampMaxDim normalizes a caller-supplied max dimension: <=0 becomes the
// default, and anything outside [MinMaxDim, MaxMaxDim] is clamped into range.
func ClampMaxDim(d int) int {
	if d <= 0 {
		return DefaultMaxDim
	}
	if d < MinMaxDim {
		return MinMaxDim
	}
	if d > MaxMaxDim {
		return MaxMaxDim
	}
	return d
}

// cacheDir is the on-disk thumbnail cache root, mirroring how other core
// packages (internal/backup, internal/frp, internal/extension) derive their
// own data-dir subdirectory from flags.DataDir.
func cacheDir() string {
	return filepath.Join(flags.DataDir, "thumbnails")
}

// cacheFileName is the cache key: sha256(path)_<maxDim>_<modUnix>.jpg. Folding
// the object's ModTime into the key makes the cache stale-safe -- a file
// overwritten in place (same path, new content, new ModTime) misses the old
// entry and regenerates, rather than serving a stale thumbnail forever.
func cacheFileName(path string, maxDim int, modUnix int64) string {
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%s_%d_%d.jpg", hex.EncodeToString(sum[:]), maxDim, modUnix)
}

// writeCacheAtomic writes data to dst via a temp-file-then-rename so a
// concurrent reader of dst (another request racing this one) never observes a
// partially-written cache file. The cache directory is created 0700 (owner
// only -- cached thumbnails can reveal image content, so the cache is not
// world-readable on disk even though the HTTP endpoint re-authorizes on every
// request).
func writeCacheAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".thumb-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// errBoundedReaderCapExceeded is the internal signal boundedReader.Read
// raises once more than its cap has been requested from it. Generate/render
// translate it into the exported ErrSourceTooLarge.
var errBoundedReaderCapExceeded = errors.New("thumbnail: bounded reader cap exceeded")

// boundedReader wraps r and fails closed (an explicit error, not a silent
// truncation/EOF) once more than max bytes have been read -- the "bounded
// source read" the design calls for, so decoding an adversarial or merely
// huge "image" can never balloon memory/CPU past a fixed ceiling.
type boundedReader struct {
	r         io.Reader
	remaining int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errBoundedReaderCapExceeded
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// renderThumbnail decodes r as an image (bounded to maxThumbSourceBytes),
// fits it within maxDim x maxDim (aspect-preserving, EXIF-orientation
// corrected), and JPEG-encodes the result. Split out from Generate so it can
// be unit-tested directly against in-memory bytes without a storage/op fixture.
func renderThumbnail(r io.Reader, maxDim int) ([]byte, error) {
	// Read the whole (byte-bounded) source into memory once so we can inspect
	// its declared dimensions cheaply before committing to a full decode.
	br := &boundedReader{r: r, remaining: maxThumbSourceBytes}
	data, err := io.ReadAll(br)
	if err != nil {
		if errors.Is(err, errBoundedReaderCapExceeded) {
			return nil, ErrSourceTooLarge
		}
		return nil, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}
	// Decompression-bomb guard: reject on header-declared pixel count before a
	// full decode allocates the bitmap.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxThumbPixels {
		return nil, ErrSourceTooLarge
	}
	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}
	thumb := imaging.Fit(img, maxDim, maxDim, imaging.Lanczos)
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, thumb, imaging.JPEG, imaging.JPEGQuality(jpegQuality)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}
	return buf.Bytes(), nil
}

// ImageThumbnailerExtensionID is the built-in extension id the still-image
// ext.Thumbnailer below registers under (internal/bootstrap.InitThumbnailers):
// a manageable built-in, exactly like archive.ZipToolExtensionID.
const ImageThumbnailerExtensionID = "builtin.thumbnail.image"

// NewImageThumbnailer returns the built-in, in-process ext.Thumbnailer that
// wraps renderThumbnail -- the disintegration/imaging-backed still-image
// renderer this package always used, now exposed as a first-class extension
// so it dispatches through internal/extension.FindThumbnailerForExt exactly
// like a third-party plugin would.
func NewImageThumbnailer() ext.Thumbnailer { return imageThumbnailer{} }

// imageThumbnailer implements ext.Thumbnailer. It has no state (Init/Shutdown
// are no-ops), exactly like archive's zipArchiveTool.
type imageThumbnailer struct{}

func (imageThumbnailer) Manifest() ext.Manifest {
	return ext.Manifest{ID: ImageThumbnailerExtensionID}
}
func (imageThumbnailer) Init(ctx context.Context, h ext.Host) error { return nil }
func (imageThumbnailer) Shutdown(ctx context.Context) error         { return nil }

// Exts reports imageExtensions' keys, dot-stripped -- the same still-image
// formats isThumbnailableExt has always gated.
func (imageThumbnailer) Exts() []string {
	exts := make([]string, 0, len(imageExtensions))
	for e := range imageExtensions {
		exts = append(exts, strings.TrimPrefix(e, "."))
	}
	return exts
}

// Render adapts src.Reader (the in-process-only fast path -- see
// ext.ThumbSource's doc comment; src.Path/Size are not yet exercised by any
// caller) to renderThumbnail, and reports the fixed JPEG mime type
// renderThumbnail always encodes to.
func (imageThumbnailer) Render(ctx context.Context, src ext.ThumbSource, maxDim int) ([]byte, string, error) {
	if src.Reader == nil {
		return nil, "", errors.New("thumbnail: image thumbnailer requires an in-process Reader source")
	}
	data, err := renderThumbnail(src.Reader, maxDim)
	if err != nil {
		return nil, "", err
	}
	return data, "image/jpeg", nil
}

// openLinkReader adapts a resolved model.Link into a plain io.ReadCloser of
// the whole object, the same two cases internal/webdav's newLinkReadSeeker
// handles: a local/smb-like driver hands back an MFile directly; a
// URL-backed (remote) driver goes through
// stream.GetRangeReadCloserFromLink + an unbounded (Length: -1) RangeRead,
// the same helper server/common.ProxyRange and internal/webdav use.
func openLinkReader(ctx context.Context, link *model.Link, size int64) (io.ReadCloser, error) {
	if link.MFile != nil {
		return link.MFile, nil
	}
	rrc := link.RangeReadCloser
	if rrc == nil {
		if link.URL == "" {
			return nil, errors.New("thumbnail: link has neither a local file nor a URL to read")
		}
		var err error
		rrc, err = stream.GetRangeReadCloserFromLink(size, link)
		if err != nil {
			return nil, err
		}
	}
	return rrc.RangeRead(ctx, http_range.Range{Start: 0, Length: -1})
}

// Generate authorizes path (ActRead, the sole access boundary -- a caller
// only ever thumbnails what they could otherwise read) for user, then returns
// a thumbnail: a cache hit on disk, or a freshly rendered-and-cached one.
//
// path must already be the full, base-path-joined virtual path (the same
// convention every server/handles fs.* handler follows: the handler calls
// user.JoinPath on the raw query/body path before touching op/fs).
func Generate(ctx context.Context, user *model.User, path string, maxDim int) ([]byte, string, error) {
	maxDim = ClampMaxDim(maxDim)

	// Authorize FIRST, before anything that would distinguish "not an image"
	// from "doesn't exist" from "is a directory" -- a caller with no ActRead
	// on path gets the same denial no matter what's actually there.
	if err := op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: path}); err != nil {
		return nil, "", err
	}

	// Dispatch by the source's own file extension, exactly like
	// internal/archive resolves an ArchiveTool: a miss here (no registered
	// Thumbnailer claims path's extension -- including when the built-in
	// image renderer has been admin-disabled) reports the exact same
	// ErrNotImageThumbnailable a plain unsupported extension always did.
	tool, _, release, ok := extension.FindThumbnailerForExt(extension.DefaultRegistry, filepath.Ext(path))
	if !ok {
		return nil, "", ErrNotImageThumbnailable
	}
	defer release()

	obj, err := fs.Get(ctx, path, &fs.GetArgs{NoLog: true})
	if err != nil {
		return nil, "", err
	}
	if obj.IsDir() {
		return nil, "", ErrNotImageThumbnailable
	}

	cachePath := filepath.Join(cacheDir(), cacheFileName(path, maxDim, obj.ModTime().Unix()))
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, cachePath, nil
	}

	link, linkObj, err := fs.Link(ctx, path, model.LinkArgs{})
	if err != nil {
		return nil, "", err
	}
	rc, err := openLinkReader(ctx, link, linkObj.GetSize())
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()

	data, _, err := tool.Render(ctx, ext.ThumbSource{Reader: rc}, maxDim)
	if err != nil {
		return nil, "", err
	}
	// Host-side output cap: enforced on EVERY dispatched Thumbnailer's bytes,
	// built-in or third-party plugin alike -- see maxThumbOutputBytes's doc
	// comment. Checked before the bytes ever reach the cache/response.
	if int64(len(data)) > maxThumbOutputBytes {
		return nil, "", ErrRenderedThumbnailTooLarge
	}

	if err := writeCacheAtomic(cachePath, data); err != nil {
		// Cache write is best-effort: a request that generated fine but
		// couldn't persist the cache still serves the correct bytes, it just
		// regenerates on the next request too.
		log.Warnf("thumbnail: failed to write cache %s: %v", cachePath, err)
	}
	return data, cachePath, nil
}
