package grpcproto

// Handshake parameters for the AList storage-driver plugin protocol (Step 9.2,
// fixing the 9.0 §9 "later, non-blocking" small decisions). This file is the
// single source of truth for the magic cookie and protocol version; both the
// host launcher (internal/extension/grpc/host) and the plugin serve helper
// (ext/grpcplugin) read them from here.
const (
	// MagicCookieKey and MagicCookieValue are go-plugin's basic mutual
	// handshake: a plugin binary launched without the matching cookie in its
	// environment refuses to serve, so a stray exec of a plugin does nothing.
	MagicCookieKey   = "ALIST_PLUGIN"
	MagicCookieValue = "alist-plugin-core-v4"

	// ProtocolVersion is bumped on breaking wire changes; the host negotiates
	// supported versions at launch.
	ProtocolVersion = 1

	// PluginName is the go-plugin dispense key for a Kind=StorageDriver plugin.
	PluginName = "storage_driver"

	// AuthPluginName is the go-plugin dispense key for a Kind=AuthProvider
	// plugin (WS-8).
	AuthPluginName = "auth_provider"

	// AuthNPluginName is the go-plugin dispense key for a Kind=AuthNMethod
	// plugin (login methods: SSO/OIDC, LDAP, TOTP, WebAuthn). Distinct from
	// AuthPluginName — different wire service (AuthNMethodService) and a
	// different trust surface (asserts user identity, never issues a token).
	AuthNPluginName = "authn_method"

	// BgServicePluginName is the go-plugin dispense key for a
	// Kind=BackgroundService plugin: a long-lived host-managed task (FRP tunnel,
	// scheduled sync, watcher) served over BackgroundServiceService.
	BgServicePluginName = "background_service"

	// HostCapProbePluginName is the go-plugin dispense key for the F0 host
	// capability probe fixture (internal/extension/grpc/testdata/hostcapplugin):
	// a plugin that serves HostCapProbeService and dials the reverse HostService
	// bridge to exercise borrowed Host capabilities in an integration test.
	HostCapProbePluginName = "hostcap_probe"

	// APIRoutePluginName is the go-plugin dispense key for a Kind=APIRoute
	// plugin (W8a): sub-routes a plugin mounts (webhook / share API / public
	// page) served over APIRouteService.
	APIRoutePluginName = "api_route"

	// ActionPluginName is the go-plugin dispense key for a Kind=Action plugin
	// (W8b): the menu actions a plugin declared, executed on the user's behalf
	// over ActionService.
	ActionPluginName = "action"

	// AssistPluginName is the go-plugin dispense key for the assist auxiliary
	// service (SDK §4.1): any Kind's plugin whose impl also implements
	// ext.Assist rides this dispense name alongside its primary Kind's
	// dispense name (e.g. "action"), served over AssistService.
	AssistPluginName = "assist"

	// EventHookPluginName is the go-plugin dispense key for a Kind=EventHook
	// plugin (W7): a notification sink for host fs events, served over
	// EventHookService.
	EventHookPluginName = "event_hook"

	// ScheduledJobPluginName is the go-plugin dispense key for a
	// Kind=ScheduledJob plugin (W7): a time-driven background task, served
	// over ScheduledJobService.
	ScheduledJobPluginName = "scheduled_job"

	// NotificationPluginName is the go-plugin dispense key for a
	// Kind=NotificationChannel plugin (W7b): a notification sink the host fans
	// notices out to (email/webhook/Telegram/...), served over
	// NotificationService.
	NotificationPluginName = "notification"

	// FileDecoratorPluginName is the go-plugin dispense key for a
	// Kind=FileDecorator plugin (W9a): the extension field group(s) behind a
	// FileColumn contribution (label badges, share markers), served over
	// FileDecoratorService.
	FileDecoratorPluginName = "file_decorator"

	// AccessProtocolPluginName is the go-plugin dispense key for a
	// Kind=AccessProtocol plugin's TransportHTTP half (W9a): WebDAV/S3/MCP
	// served over AccessProtocolService. TransportConn (FTP/SFTP) protocols
	// never dispense under this name — they are native/in-process only (design
	// §5) and do not cross the gRPC boundary.
	AccessProtocolPluginName = "access_protocol"

	// OfflineDownloadToolPluginName is the go-plugin dispense key for a
	// Kind=OfflineDownloadTool plugin (Phase E ring 1): an external download
	// daemon backend (aria2/qBittorrent/Transmission-alike) the offline_download
	// base hands a URL to and polls by gid, served over
	// OfflineDownloadToolService.
	OfflineDownloadToolPluginName = "offline_download_tool"

	// SearchIndexerPluginName is the go-plugin dispense key for a
	// Kind=SearchIndexer plugin (Phase E ring 1b): a real-index file-search
	// backend (database/bleve/meilisearch-alike) the `search` base builds and
	// queries, served over SearchIndexerService.
	SearchIndexerPluginName = "search_indexer"

	// ArchiveToolPluginName is the go-plugin dispense key for a
	// Kind=ArchiveTool plugin (Phase E wave 2): an archive format
	// internal/archive's dispatch layer looks inside (rar/7z/... alongside the
	// built-in zip tool), served over ArchiveToolService.
	ArchiveToolPluginName = "archive_tool"

	// ChunkSize is the byte-slice size for OpenRange streaming and Put ReadAt
	// round-trips: within gRPC's default 4 MiB frame cap, large enough to amortize
	// per-RPC overhead, small enough to bound memory.
	ChunkSize = 64 * 1024
)
