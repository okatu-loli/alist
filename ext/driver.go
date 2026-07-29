package ext

import (
	"context"
	"encoding/json"
)

// StorageDriver is a storage backend extension (9.0 §3). On top of the
// process-level Extension lifecycle it adds instance-level lifecycle, because a
// single driver process multiplexes many mounted storages: ops route to an
// instance by ObjRef.StorageID.
type StorageDriver interface {
	Extension
	DriverConfig() DriverConfig

	// NewInstance delivers a mounted storage's Addition JSON to the driver and
	// builds its session. One mounted storage = one instance.
	NewInstance(ctx context.Context, storageID string, config json.RawMessage) error
	// DropInstance tears an instance down.
	DropInstance(ctx context.Context, storageID string) error

	// List lists a directory. dir.StorageID routes to the instance.
	List(ctx context.Context, dir ObjRef, args ListArgs) ([]Obj, error)
	// Link resolves how to fetch a file's bytes (see Link / ByteSource).
	Link(ctx context.Context, file ObjRef, args LinkArgs) (*Link, error)
}

// The following are the optional storage-driver capabilities. A driver declares
// the ones it implements in Manifest.Capabilities; the host validates the
// declaration against the implemented interfaces at registration.

type Mkdirer interface {
	Mkdir(ctx context.Context, parent ObjRef, name string) (Obj, error)
}

type Mover interface {
	Move(ctx context.Context, src, dstDir ObjRef) (Obj, error)
}

type Renamer interface {
	Rename(ctx context.Context, src ObjRef, newName string) (Obj, error)
}

type Copier interface {
	Copy(ctx context.Context, src, dstDir ObjRef) (Obj, error)
}

type Remover interface {
	Remove(ctx context.Context, obj ObjRef) error
}

// Putter uploads a file. The bytes are pulled from the host-held UploadStream
// (9.0 §2); progress/rate-limit/cancel are host concerns.
type Putter interface {
	Put(ctx context.Context, dstDir ObjRef, up UploadStream) (Obj, error)
}

// RapidPutter tries a hash-based instant ("rapid") upload: if the storage
// already has content matching the given full-file hashes, it links a new
// file without transferring bytes (SP-H). Semantics mirror the host's
// driver.PutRapid contract:
//   - hit:  (obj, true, nil)
//   - miss: (Obj{}, false, nil) — caller falls back to streaming
//   - hash flavor unavailable / not supported: error with code NOT_SUPPORT
//
// Implementations MUST NOT read or spool bytes to compute a hash.
// hashes keys are utils hash names ("md5", "sha1", "sha256").
type RapidPutter interface {
	TryRapidUpload(ctx context.Context, dstDir ObjRef, name string, size int64, hashes map[string]string) (Obj, bool, error)
}

// PutURLer uploads by handing a URL to the storage (v3 PutURL; offline
// SimpleHttp path).
type PutURLer interface {
	PutURL(ctx context.Context, dstDir ObjRef, name, url string) (Obj, error)
}

// Getter fetches an object directly by path, saving a List (9.0 §6).
type Getter interface {
	Get(ctx context.Context, storageID, path string) (Obj, error)
}

// GetRooter resolves the storage's root object (9.0 §6).
type GetRooter interface {
	GetRoot(ctx context.Context, storageID string) (Obj, error)
}

// Other carries an opaque, driver-specific request/response (v3 Other's
// interface{} payload becomes json.RawMessage across the boundary, 9.0 §6).
type Other interface {
	Other(ctx context.Context, obj ObjRef, method string, data json.RawMessage) (json.RawMessage, error)
}

// Quotaer reports a network drive's space usage (9.0 §6b).
type Quotaer interface {
	Quota(ctx context.Context, storageID string) (Quota, error)
}

type Quota struct {
	Total int64 `json:"total"` // bytes; -1 = unknown/unlimited
	Used  int64 `json:"used"`  // bytes; -1 = unknown
	Free  int64 `json:"free"`  // bytes; -1 = unknown
}

// RemoteDownloader submits a remote resource to the network drive's own cloud
// download queue — bytes never pass through AList (9.0 §6b).
type RemoteDownloader interface {
	SupportedSchemes() []string
	AddRemoteDownload(ctx context.Context, dstDir ObjRef, url string, opts map[string]any) (RemoteTask, error)
	RemoteDownloadStatus(ctx context.Context, storageID, taskID string) (RemoteTaskStatus, error)
	RemoteDownloadCancel(ctx context.Context, storageID, taskID string) error
}

type RemoteTask struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type RemoteTaskStatus struct {
	State    string  `json:"state"`    // pending|downloading|seeding|done|error
	Progress float64 `json:"progress"` // 0..1
	Speed    int64   `json:"speed"`    // bytes/s; -1 unknown
	Detail   string  `json:"detail"`
}

// ArchiveMeta is the serializable meta-info of an archive (9.0 §6). The tree
// structure, when present, is carried out-of-band by the host.
type ArchiveMeta struct {
	Comment   string `json:"comment,omitempty"`
	Encrypted bool   `json:"encrypted"`
}

// ArchiveReader is a driver's native archive handling (115/云盘). Distinct from
// the ArchiveTool Kind (9.0 §6). Extract returns a Link (§1 contract).
type ArchiveReader interface {
	GetArchiveMeta(ctx context.Context, obj ObjRef, password string) (ArchiveMeta, error)
	ListArchive(ctx context.Context, obj ObjRef, innerPath, password string) ([]Obj, error)
	Extract(ctx context.Context, obj ObjRef, innerPath, password string) (*Link, error)
}

type ArchiveGetter interface {
	ArchiveGet(ctx context.Context, obj ObjRef, innerPath, password string) (Obj, error)
}

type ArchiveDecompress interface {
	ArchiveDecompress(ctx context.Context, src, dstDir ObjRef, innerPath, password string, putIntoNewDir bool) ([]Obj, error)
}
