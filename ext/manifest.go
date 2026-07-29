package ext

// Kind is an extension point. An extension declares one or more Kinds it
// implements (SDK doc §1). StorageDriver is the only Kind with a live
// implementation in 9.1; the rest are reserved so their identifiers are frozen
// alongside the rest of the contract.
type Kind string

const (
	KindStorageDriver Kind = "StorageDriver"
	// KindAuthProvider brokers a storage vendor's OAuth authorization-code
	// exchange (ext.AuthProvider): it turns a code into a storage *credential*,
	// with the client_id/secret exchange happening inside the plugin process.
	KindAuthProvider Kind = "AuthProvider"
	// KindAuthNMethod adds a user login method (ext.AuthNMethod): SSO/OIDC, LDAP,
	// TOTP, WebAuthn. It authenticates an AList *user* and, on success, lets the
	// host mint a session — the plugin never signs a token. This is a different
	// axis from KindAuthProvider (storage credential vs user session); the two
	// share no interface or code path. Login methods are native/official tier
	// only (design §5): closed-source third parties can never provide one.
	KindAuthNMethod         Kind = "AuthNMethod"
	KindOfflineDownloadTool Kind = "OfflineDownloadTool"
	KindArchiveTool         Kind = "ArchiveTool"
	KindSearchIndexer       Kind = "SearchIndexer"
	KindThumbnailer         Kind = "Thumbnailer"
	KindFileTransformer     Kind = "FileTransformer"
	KindMetadataProvider    Kind = "MetadataProvider"
	KindEventHook           Kind = "EventHook"
	KindScheduledJob        Kind = "ScheduledJob"
	KindNotification        Kind = "NotificationChannel"
	KindAction              Kind = "Action"
	KindPreviewer           Kind = "Previewer"
	KindUIPanel             Kind = "UIPanel"
	KindAPIRoute            Kind = "APIRoute"
	// KindAccessProtocol exposes the AList filesystem over an inbound network
	// protocol (ext.AccessProtocol): WebDAV/S3/MCP (TransportHTTP) or FTP/SFTP
	// (TransportConn). Orthogonal to StorageDriver — inbound vs outbound. Runs
	// native/official only (design §5): it owns sockets / speaks wire protocols,
	// so it never runs in a WASM sandbox.
	KindAccessProtocol Kind = "AccessProtocol"
	// KindBackgroundService is a long-running side task started after boot
	// (ext.BackgroundService): frp tunnel, a sync engine. The host supervises it
	// (restart/backoff, panic isolation) and polls Status() for the frontend
	// status card.
	KindBackgroundService Kind = "BackgroundService"
	// KindFeatureBundle is a composite feature (ext.FeatureBundle): share/labels.
	// It contributes several existing extension points at once (APIRoute +
	// PublicPage + EventHook + ScheduledJob) plus host Store/Sign, which the host
	// expands onto the F1 wiring points rather than inventing a new one.
	KindFeatureBundle Kind = "FeatureBundle"
	// KindFileDecorator supplies extension field groups for the file listing
	// (ext.FileDecorator): label badges, share markers. The host calls it in
	// parallel under a per-group time budget; a slow group is marked pending and
	// backfilled, never blocking the base listing (design §2.6).
	KindFileDecorator Kind = "FileDecorator"
	// KindFeature is a plain, host-registered core feature turned into a
	// manageable built-in extension (Project A-2): offline download, file search,
	// sharing, SSO login. Unlike FeatureBundle it contributes no interface and is
	// not expanded onto any wiring point — it is a pure registry entry that gives
	// an existing, natively-wired core feature a manageable/gate-able/aggregatable
	// identity. The host registers it enabled at boot (RegisterBuiltinExtension);
	// an admin may turn it off, and a RequireExtensionEnabled gate on the feature's
	// entry-point routes turns that off into a real 403 boundary.
	KindFeature Kind = "Feature"
)

// Capability names an optional interface a StorageDriver may implement. It is
// orthogonal to Kind (9.0 §4). The host validates that a driver's declared
// Capabilities are a subset of what it actually implements, and — with gRPC
// (9.2) — dispatches from the declaration because a single proxy struct cannot
// express "has Move but not Copy" through Go type assertions.
type Capability string

const (
	CapMkdir  Capability = "Mkdir"
	CapMove   Capability = "Move"
	CapRename Capability = "Rename"
	CapCopy   Capability = "Copy"
	CapRemove Capability = "Remove"
	CapPut    Capability = "Put"
	CapPutURL Capability = "PutURL"
	// CapPutRapid marks driver.PutRapid (SP-H 秒传: hash-based instant upload,
	// no bytes transferred on hit).
	CapPutRapid          Capability = "PutRapid"
	CapGetter            Capability = "Getter"
	CapGetRooter         Capability = "GetRooter"
	CapOther             Capability = "Other"
	CapRangeOpener       Capability = "RangeOpener"
	CapArchiveReader     Capability = "ArchiveReader"
	CapArchiveGetter     Capability = "ArchiveGetter"
	CapArchiveDecompress Capability = "ArchiveDecompress"
	CapQuota             Capability = "Quota"
	CapRemoteDownload    Capability = "RemoteDownload"
)

// Manifest is the contract root of an extension, distributed with it as
// plugin.json (SDK doc §1).
type Manifest struct {
	ID           string            `json:"id"`       // globally unique, reverse-domain: com.aliyun.drive_open
	Name         map[string]string `json:"name"`     // i18n display name
	Version      string            `json:"version"`  // semver
	CoreAPI      string            `json:"core_api"` // semver constraint on the host core API (0.1.0 until GA), e.g. "^0.1.0"
	Platforms    []string          `json:"platforms,omitempty"`
	Kinds        []Kind            `json:"kinds"`
	Capabilities []Capability      `json:"capabilities,omitempty"` // optional interfaces implemented; validated at registration
	Provenance   Provenance        `json:"provenance"`
	Permissions  Permissions       `json:"permissions"`
	// Wizard and Contributions are the Step 10 declarative front-end surface:
	// the install flow the host drives and the UI contributions it aggregates.
	// Both are optional — a plain storage driver leaves them zero.
	Wizard        Wizard        `json:"wizard,omitempty"`
	Contributions Contributions `json:"contributions,omitempty"`
	// Info is an optional declarative storage-config form for StorageDriver
	// plugins. The host synthesizes the generic common fields itself; a plugin
	// only declares its driver-specific Additional items (and DriverConfig).
	// Without it, the "add storage" form for the plugin driver is empty.
	Info *Info `json:"info,omitempty"`
	// DisableWarning is the declarative replacement for a hardcoded front-end
	// confirm list (formerly CONFIRM_ON_DISABLE_IDS): an optional localized
	// (LangMap, en/zh) message shown to the admin BEFORE disabling this
	// extension, stating the real consequence of turning it off — e.g. share:
	// "关闭后所有公开分享链接将立即失效" / "Disabling this immediately invalidates all
	// public share links". A plugin or built-in with genuine disable
	// consequences declares the warning copy here; empty/absent means the
	// extension disables without a special confirm.
	DisableWarning map[string]string `json:"disable_warning,omitempty"`
	// Signature and Checksum are reserved and currently unused: 9.3 package
	// verification reads the sidecar checksums.txt + signature files instead.
	Signature string `json:"signature,omitempty"`
	Checksum  string `json:"checksum,omitempty"`
}

// Provenance decides the trust tier and execution mode (SDK doc §1).
type Provenance struct {
	Source    string `json:"source"`     // official | verified-community | third-party
	License   string `json:"license"`    // SPDX id or "proprietary"
	SourceURL string `json:"source_url"` // source repo; empty => treated as closed source
	Pricing   string `json:"pricing"`    // free | paid
}

// Permissions is a declarative permission grant, shown to the admin at
// install time for informed consent (SDK doc §1). StorageTokens is enforced
// when scoped tokens are issued (IssueScopedToken); the rest is informational
// — every plugin runs as a full-permission process (plugin parity).
type Permissions struct {
	Network       []string `json:"network,omitempty"`        // allowed host globs
	StorageTokens []string `json:"storage_tokens,omitempty"` // "self" or provider ids
	Filesystem    string   `json:"filesystem,omitempty"`     // none | temp | full
	HostAPIs      []string `json:"host_apis,omitempty"`      // requested Host methods
}
