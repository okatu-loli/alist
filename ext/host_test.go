package ext

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// noopHost is a complete but inert Host: it exists to pin the expanded Host
// capability surface at compile time (a plugin author embedding it gets every
// accessor). If a capability method is added/removed from an interface below,
// this file stops compiling — the intended tripwire.
type noopHost struct{}

func (noopHost) Logger() Logger                                 { return nil }
func (noopHost) FS() FS                                         { return noopFS{} }
func (noopHost) Auth() Auth                                     { return noopAuth{} }
func (noopHost) Store() Store                                   { return noopStore{} }
func (noopHost) Sign() Signer                                   { return noopSigner{} }
func (noopHost) HTTP() HTTPClient                               { return noopHTTP{} }
func (noopHost) Cache() Cache                                   { return noopCache{} }
func (noopHost) LoadConfig(context.Context) ([]byte, error)     { return nil, nil }
func (noopHost) SaveConfig(context.Context, []byte) error       { return nil }
func (noopHost) Capabilities(context.Context) ([]string, error) { return nil, nil }

type noopFS struct{}

func (noopFS) Resolve(context.Context, Principal, string) (ObjRef, error)        { return ObjRef{}, nil }
func (noopFS) List(context.Context, Principal, ObjRef, ListArgs) ([]Obj, error)  { return nil, nil }
func (noopFS) Get(context.Context, Principal, ObjRef) (Obj, error)               { return Obj{}, nil }
func (noopFS) Link(context.Context, Principal, ObjRef, LinkArgs) (*Link, error)  { return nil, nil }
func (noopFS) Mkdir(context.Context, Principal, ObjRef, string) (Obj, error)     { return Obj{}, nil }
func (noopFS) Move(context.Context, Principal, ObjRef, ObjRef) error             { return nil }
func (noopFS) Rename(context.Context, Principal, ObjRef, string) error           { return nil }
func (noopFS) Copy(context.Context, Principal, ObjRef, ObjRef) error             { return nil }
func (noopFS) Remove(context.Context, Principal, ObjRef) error                   { return nil }
func (noopFS) Put(context.Context, Principal, ObjRef, UploadStream) (Obj, error) { return Obj{}, nil }

type noopAuth struct{}

func (noopAuth) Login(context.Context, string, string) (Principal, error)             { return "", nil }
func (noopAuth) VerifyToken(context.Context, string) (Principal, error)               { return "", nil }
func (noopAuth) VerifyAppPassword(context.Context, string, string) (Principal, error) { return "", nil }

type noopStore struct{}

func (noopStore) KV() KV             { return noopKV{} }
func (noopStore) Secrets() KV        { return noopKV{} }
func (noopStore) Table(string) Table { return noopTable{} }

type noopKV struct{}

func (noopKV) Get(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
func (noopKV) Set(context.Context, string, []byte) error         { return nil }
func (noopKV) Delete(context.Context, string) error              { return nil }
func (noopKV) List(context.Context, string) ([]string, error)    { return nil, nil }

type noopTable struct{}

func (noopTable) Insert(context.Context, map[string]any) (string, error) { return "", nil }
func (noopTable) Get(context.Context, string) (map[string]any, bool, error) {
	return nil, false, nil
}
func (noopTable) Update(context.Context, string, map[string]any) error { return nil }
func (noopTable) Delete(context.Context, string) error                 { return nil }
func (noopTable) List(context.Context) ([]TableRow, error)             { return nil, nil }

type noopSigner struct{}

func (noopSigner) SignLink(context.Context, Principal, ObjRef, SignScope, time.Duration) (string, error) {
	return "", nil
}
func (noopSigner) VerifyLink(context.Context, string) (ObjRef, SignScope, error) {
	return ObjRef{}, "", nil
}

type noopHTTP struct{}

func (noopHTTP) Do(context.Context, *HTTPRequest) (*HTTPResponse, error) { return nil, nil }

type noopCache struct{}

func (noopCache) Get(string) ([]byte, bool)         { return nil, false }
func (noopCache) Set(string, []byte, time.Duration) {}
func (noopCache) Delete(string)                     {}

var _ Host = noopHost{}

func TestHostCapabilitySurface(t *testing.T) {
	var h Host = noopHost{}
	// Every accessor is reachable and typed as its capability interface.
	var _ FS = h.FS()
	var _ Auth = h.Auth()
	var _ Store = h.Store()
	var _ Signer = h.Sign()
	var _ HTTPClient = h.HTTP()
	var _ Cache = h.Cache()
	var _ KV = h.Store().KV()
	var _ KV = h.Store().Secrets()
	var _ Table = h.Store().Table("t")
}

func TestPermissionDeniedSentinelCode(t *testing.T) {
	code, ok := CodeOf(ErrPermissionDenied)
	if !ok || code != ErrCodePermissionDenied {
		t.Fatalf("ErrPermissionDenied code = %q,%v; want %q,true", code, ok, ErrCodePermissionDenied)
	}
	code, ok = CodeOf(ErrNeedAppPassword)
	if !ok || code != ErrCodePermissionDenied {
		t.Fatalf("ErrNeedAppPassword code = %q,%v; want %q,true", code, ok, ErrCodePermissionDenied)
	}
}

func TestHTTPRequestDTOCarriesHeader(t *testing.T) {
	req := &HTTPRequest{Method: http.MethodGet, URL: "https://example.com", Header: http.Header{"X-A": {"1"}}}
	if req.Header.Get("X-A") != "1" {
		t.Fatal("header round-trip failed")
	}
}
