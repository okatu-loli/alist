// Package archive is the host-side dispatch layer behind archive browsing: it
// lets a caller look INSIDE an archive file already reachable through the
// normal fs read path (op.Authorize(ActRead) + fs.Get + fs.Link) without
// downloading the whole archive first. Meta/List/Extract are format-agnostic:
// they authorize the path, resolve the ext.ArchiveTool registered for the
// file's extension (internal/extension.FindArchiveToolForExt — built-in
// zip, or a Kind=ArchiveTool gRPC plugin for rar/7z/...), and dispatch to it.
// A driver-native ext.ArchiveReader/ArchiveGetter/ArchiveDecompress (see
// ext/driver.go) is a separate, orthogonal seam and out of scope here.
//
// Security model (mirrors internal/thumbnail's doc comment) -- and, since
// Phase E wave 2, applies EQUALLY to a third-party ArchiveTool plugin's
// output, never just the built-in zip tool:
//   - op.Authorize(ActRead) on the archive's own path is the ONLY access
//     boundary: a caller can browse into an archive only if they could
//     otherwise read/download the whole file.
//   - zip-slip: every entry name (and every caller-supplied inner path) is
//     cleaned and confined to the virtual root; an entry whose name contains
//     a ".." path segment, or a List child name that isn't a single clean
//     path segment, is excluded from every listing and is never addressable
//     by Extract (normalizeZipName / validEntryName below) -- this host-side
//     check runs on every ArchiveTool's output, not just the built-in zip
//     format's, so a plugin that forgets to defend against it doesn't get to
//     smuggle a traversal path past the host.
//   - zip-bomb: an archive's total declared entry count and uncompressed size
//     are capped at Meta/List time (maxZipEntries, maxZipTotalUncompressedSize)
//     -- both inside the built-in zip tool's own parsing AND again, generically,
//     against whatever any resolved tool reports back, so an untrusted plugin
//     that doesn't self-enforce these caps is still caught. A single entry's
//     declared uncompressed size is capped at Extract time
//     (maxExtractEntryBytes) both up front (the tool checks its own declared
//     size) AND via a bounded reader this package wraps around every tool's
//     returned stream, so an entry that lies about its declared size still
//     can't stream past the cap regardless of which tool produced it.
//   - For the built-in zip tool specifically, the archive itself is read via a
//     ranged io.ReaderAt (see readerAtForLink) passed in-process through
//     ext.ArchiveSource.ReaderAt -- archive/zip.NewReader seeks to read the
//     central directory at the end and then only the requested entry's
//     compressed bytes, so a large archive is never buffered into memory
//     whole, and a remote (non-local) storage is never downloaded in full just
//     to browse it. An out-of-process (gRPC plugin) ArchiveTool cannot receive
//     a live ReaderAt across the process boundary, so the archive's full
//     content is localized to a host-owned temp file first (ArchiveSource.Path)
//     -- see resolveArchiveSource.
package archive

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	stdpath "path"
	"strings"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/http_range"
)

// Sentinel errors. server/handles/archive.go maps these (via errors.Is) to
// HTTP status. Unchanged across the Phase E wave 2 dispatch refactor: the
// built-in zip tool returns these exact values (it lives in this same
// package), and mapToolErr translates a cross-boundary plugin's ext.Error
// code back to the matching one of these, so a caller never has to care
// which ArchiveTool actually served a given archive.
var (
	// ErrNotArchive is returned for a path whose extension no registered
	// ArchiveTool claims, a directory, or content that doesn't parse as that
	// tool's archive format.
	ErrNotArchive = errors.New("archive: not a recognized/parsable archive")
	// ErrArchiveEncrypted is returned by Extract when the specific entry
	// requested is encrypted and the tool cannot (or was not given a
	// password to) decrypt it. Std archive/zip cannot decrypt ANY zip
	// encryption scheme (no password support at all), so such an entry can
	// never be read by the built-in zip tool -- the password parameter every
	// exported function accepts is a forward-looking seam for a
	// driver-backed or third-party-decrypt tool that CAN honor it.
	ErrArchiveEncrypted = errors.New("archive: entry is encrypted, decryption is not supported")
	// ErrInnerNotFound is returned when innerPath (List) or the exact entry
	// (Extract) doesn't resolve to anything inside the archive -- including a
	// caller-supplied inner path that contains a ".." segment, so there is no
	// oracle distinguishing "traversal attempt" from "genuinely missing".
	ErrInnerNotFound = errors.New("archive: inner path not found in archive")
	// ErrIsDir is returned by Extract when innerPath names a directory.
	ErrIsDir = errors.New("archive: inner path is a directory")
	// ErrTooManyEntries is returned when the archive declares more entries
	// than maxZipEntries -- enforced both inside the built-in zip tool's own
	// parsing and, generically, against any tool's self-reported EntryCount.
	ErrTooManyEntries = errors.New("archive: central directory exceeds the maximum entry count")
	// ErrArchiveTooLarge is returned when the sum of every entry's declared
	// uncompressed size exceeds maxZipTotalUncompressedSize.
	ErrArchiveTooLarge = errors.New("archive: declares a total uncompressed size exceeding the maximum")
	// ErrEntryTooLarge is returned by Extract when the target entry's
	// declared (or, mid-stream, actual) uncompressed size exceeds
	// maxExtractEntryBytes.
	ErrEntryTooLarge = errors.New("archive: entry's uncompressed size exceeds the maximum extractable size")
)

// Caps -- vars (not consts) so tests can lower them to exercise the refusal
// paths without needing multi-hundred-thousand-entry or gigabyte fixtures.
// Applied both inside the built-in zip tool's own parsing (openZipReader) and
// again, generically, against any resolved ArchiveTool's reported
// counts/sizes (see Meta/Extract below) -- the latter is what actually
// protects against an untrusted plugin that doesn't self-enforce them.
var (
	// maxZipEntries bounds how many entries an archive's central directory
	// may declare before Meta/List/Extract refuse it outright
	// (ErrTooManyEntries).
	maxZipEntries = 100000
	// maxZipTotalUncompressedSize bounds the sum of every entry's declared
	// uncompressed size (ErrArchiveTooLarge) -- a coarse zip-bomb guard at
	// open time, independent of which single entry (if any) is later
	// extracted.
	maxZipTotalUncompressedSize int64 = 100 << 30 // 100 GiB
	// maxExtractEntryBytes bounds a single entry's declared uncompressed size
	// AND the actual bytes Extract's reader will ever yield (ErrEntryTooLarge)
	// -- the per-entry zip-bomb guard.
	maxExtractEntryBytes int64 = 10 << 30 // 10 GiB
)

// ArchiveMeta is the serializable meta-info of an archive returned by Meta.
type ArchiveMeta struct {
	Comment               string `json:"comment,omitempty"`
	Encrypted             bool   `json:"encrypted"`
	EntryCount            int    `json:"entry_count"`
	TotalUncompressedSize int64  `json:"total_uncompressed_size"`
}

// Entry is one immediate child of a virtual directory inside an archive, as
// returned by List.
type Entry struct {
	Name     string    `json:"name"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Meta authorizes path (ActRead) for user, resolves the ArchiveTool claiming
// its extension, and returns its meta-info: entry count, whether any entry is
// encrypted, and the total declared uncompressed size. path must already be
// the full, base-path-joined virtual path (same convention as
// internal/thumbnail.Generate).
func Meta(ctx context.Context, user *model.User, path, password string) (*ArchiveMeta, error) {
	tool, src, cleanup, err := resolveArchiveSource(ctx, user, path, password)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	tm, err := tool.Meta(ctx, src, password)
	if err != nil {
		return nil, mapToolErr(err)
	}
	// Host-side safety net: re-validated against ANY tool's self-reported
	// counts, not just trusted for the built-in zip tool (which already
	// enforces this identically inside openZipReader, making this a no-op for
	// it -- see the package doc comment).
	if tm.EntryCount > maxZipEntries {
		return nil, ErrTooManyEntries
	}
	if tm.TotalUncompressedSize > maxZipTotalUncompressedSize {
		return nil, ErrArchiveTooLarge
	}
	return &ArchiveMeta{
		Comment:               tm.Comment,
		Encrypted:             tm.Encrypted,
		EntryCount:            tm.EntryCount,
		TotalUncompressedSize: tm.TotalUncompressedSize,
	}, nil
}

// List returns the immediate children of innerPath within the archive at
// path (a virtual-directory view) -- innerPath=="" means the archive root.
func List(ctx context.Context, user *model.User, path, innerPath, password string) ([]Entry, error) {
	clean, _, ok := normalizeZipName(innerPath)
	if !ok {
		return nil, ErrInnerNotFound
	}

	tool, src, cleanup, err := resolveArchiveSource(ctx, user, path, password)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	rawEntries, err := tool.List(ctx, src, clean, password)
	if err != nil {
		return nil, mapToolErr(err)
	}

	// Host-side safety net: a returned child name must be a single, clean
	// path segment (never "..", never carrying a nested "/segment" a
	// malicious/buggy tool could smuggle in) -- excluded, not erred, mirroring
	// how the built-in zip tool itself already excludes a zip-slip entry from
	// its own listing.
	result := make([]Entry, 0, len(rawEntries))
	for _, e := range rawEntries {
		if !validEntryName(e.Name) {
			continue
		}
		result = append(result, Entry{Name: e.Name, IsDir: e.IsDir, Size: e.Size, Modified: e.Modified})
	}
	return result, nil
}

// Extract opens ONE inner file entry for streaming. innerPath must name a
// file, not a directory (ErrIsDir). The returned ReadCloser's Close releases
// every resource the dispatch acquired to serve it (the tool's own stream,
// the underlying link resource for an in-process tool, or the localized temp
// file + pluginproc reference for a gRPC tool) -- callers must always Close
// it.
func Extract(ctx context.Context, user *model.User, path, innerPath, password string) (io.ReadCloser, int64, error) {
	target, _, ok := normalizeZipName(innerPath)
	if !ok || target == "" {
		return nil, 0, ErrInnerNotFound
	}

	tool, src, cleanup, err := resolveArchiveSource(ctx, user, path, password)
	if err != nil {
		return nil, 0, err
	}

	rc, size, err := tool.Extract(ctx, src, target, password)
	if err != nil {
		cleanup()
		return nil, 0, mapToolErr(err)
	}
	// Host-side safety net: re-validated against ANY tool's self-reported
	// size before even reading from it -- a no-op for the built-in zip tool,
	// which already refuses an oversize entry itself.
	if size > maxExtractEntryBytes {
		_ = rc.Close()
		cleanup()
		return nil, 0, ErrEntryTooLarge
	}
	bounded := &boundedEntryReadCloser{
		r:         rc,
		remaining: maxExtractEntryBytes,
		closers:   []io.Closer{rc, closerFunc(cleanup)},
	}
	return bounded, size, nil
}

// resolveArchiveSource is the shared authorize-then-dispatch path every
// exported function funnels through: op.Authorize(ActRead) is the sole access
// boundary, then the file's extension picks an ext.ArchiveTool
// (internal/extension.FindArchiveToolForExt) and an ext.ArchiveSource is built
// for it -- ReaderAt for an in-process tool (zero-copy, no download), or a
// localized temp file for an out-of-process gRPC tool (which cannot receive a
// live ReaderAt across the process boundary). The gRPC path is capped
// (maxArchiveLocalizeBytes, ErrArchiveLocalizeTooLarge) and cached across
// repeated calls against the same archive (defaultLocalizeCache) -- browsing
// one archive is several HTTP requests (Meta, List, Extract), each of which
// would otherwise re-download the whole thing. The returned cleanup releases
// EVERY resource this acquired (underlying link resource / cache reference /
// pluginproc reference) and must always be called exactly once.
func resolveArchiveSource(ctx context.Context, user *model.User, path, password string) (ext.ArchiveTool, ext.ArchiveSource, func(), error) {
	if err := op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: path, Password: password}); err != nil {
		return nil, ext.ArchiveSource{}, func() {}, err
	}

	fileExt := strings.TrimPrefix(strings.ToLower(stdpath.Ext(path)), ".")
	tool, remote, release, ok := extension.FindArchiveToolForExt(extension.DefaultRegistry, fileExt)
	if !ok {
		return nil, ext.ArchiveSource{}, func() {}, ErrNotArchive
	}

	obj, err := fs.Get(ctx, path, &fs.GetArgs{NoLog: true})
	if err != nil {
		release()
		return nil, ext.ArchiveSource{}, func() {}, err
	}
	if obj.IsDir() {
		release()
		return nil, ext.ArchiveSource{}, func() {}, ErrNotArchive
	}

	link, linkObj, err := fs.Link(ctx, path, model.LinkArgs{})
	if err != nil {
		release()
		return nil, ext.ArchiveSource{}, func() {}, err
	}
	size := linkObj.GetSize()

	if !remote {
		ra, closer, err := readerAtForLink(ctx, link, size)
		if err != nil {
			release()
			return nil, ext.ArchiveSource{}, func() {}, err
		}
		cleanup := func() {
			_ = closer.Close()
			release()
		}
		return tool, ext.ArchiveSource{ReaderAt: ra, Size: size}, cleanup, nil
	}

	if size > maxArchiveLocalizeBytes {
		release()
		return nil, ext.ArchiveSource{}, func() {}, ErrArchiveLocalizeTooLarge
	}

	// Cache the localized temp file across repeated Meta/List/Extract calls
	// against the SAME archive (browsing one is several HTTP requests, each
	// of which used to re-download the whole thing) -- keyed by the archive's
	// "source signature" (virtual path + size + modtime), so a caller
	// re-opening the SAME unchanged file reuses the download and a caller
	// opening it again AFTER it changed gets a fresh one. See localize_cache.go.
	key := localizeCacheKey{path: path, size: size, modified: obj.ModTime().UnixNano()}
	tmpPath, cacheRelease, err := defaultLocalizeCache.localize(key, func() (string, error) {
		return localizeToTemp(ctx, link, size)
	})
	if err != nil {
		release()
		return nil, ext.ArchiveSource{}, func() {}, err
	}
	cleanup := func() {
		// cacheRelease only drops this caller's reference -- the temp file
		// itself is removed later by the cache's own TTL/size-budget sweep,
		// never synchronously here (a concurrent caller may still be reading
		// the same cached file).
		cacheRelease()
		release()
	}
	return tool, ext.ArchiveSource{Path: tmpPath, Size: size}, cleanup, nil
}

// localizeToTemp downloads link's full content (size bytes) into a fresh temp
// file under conf.Conf.TempDir and returns its path. Only ever needed for an
// out-of-process (gRPC) ArchiveTool -- an in-process tool takes the
// ReaderAt fast path in resolveArchiveSource instead, exactly preserving the
// pre-Phase-E-wave-2 behavior of never downloading a remote archive whole
// just to browse it.
func localizeToTemp(ctx context.Context, link *model.Link, size int64) (path string, err error) {
	ra, closer, err := readerAtForLink(ctx, link, size)
	if err != nil {
		return "", err
	}
	defer closer.Close()

	// Defensive: conf.Conf.TempDir is normally created once at startup
	// (bootstrap.InitConfig), but this doesn't rely on that ordering --
	// mirrors internal/thumbnail's writeCacheAtomic doing the same
	// MkdirAll-before-CreateTemp for its own cache directory.
	if err := os.MkdirAll(conf.Conf.TempDir, 0o700); err != nil {
		return "", err
	}

	f, err := os.CreateTemp(conf.Conf.TempDir, "alist-archivetool-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()

	if _, err = io.Copy(f, io.NewSectionReader(ra, 0, size)); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// mapToolErr translates a cross-boundary ext.Error code (a gRPC ArchiveTool's
// error, flattened over the wire) back to the matching package sentinel, so a
// caller (server/handles/archive.go's error-status mapping) never has to care
// whether the archive was served by the built-in zip tool or a plugin. The
// built-in zip tool returns these sentinels DIRECTLY (no ext.Error wrapping,
// since it lives in this same package) -- ext.CodeOf reports not-ok for those,
// so err passes through completely unchanged: zero behavior change for zip.
func mapToolErr(err error) error {
	code, ok := ext.CodeOf(err)
	if !ok {
		return err
	}
	switch code {
	case ext.ErrCodeObjectNotFound:
		return ErrInnerNotFound
	case ext.ErrCodeWrongArchivePassword:
		return ErrArchiveEncrypted
	case ext.ErrCodeArchiveIsDir:
		return ErrIsDir
	case ext.ErrCodeArchiveInvalid:
		return ErrNotArchive
	default:
		return err
	}
}

// closerFunc adapts a plain func() (a cleanup callback with no error to
// report) to an io.Closer, so it can ride alongside a real io.Closer in
// boundedEntryReadCloser.closers.
type closerFunc func()

func (f closerFunc) Close() error {
	f()
	return nil
}

// validEntryName reports whether name is safe to surface as one List result:
// non-empty, not "." or "..", and not itself a multi-segment path (a List
// result is always ONE virtual-directory child, never a nested path) --
// guards against a malicious/buggy ArchiveTool trying to smuggle a traversal
// segment or an unexpectedly-nested name past the host. The built-in zip
// tool's own List already only ever produces single-segment child names, so
// this never rejects anything for it.
func validEntryName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
}

// ZipToolExtensionID is the built-in extension id the zip ArchiveTool
// registers under (internal/bootstrap.InitArchiveTools): a manageable
// Kind=ArchiveTool entry alongside every other built-in.
const ZipToolExtensionID = "builtin.archive.zip"

// NewZipArchiveTool returns the built-in, in-process ext.ArchiveTool std
// archive/zip backs. Stateless: a bare value is a complete, ready instance.
func NewZipArchiveTool() ext.ArchiveTool { return zipArchiveTool{} }

// zipArchiveTool is the built-in ext.ArchiveTool implementation std
// archive/zip backs (Phase E wave 2). Every exported package-level
// Meta/List/Extract above funnels through this via resolveArchiveSource --
// this type's methods are the same zip-parsing logic that used to live
// directly in those functions before the ArchiveTool extension point existed,
// now reached through that seam so a third-party plugin can sit alongside it
// for other archive formats. Always dispatched in-process (never gRPC, since
// it is registered via RegisterTrustedArchiveToolInstance): its methods
// require src.ReaderAt and never look at src.Path.
type zipArchiveTool struct{}

var _ ext.ArchiveTool = zipArchiveTool{}

func (zipArchiveTool) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ZipToolExtensionID,
		Name:  map[string]string{"en": "Archive: ZIP", "zh": "归档:ZIP"},
		Kinds: []ext.Kind{ext.KindArchiveTool},
	}
}
func (zipArchiveTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (zipArchiveTool) Shutdown(ctx context.Context) error         { return nil }

func (zipArchiveTool) Exts() []string { return []string{"zip"} }

func (zipArchiveTool) Meta(ctx context.Context, src ext.ArchiveSource, password string) (ext.ArchiveToolMeta, error) {
	zr, err := openZipReader(src)
	if err != nil {
		return ext.ArchiveToolMeta{}, err
	}
	m := ext.ArchiveToolMeta{Comment: zr.Comment}
	for _, f := range zr.File {
		name, _, ok := normalizeZipName(f.Name)
		if !ok || name == "" {
			continue // zip-slip / empty: excluded from every view
		}
		if f.Flags&0x1 != 0 {
			m.Encrypted = true
		}
		if !f.FileInfo().IsDir() {
			m.EntryCount++
			m.TotalUncompressedSize += int64(f.UncompressedSize64)
		}
	}
	return m, nil
}

func (zipArchiveTool) List(ctx context.Context, src ext.ArchiveSource, innerPath, password string) ([]ext.ArchiveEntry, error) {
	zr, err := openZipReader(src)
	if err != nil {
		return nil, err
	}

	base := innerPath // already normalized+validated by the generic dispatch layer

	order := make([]string, 0)
	children := make(map[string]*ext.ArchiveEntry)
	found := base == "" // the root always "exists"

	for _, f := range zr.File {
		name, isDirHint, ok := normalizeZipName(f.Name)
		if !ok || name == "" {
			continue
		}
		entryIsDir := isDirHint || f.FileInfo().IsDir()

		rel := name
		if base != "" {
			if name == base {
				if entryIsDir {
					found = true
				}
				continue // the directory itself, not a child of itself
			}
			prefix := base + "/"
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			found = true
			rel = strings.TrimPrefix(name, prefix)
		}
		if rel == "" {
			continue
		}

		var childName string
		var childIsDir bool
		if idx := strings.IndexByte(rel, '/'); idx >= 0 {
			childName = rel[:idx]
			childIsDir = true
		} else {
			childName = rel
			childIsDir = entryIsDir
		}

		e, exists := children[childName]
		if !exists {
			e = &ext.ArchiveEntry{Name: childName}
			children[childName] = e
			order = append(order, childName)
		}
		if childIsDir {
			e.IsDir = true
		}
		// Only an exact (non-implicit) entry at this level carries real
		// size/modtime -- an implicit directory (inferred from a deeper
		// entry's path) has neither.
		if strings.IndexByte(rel, '/') < 0 {
			e.Size = int64(f.UncompressedSize64)
			e.Modified = f.Modified
		}
	}

	if !found {
		return nil, ErrInnerNotFound
	}
	result := make([]ext.ArchiveEntry, 0, len(order))
	for _, name := range order {
		result = append(result, *children[name])
	}
	return result, nil
}

func (zipArchiveTool) Extract(ctx context.Context, src ext.ArchiveSource, innerPath, password string) (io.ReadCloser, int64, error) {
	zr, err := openZipReader(src)
	if err != nil {
		return nil, 0, err
	}

	target := innerPath // already normalized+validated by the generic dispatch layer

	var match *zip.File
	for _, f := range zr.File {
		name, _, ok := normalizeZipName(f.Name)
		if !ok || name != target {
			continue
		}
		match = f
		break
	}
	if match == nil {
		return nil, 0, ErrInnerNotFound
	}
	if match.FileInfo().IsDir() || strings.HasSuffix(match.Name, "/") {
		return nil, 0, ErrIsDir
	}
	if match.Flags&0x1 != 0 {
		return nil, 0, ErrArchiveEncrypted
	}
	size := int64(match.UncompressedSize64)
	if size > maxExtractEntryBytes {
		return nil, 0, ErrEntryTooLarge
	}

	rc, err := match.Open()
	if err != nil {
		return nil, 0, err
	}
	return rc, size, nil
}

// openZipReader parses src (which must carry a ReaderAt -- the built-in zip
// tool is always dispatched in-process) as a zip central directory and
// enforces the entry-count/total-size caps, exactly as internal/archive's
// pre-Phase-E-wave-2 openArchive did.
func openZipReader(src ext.ArchiveSource) (*zip.Reader, error) {
	if src.ReaderAt == nil {
		return nil, errors.New("archive: zip tool requires an in-process ReaderAt source")
	}
	zr, err := zip.NewReader(src.ReaderAt, src.Size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotArchive, err)
	}
	if len(zr.File) > maxZipEntries {
		return nil, ErrTooManyEntries
	}
	var total int64
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			total += int64(f.UncompressedSize64)
		}
	}
	if total > maxZipTotalUncompressedSize {
		return nil, ErrArchiveTooLarge
	}
	return zr, nil
}

// normalizeZipName cleans a raw zip entry name (or caller-supplied inner
// path) into a slash-separated path relative to the archive root: no
// leading/trailing slash, no empty or "." segments. ok is false if any
// segment is literally ".." -- such a name is a zip-slip attempt (for an
// entry) or a traversal attempt (for a caller's inner path) and is NEVER
// listed nor addressable; the caller must treat !ok the same as "not found"
// (no oracle). isDirHint reports whether the raw name ended in "/" (zip's own
// directory-entry convention), for names that don't otherwise carry that
// information post-clean.
func normalizeZipName(raw string) (name string, isDirHint bool, ok bool) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	isDirHint = strings.HasSuffix(raw, "/")
	segs := strings.Split(raw, "/")
	clean := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			return "", false, false
		}
		clean = append(clean, s)
	}
	return strings.Join(clean, "/"), isDirHint, true
}

// readerAtForLink adapts a resolved model.Link into a ranged io.ReaderAt+
// io.Closer pair, the same two cases internal/webdav's newLinkReadSeeker and
// internal/thumbnail's openLinkReader handle: a local/smb-like driver hands
// back an MFile directly (model.File already satisfies io.ReaderAt); a
// URL-backed (remote) driver goes through
// stream.GetRangeReadCloserFromLink, wrapped by rangeReaderAt below to
// present arbitrary-offset ReadAt calls as the ranged RangeRead requests
// zip.NewReader's central-directory seeks and per-entry reads need.
func readerAtForLink(ctx context.Context, link *model.Link, size int64) (io.ReaderAt, io.Closer, error) {
	if link.MFile != nil {
		return link.MFile, link.MFile, nil
	}
	rrc := link.RangeReadCloser
	if rrc == nil {
		if link.URL == "" {
			return nil, nil, errors.New("archive: link has neither a local file nor a URL to read")
		}
		var err error
		rrc, err = stream.GetRangeReadCloserFromLink(size, link)
		if err != nil {
			return nil, nil, err
		}
	}
	return &rangeReaderAt{ctx: ctx, rrc: rrc}, rrc, nil
}

// rangeReaderAt turns a model.RangeReadCloserIF (one-shot "give me bytes
// [start,start+length)") into an io.ReaderAt, satisfying io.ReaderAt's
// contract by always fully filling p or returning a non-nil error (via
// io.ReadFull) explaining the short read. Each ReadAt opens and closes its
// own range read -- archive/zip only ever calls this to read the central
// directory once and then each requested entry's compressed bytes, so the
// per-call overhead of a remote range request is bounded by the number of
// entries actually touched, never the whole archive.
type rangeReaderAt struct {
	ctx context.Context
	rrc model.RangeReadCloserIF
}

func (r *rangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	rc, err := r.rrc.RangeRead(r.ctx, http_range.Range{Start: off, Length: int64(len(p))})
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	return io.ReadFull(rc, p)
}

// boundedEntryReadCloser wraps an ArchiveTool's returned entry stream so a
// caller can never read past maxExtractEntryBytes even if the tool's declared
// size understated its real output (a lying/corrupt zip-bomb entry, or a
// misbehaving plugin), and Close releases every underlying resource (the
// tool's own stream AND the dispatch's cleanup -- the archive's link resource
// / localized temp file / pluginproc reference) in one call.
type boundedEntryReadCloser struct {
	r         io.Reader
	remaining int64
	closers   []io.Closer
}

func (b *boundedEntryReadCloser) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, ErrEntryTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *boundedEntryReadCloser) Close() error {
	var errsList []error
	for _, c := range b.closers {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			errsList = append(errsList, err)
		}
	}
	return errors.Join(errsList...)
}
