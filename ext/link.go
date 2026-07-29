package ext

import (
	"context"
	"io"
	"net/http"
)

// ByteSource is the wire-serializable classification of where a Link's bytes
// come from (9.0 §1). v3 had three forms (URL / RangeReadCloser / MFile), two of
// them json:"-". They collapse to two: URL, and Stream — where Stream absorbs
// both RangeReadCloser and MFile (an MFile is just "can read a range from an open
// file", a special case of Stream).
type ByteSource string

const (
	ByteSourceURL    ByteSource = "url"    // host pulls the bytes from URL+Header itself
	ByteSourceStream ByteSource = "stream" // bytes come out of the driver: host calls back RangeOpener.OpenRange
)

// Link describes how the host obtains a file's bytes (9.0 §1). Unlike v3's
// model.Link it carries no live Go handles, so it survives serialization.
type Link struct {
	ByteSource   ByteSource  `json:"byte_source"`
	URL          string      `json:"url,omitempty"`       // ByteSource==url
	Header       http.Header `json:"header,omitempty"`    // ByteSource==url
	StreamID     string      `json:"stream_id,omitempty"` // ByteSource==stream: echoed back on OpenRange to locate the session
	RangeSupport bool        `json:"range_support"`
	Expiration   int64       `json:"expiration,omitempty"`   // seconds; 0 = do not cache (v3 nil)
	IPCacheKey   bool        `json:"ip_cache_key,omitempty"` // cache key is scoped by client IP (v3 op/fs.go semantics)
	Concurrency  int         `json:"concurrency,omitempty"`
	PartSize     int         `json:"part_size,omitempty"`
}

// Range is a byte range request. Length < 0 means "to the end" (9.0 §1).
type Range struct {
	Start  int64
	Length int64
}

// RangeOpener is the byte source for ByteSourceStream links. The host's stream
// layer adapts it to its internal range-reader. Under gRPC (9.2) this becomes a
// server-streaming RPC; in-process native drivers may take a zero-copy fast path
// (the SDK's in-process adapter recognizes it) that never hits the wire.
type RangeOpener interface {
	OpenRange(ctx context.Context, ref ObjRef, streamID string, rng Range) (io.ReadCloser, error)
}

// LinkArgs is the serializable redefinition of v3's LinkArgs (which carried a
// *http.Request). The host extracts the whitelisted subset a driver needs
// instead of passing the whole request across the boundary (9.0 §1).
type LinkArgs struct {
	IP       string      `json:"ip"`
	Header   http.Header `json:"header,omitempty"`
	Type     string      `json:"type,omitempty"`
	Redirect bool        `json:"redirect"`
}

// ListArgs are the arguments to StorageDriver.List.
type ListArgs struct {
	ReqPath string `json:"req_path,omitempty"`
	Refresh bool   `json:"refresh,omitempty"`
}
