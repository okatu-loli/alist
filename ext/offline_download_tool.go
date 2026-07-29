package ext

import (
	"context"
)

// OfflineDownloadTool is the extension point behind offline download's daemon
// backends (design: 子项目 B). The v4 core ships a direct-download path
// (SimpleHttp: op.Put straight into the target storage) as the always-available
// base — SimpleHttp is NOT a tool, it is the offline_download base's built-in
// direct path. A OfflineDownloadTool adds an external download daemon that the
// base hands a URL to and then polls: aria2, qBittorrent and transmission each
// implement this interface as an independent built-in extension.
//
// Like ext.SearchIndexer and ext.AuthNMethod, the contract is defined here so an
// out-of-process (gRPC) third-party backend is a stable future seam; this wave
// wires only in-process TRUSTED instances the host itself authors
// (RegisterTrustedOfflineDownloadToolInstance on the host side). The v3 coupling
// to a concrete *DownloadTask is deliberately INVERTED into value DTOs (AddArgs
// / ToolStatus) so a tool never reaches back into the task/db layer — it only
// takes a URL to add and reports status by gid.
type OfflineDownloadTool interface {
	Extension

	// Config reports the tool's identity (Name, matched against the
	// offline_download_tool setting to resolve the active tool).
	Config() OfflineDownloadToolConfig

	// AddURL hands the daemon a URL to fetch into args.TempDir and returns the
	// daemon-side gid identifying the resulting download.
	AddURL(ctx context.Context, args AddArgs) (gid string, err error)

	// Remove cancels/removes the download identified by gid from the daemon.
	Remove(ctx context.Context, gid string) error

	// Status reports the current progress/completion of the download gid.
	Status(ctx context.Context, gid string) (ToolStatus, error)
}

// OfflineDownloadToolConfig is a tool's self-description. Name is the stable id
// the offline_download_tool setting selects on (e.g. "aria2", "qBittorrent",
// "Transmission").
type OfflineDownloadToolConfig struct {
	Name string `json:"name"`
}

// AddArgs parameterizes one AddURL call: the source URL, the host-provided
// temp directory the daemon should download into, and the id of the user the
// download runs on behalf of.
type AddArgs struct {
	URL     string `json:"url"`
	TempDir string `json:"temp_dir"`
	UID     string `json:"uid"`
}

// ToolStatus is a value snapshot of one download's state as reported by the
// daemon. NewGID follows a daemon that re-keys a download (e.g. aria2 promoting
// a metadata download to the real file); Completed signals the base to transfer
// the finished files into the target storage; Err carries a daemon-side failure
// message. Err is a string (not error) because this DTO crosses the gRPC
// boundary for an out-of-process tool (see grpcproto.OfflineDownloadToolService):
// an `error` value cannot itself round-trip over the wire, and a string keeps
// this shape simple where every other cross-boundary error uses a sibling
// ErrorProto/ErrorReply field instead — the empty string means "no error", same
// as a nil error would.
type ToolStatus struct {
	TotalBytes int64   `json:"total_bytes"`
	Progress   float64 `json:"progress"`
	NewGID     string  `json:"new_gid"`
	Completed  bool    `json:"completed"`
	Status     string  `json:"status"`
	Err        string  `json:"err,omitempty"`
}
