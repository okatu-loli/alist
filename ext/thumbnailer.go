package ext

import (
	"context"
	"io"
)

// Thumbnailer is the extension point behind internal/thumbnail's Generate
// (Phase E wave 3): a Kind=Thumbnailer extension lets the host dispatch a
// still-image (or, for a future plugin, a video/other-format) source to a
// renderer that produces a resized preview, without internal/thumbnail
// hardcoding one specific decode library. The built-in disintegration/imaging
// renderer (previously internal/thumbnail's only, hardcoded path) is itself
// wired as one such extension (builtin.thumbnail.image) so a third-party
// plugin can add a format imaging cannot decode (e.g. a video codec, HEIC)
// alongside it through the exact same dispatch path.
//
// Security model (mirrors ext/archive_tool.go's ArchiveTool doc comment): the
// HOST alone owns the access-control and resource-ceiling invariants —
// op.Authorize(ActRead) on the source path is the sole access boundary
// (enforced by internal/thumbnail before a Thumbnailer is ever consulted),
// and the bytes a Thumbnailer hands back are re-capped by the host dispatch
// layer (a fixed maximum output size) before ever being cached or served —
// a Thumbnailer is never trusted to have already bounded its own output. The
// source-side bounds (max source bytes, max decoded pixel count) the built-in
// renderer applies are a property of THAT renderer, not a host-enforced
// invariant every Thumbnailer must share, since a future non-image renderer
// (video) has a different notion of "source too large."
type Thumbnailer interface {
	Extension

	// Exts reports the lowercase, dot-less file extensions this thumbnailer
	// claims (e.g. "jpg", "png"). The host dispatch layer matches a
	// requested source's extension against every registered thumbnailer's
	// Exts() to decide who handles it; a file whose extension nothing claims
	// falls back the same way an unsupported format always did
	// (ErrNotImageThumbnailable).
	Exts() []string

	// Render decodes src and returns a preview fit within maxDim x maxDim
	// (aspect-preserving), plus the encoded image bytes' MIME type (e.g.
	// "image/jpeg"). maxDim is already clamped to [MinMaxDim, MaxMaxDim] by
	// the host before this is called.
	Render(ctx context.Context, src ThumbSource, maxDim int) (data []byte, mime string, err error)
}

// ThumbSource is how the host hands a Thumbnailer the source file's bytes.
//
// Path/Size is the wire-safe form: a gRPC-dispatched (out-of-process)
// thumbnailer would only ever see these two fields, mirroring
// ArchiveSource's Path/Size — the host would localize the source's full
// content to a local temp file before dispatching to such a tool. This form
// is NOT yet exercised: the cross-process (binary-stream) leg for
// Kind=Thumbnailer is deferred (the wire format for a large image/video
// stream is not yet decided), so Path/Size are currently always zero-value
// for every dispatch this host performs.
//
// Reader is the IN-PROCESS-ONLY fast path: the host sets Reader to a bounded
// stream over the source's storage-backed link directly (the same
// openLinkReader path internal/thumbnail always used), so an in-process
// thumbnailer (the built-in image renderer, or any future in-process
// Thumbnailer) never requires the source to be localized to disk first.
// Render must not retain Reader past the call — it reads sequentially to
// EOF, unlike ArchiveSource's random-access ReaderAt, since generating a
// thumbnail is inherently a single sequential decode of the whole source.
type ThumbSource struct {
	Path   string
	Size   int64
	Reader io.Reader `json:"-"`
}
