package ext

import (
	"context"
	"net/http"
	"time"
)

// This file expands the Host capability surface (design docs
// v4-plugin-extensions-design.md §3): the set of core "underlying" abilities a
// plugin borrows through the Host callback instead of touching storage / db /
// signing keys / auth decisions directly (§0's iron rule). Every capability is
// gated at the host boundary by the plugin's runtime Grant (§6.3); a plugin
// that was not granted a capability sees ErrPermissionDenied from the
// corresponding Host method.
//
// The Host interface itself is declared in extension.go (Logger stays there for
// backward compatibility); this file adds the FS/Auth/Store/Sign/HTTP/Cache
// sub-interfaces and their serializable DTOs.

// Principal (declared in authn_method.go) is the opaque, host-issued identity
// handle the FS/Auth capabilities key off of. The empty Principal ("") means
// "the plugin's own bound principal": the restricted identity the host granted
// the plugin at Init time (built from the plugin's Grant). Protocol plugins that
// authenticate an inbound user exchange its credentials for a fresh Principal
// via Auth().

// FS is the host filesystem borrowed by a plugin. EVERY method runs the core
// Authorize against the acting principal before touching storage (design §0,
// §3): borrowing FS never bypasses authorization. The plugin only ever holds
// opaque ObjRefs (pillar A) — never a mutable virtual-path key it could
// fabricate to reach outside its grant. Even a fabricated ref is backstopped by
// the per-call Authorize.
type FS interface {
	// Resolve maps a virtual path to an ObjRef. It is authorized (read) so a
	// plugin cannot probe existence outside its grant. This is the per-request
	// hot path for protocol plugins that live in the path world (WebDAV/S3/FTP).
	Resolve(ctx context.Context, p Principal, path string) (ObjRef, error)
	List(ctx context.Context, p Principal, dir ObjRef, a ListArgs) ([]Obj, error)
	Get(ctx context.Context, p Principal, ref ObjRef) (Obj, error)
	Link(ctx context.Context, p Principal, ref ObjRef, a LinkArgs) (*Link, error)
	Mkdir(ctx context.Context, p Principal, parent ObjRef, name string) (Obj, error)
	Move(ctx context.Context, p Principal, src, dstDir ObjRef) error
	Rename(ctx context.Context, p Principal, src ObjRef, name string) error
	Copy(ctx context.Context, p Principal, src, dstDir ObjRef) error
	Remove(ctx context.Context, p Principal, ref ObjRef) error
	Put(ctx context.Context, p Principal, dstDir ObjRef, up UploadStream) (Obj, error)
}

// Auth lets a protocol plugin turn an inbound credential into a Principal (the
// host runs rate-limiting / brute-force protection at this choke point) and
// verify tokens. Token ISSUANCE always stays in the core (design §7 decision 3):
// a plugin only proves "this credential is valid", the host decides what to
// mint.
type Auth interface {
	// Login resolves a username/password to a Principal. The host applies
	// login rate-limiting here. For a 2FA-enabled account it returns
	// ErrNeedAppPassword, forcing protocol clients onto an app password rather
	// than letting a protocol login bypass the second factor.
	Login(ctx context.Context, username, password string) (Principal, error)
	// VerifyToken resolves a bearer/session token to a Principal.
	VerifyToken(ctx context.Context, token string) (Principal, error)
	// VerifyAppPassword resolves a per-client app password to a Principal.
	VerifyAppPassword(ctx context.Context, username, appPassword string) (Principal, error)
}

// ErrNeedAppPassword is returned by Auth.Login for an account with a second
// factor enabled: a protocol client cannot enter a TOTP code, so it must use an
// app password instead of the raw account password (design §2.1).
var ErrNeedAppPassword = NewError(ErrCodePermissionDenied, "account requires an app password for protocol login")

// Store is a plugin's private, host-managed persistence, namespaced by plugin
// id so plugin A can never read plugin B's data and no plugin can touch a core
// table (design §3). F0 exposes the KV half; a manifest-declared, host-
// migrated RELATIONAL schema (SQL-backed tables) remains a later extension —
// Table() here is the honest, schemaless intermediate: named tables of
// JSON-document rows addressed by a host-assigned primary key, layered over
// the same store backend as KV().
type Store interface {
	KV() KV
	// Secrets returns a KV namespace whose values are encrypted at rest
	// (AES-256-GCM) on the host side — for a plugin holding OAuth tokens or API
	// keys it should not persist in the plaintext KV. It shares KV's shape and
	// per-plugin namespacing/grant gate, but its storage extent is disjoint from
	// KV()'s: a key set through Secrets() is never visible through KV() (and
	// vice versa), even when both hold the same key string.
	Secrets() KV
	// Table returns a named, schemaless document table: JSON rows addressed by
	// a host-assigned primary key. Unlike Secrets (a fully disjoint storage
	// extent), Table is a structured VIEW over the same backend as KV() —
	// isolation between tables (and from plain KV keys) is by key-prefix
	// design (a reserved "table:" prefix), not a separate storage extent. name
	// must not be empty and must not contain ':' (rejected — see the hostcap
	// implementation's escaping scheme).
	Table(name string) Table
}

// TableRow is one row returned by Table.List: its host-assigned primary key
// paired with the JSON document stored at it.
type TableRow struct {
	ID  string
	Row map[string]any
}

// Table is a per-plugin, schemaless document table: named rows of arbitrary
// JSON data addressed by a primary key the host assigns on Insert. This is
// the F0 realization of "structured storage beyond raw KV" — no SQL engine,
// no manifest-declared schema (that richer relational form is a later
// extension); just PK-addressed JSON rows with list/scan, namespaced by
// plugin id and table name on the host side.
type Table interface {
	// Insert stores row as a new document and returns the host-assigned id
	// (a random, unique token — never derived from a counter or wall clock).
	Insert(ctx context.Context, row map[string]any) (id string, err error)
	// Get returns the row at id, or ok=false if no such row exists in this
	// table.
	Get(ctx context.Context, id string) (row map[string]any, ok bool, err error)
	// Update replaces the row at id. It is error-if-missing (not an upsert):
	// updating a nonexistent id returns an error rather than silently
	// creating it, so a caller's Update always means "I believe this row
	// already exists" — Insert is the only way to mint a new id.
	Update(ctx context.Context, id string, row map[string]any) error
	// Delete removes the row at id. Deleting a nonexistent id is a no-op
	// (mirrors KV.Delete's semantics).
	Delete(ctx context.Context, id string) error
	// List returns every row in this table (id + row), scanning the table's
	// key-prefix range. Order is unspecified.
	List(ctx context.Context) ([]TableRow, error)
}

// KV is a namespaced key/value store. All keys are implicitly scoped to the
// calling plugin's id on the host side — the plugin cannot address another
// plugin's namespace.
type KV interface {
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)
	Set(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	// List returns the keys under prefix (all keys when prefix is "").
	List(ctx context.Context, prefix string) ([]string, error)
}

// SignScope is the permitted use of a signed link.
type SignScope string

const (
	// SignScopeDownload authorizes fetching the referenced object's bytes.
	SignScopeDownload SignScope = "download"
)

// Signer issues signed, ID-anchored, expiring links reusing the core key set —
// the plugin never sees the key (design §3). A link is anchored to an ObjRef the
// plugin has access to; SignLink authorizes the ref before signing so a plugin
// cannot mint a link to an object outside its grant (invariant 4).
type Signer interface {
	SignLink(ctx context.Context, p Principal, ref ObjRef, scope SignScope, ttl time.Duration) (token string, err error)
	VerifyLink(ctx context.Context, token string) (ref ObjRef, scope SignScope, err error)
}

// HTTPRequest is the serializable outbound request a plugin asks the host to
// make (design §3, §6.3). The host enforces the network allowlist (global
// outbound policy ∩ the plugin's Grant.Network) before dialing; a host not on
// the allowlist yields ErrPermissionDenied.
type HTTPRequest struct {
	Method string      `json:"method"`
	URL    string      `json:"url"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// HTTPResponse is the serializable response for a HTTPClient.Do call.
type HTTPResponse struct {
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// HTTPClient is the host-gated outbound HTTP capability. Do rejects any request
// whose target host is not on the effective allowlist (Grant.Network) before
// the request leaves the process.
type HTTPClient interface {
	Do(ctx context.Context, req *HTTPRequest) (*HTTPResponse, error)
}

// Cache is a per-plugin in-memory cache, namespaced by plugin id.
type Cache interface {
	Get(key string) (value []byte, ok bool)
	Set(key string, value []byte, ttl time.Duration)
	Delete(key string)
}
