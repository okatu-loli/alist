package ext

import (
	"context"
	"io"
	"time"
)

// ArchiveTool is the extension point behind archive browsing (Phase E wave
// 2): a Kind=ArchiveTool extension lets the `internal/archive` dispatch layer
// look INSIDE an archive file (list its entries, extract one) without the
// caller downloading the whole thing first. The built-in zip fallback
// (previously internal/archive's only, hardcoded format) is itself wired as
// one such extension (builtin.archive.zip) so a third-party plugin can add
// rar/7z/... alongside it through the exact same dispatch path.
//
// Security model: the HOST alone owns the zip-slip / zip-bomb invariants —
// op.Authorize(ActRead) on the archive's own path is the sole access
// boundary (enforced by internal/archive before a tool is ever consulted),
// and every entry name / count / size a tool reports is re-validated by the
// host dispatch layer against the SAME caps and traversal check the built-in
// zip implementation itself applies, before being handed to a caller. A tool
// is never trusted to have already done this — see internal/archive's
// dispatch layer doc comment for the concrete caps.
type ArchiveTool interface {
	Extension

	// Exts reports the lowercase, dot-less file extensions this tool claims
	// (e.g. "zip", "rar", "7z"). The host dispatch layer matches a requested
	// archive's extension against every registered tool's Exts() to decide
	// who handles it; a file whose extension nothing claims is refused the
	// same way an unsupported format always was.
	Exts() []string

	// Meta returns an archive's meta-info: entry count, whether any entry is
	// encrypted, and the total declared uncompressed size.
	Meta(ctx context.Context, src ArchiveSource, password string) (ArchiveToolMeta, error)

	// List returns the immediate children of innerPath within the archive (a
	// virtual-directory view) -- innerPath=="" means the archive root.
	List(ctx context.Context, src ArchiveSource, innerPath, password string) ([]ArchiveEntry, error)

	// Extract opens ONE inner file entry for streaming. innerPath must name a
	// file, not a directory. size is the entry's declared uncompressed size
	// (for a Content-Length-style header); the returned ReadCloser streams the
	// entry's decompressed bytes and must always be Closed by the caller.
	Extract(ctx context.Context, src ArchiveSource, innerPath, password string) (rc io.ReadCloser, size int64, err error)
}

// ArchiveSource is how the host hands an archive's bytes to an ArchiveTool.
//
// Path/Size is the wire-safe form: a gRPC-dispatched (out-of-process) tool
// only ever sees these two fields, because a live byte-range handle cannot
// itself cross the plugin process boundary -- the host localizes the
// archive's full content to a local temp file before dispatching to such a
// tool (Path names it, Size is its length), and removes the temp file once
// the call (or, for Extract, the returned stream) is done.
//
// ReaderAt is an IN-PROCESS-ONLY fast path: when the host dispatches to a
// trusted, in-process tool (the built-in zip implementation, or any future
// in-process ArchiveTool), it sets ReaderAt to a ranged reader over the
// archive's storage-backed link directly -- exactly what internal/archive did
// before this extension point existed -- so a remote-driver archive is never
// downloaded whole just to be browsed. A gRPC client's ArchiveSource never
// populates this field (it cannot cross the wire); an in-process tool SHOULD
// prefer ReaderAt over Path/Size when both happen to be set, since it is
// never populated together with a real Path in the SAME dispatch.
type ArchiveSource struct {
	Path     string
	Size     int64
	ReaderAt io.ReaderAt `json:"-"`
}

// ArchiveToolMeta is the serializable meta-info of an archive returned by
// ArchiveTool.Meta. Field naming mirrors the pre-existing internal/archive
// ArchiveMeta.
type ArchiveToolMeta struct {
	Comment               string `json:"comment,omitempty"`
	Encrypted             bool   `json:"encrypted"`
	EntryCount            int    `json:"entry_count"`
	TotalUncompressedSize int64  `json:"total_uncompressed_size"`
}

// ArchiveEntry is one immediate child of a virtual directory inside an
// archive, as returned by ArchiveTool.List. Mirrors the pre-existing
// internal/archive Entry.
type ArchiveEntry struct {
	Name     string    `json:"name"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}
