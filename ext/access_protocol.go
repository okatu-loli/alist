package ext

import (
	"context"
	"net"
)

// AccessProtocol exposes the AList filesystem OVER an inbound network protocol
// (design v4-plugin-extensions-design.md §2.1). It is the common floor beneath
// WebDAV / S3 / MCP (served over HTTP) and FTP / SFTP (raw byte streams). It is
// orthogonal to StorageDriver: a StorageDriver connects OUT to some cloud, an
// AccessProtocol serves the AList tree IN to a client.
//
// The one iron security rule (design §0, §2.1): a protocol plugin NEVER makes an
// authorization decision. It only turns an inbound credential into a host-issued
// Principal via host.Auth().Login / VerifyToken / VerifyAppPassword, then does
// every filesystem operation through host.FS() with that Principal — and host.FS
// runs the core Authorize on every single call. So the v3 defects this fixes
// (WebDAV's per-operation re-check, S3 server auth) are satisfied structurally:
// the decision lives in the host, not in the plugin. A protocol plugin that
// tried to bypass Auth would only ever hold the empty Principal, which is its own
// narrow bound grant — it cannot mint a session or impersonate a user.
//
// Because it owns sockets and speaks wire protocols it is native/official tier
// only (design §5): it is never sandboxed to WASM, and the raw-socket transport
// (FTP/SFTP) does not cross the gRPC boundary at all (see Transport).
type AccessProtocol interface {
	Extension
	// ProtocolInfo describes how the host binds this protocol: its scheme, its
	// transport, the default port, and — for TransportHTTP — where the host mounts
	// the handler.
	ProtocolInfo() ProtocolInfo

	// ServeHTTP handles ONE inbound HTTP request for a TransportHTTP protocol
	// (WebDAV/S3/MCP). The host proxies the client's request here after mounting
	// the protocol at ProtocolInfo.MountPattern; the plugin authenticates it via
	// host.Auth() and performs every fs operation via host.FS(). The request and
	// response are the serializable HTTPRequest/HTTPResponse (the frozen wire
	// contract, same as APIRoute) so the same handler works in-process or over
	// gRPC. Only meaningful for TransportHTTP; a TransportConn plugin returns
	// ErrCodeNotImplement.
	ServeHTTP(ctx context.Context, req *HTTPRequest) (*HTTPResponse, error)

	// HandleConn services ONE accepted raw connection for a TransportConn protocol
	// (FTP/SFTP). The host owns the listener (port/TLS from settings) and hands the
	// plugin a live net.Conn; the plugin drives the wire protocol and, per command,
	// authenticates via host.Auth() and acts via host.FS(). HandleConn must return
	// when the connection closes or ctx is cancelled (host shutdown).
	//
	// NATIVE ONLY: a raw net.Conn is not serializable, so a TransportConn plugin
	// runs in-process (or as a trusted official binary), never behind the gRPC
	// bridge. The trust requirement is the point — a plugin holding a bare socket
	// is outside the capability sandbox for that connection's bytes, so only signed
	// official / built-in code provides one (design §5, §2.1 decision 4). Only
	// meaningful for TransportConn; a TransportHTTP plugin returns
	// ErrCodeNotImplement.
	HandleConn(ctx context.Context, conn net.Conn) error
}

// ProtocolInfo describes an AccessProtocol's binding to the host.
type ProtocolInfo struct {
	// Scheme is the protocol name: "webdav" | "s3" | "mcp" | "ftp" | "sftp".
	Scheme string `json:"scheme"`
	// Transport selects how the host feeds the plugin traffic.
	Transport Transport `json:"transport"`
	// DefaultPort is the port the host listens on for a TransportConn protocol
	// when the admin has not overridden it in settings. Ignored for TransportHTTP
	// (which shares the host's HTTP listener under MountPattern).
	DefaultPort int `json:"default_port,omitempty"`
	// MountPattern is where the host mounts a TransportHTTP handler, e.g. "/dav"
	// or "/s3". It is checked against the reserved-prefix table at
	// install/registration. Ignored for TransportConn.
	MountPattern string `json:"mount_pattern,omitempty"`
}

// Transport is how a protocol plugin receives inbound traffic.
type Transport uint8

const (
	// TransportHTTP: the host proxies inbound HTTP requests to ServeHTTP. Works
	// in-process or cross-process (gRPC). Used by WebDAV, S3, MCP.
	TransportHTTP Transport = iota
	// TransportConn: the host owns a listener and hands each accepted net.Conn to
	// HandleConn. Raw byte streams (FTP/SFTP); native/in-process only in v4 (a raw
	// conn does not cross the gRPC boundary).
	TransportConn
)

// String renders a Transport for logs / diagnostics.
func (t Transport) String() string {
	switch t {
	case TransportHTTP:
		return "http"
	case TransportConn:
		return "conn"
	default:
		return "unknown"
	}
}
