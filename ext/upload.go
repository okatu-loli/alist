package ext

// UploadStream is the host-held, replayable upload source handed to a driver's
// Putter (9.0 §2, "pull model"). The host lands the inbound stream in a
// temp/seekable source; the driver reads it by range via ReadAt. This satisfies
// two-pass reads (hash the whole file, then upload in parts), resumable upload,
// and header peeking of non-seekable streams.
//
// Responsibilities the host keeps (so 90 drivers need not each reimplement them,
// 9.0 §2): rate limiting is applied host-side inside ReadAt; cancellation flows
// through ctx on the driver's request methods; progress is reported via a Host
// callback rather than a per-call progress func.
type UploadStream interface {
	Info() UploadInfo
	// ReadAt is a replayable random read served by the host from a temp/seekable
	// source. It follows io.ReaderAt semantics.
	ReadAt(p []byte, off int64) (n int, err error)
}

// UploadInfo is the metadata about an upload (9.0 §2).
type UploadInfo struct {
	Name     string            `json:"name"`
	Size     int64             `json:"size"`
	Mimetype string            `json:"mimetype,omitempty"`
	Hashes   map[string]string `json:"hashes,omitempty"` // host may precompute; empty => driver hashes itself via ReadAt
	Exist    *Obj              `json:"exist,omitempty"`  // carries v3 GetExist/SetExist overwrite semantics
}
