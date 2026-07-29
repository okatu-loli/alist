package grpcplugin

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"google.golang.org/grpc"
)

// hostClient is the PLUGIN-side ext.Host backed by a gRPC connection to the
// host-served HostService (the reverse bridge, F0). A subprocess plugin obtains
// it by dialing the broker id the host handed it (see ServeHostCapProbe), then
// uses host.FS()/Store()/HTTP()/Auth()/Sign() exactly as an in-process plugin
// would — the calls travel back to the host, which enforces the plugin's Grant
// and runs Authorize.
//
// FS/Store/HTTP/Auth/Sign are all carried over the wire; Cache is
// process-local on the plugin side (design §3: it is not a shared resource, so
// there is nothing to bridge). Calling an un-bridged capability returns
// NOT_IMPLEMENT rather than silently succeeding — currently that only applies
// business-logic-side (e.g. hostcap's VerifyAppPassword until W3 builds the
// app-password store), never at the transport leg.
type hostClient struct {
	svc   pb.HostServiceClient
	cache *localCache
}

// NewHostClient builds a plugin-side ext.Host over an established gRPC
// connection to the host's HostService (dialed via the go-plugin broker).
func NewHostClient(conn *grpc.ClientConn) ext.Host {
	return &hostClient{svc: pb.NewHostServiceClient(conn), cache: newLocalCache()}
}

func (h *hostClient) Logger() ext.Logger   { return stderrLogger{} }
func (h *hostClient) FS() ext.FS           { return &hostFSClient{svc: h.svc} }
func (h *hostClient) Store() ext.Store     { return &hostStoreClient{svc: h.svc} }
func (h *hostClient) HTTP() ext.HTTPClient { return &hostHTTPClient{svc: h.svc} }
func (h *hostClient) Cache() ext.Cache     { return h.cache }
func (h *hostClient) Auth() ext.Auth       { return &hostAuthClient{svc: h.svc} }
func (h *hostClient) Sign() ext.Signer     { return &hostSignClient{svc: h.svc} }

func (h *hostClient) LoadConfig(ctx context.Context) ([]byte, error) {
	reply, err := h.svc.LoadConfig(ctx, &pb.LoadConfigRequest{})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return reply.Config, nil
}

func (h *hostClient) SaveConfig(ctx context.Context, cfg []byte) error {
	reply, err := h.svc.SaveConfig(ctx, &pb.SaveConfigRequest{Config: cfg})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

func (h *hostClient) Capabilities(ctx context.Context) ([]string, error) {
	reply, err := h.svc.Capabilities(ctx, &pb.CapabilitiesRequest{})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return reply.Apis, nil
}

// --- FS ---

type hostFSClient struct{ svc pb.HostServiceClient }

func (f *hostFSClient) Resolve(ctx context.Context, p ext.Principal, path string) (ext.ObjRef, error) {
	reply, err := f.svc.FSResolve(ctx, &pb.FSResolveRequest{Principal: string(p), Path: path})
	if err != nil {
		return ext.ObjRef{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.ObjRef{}, e
	}
	return pb.RefFromProto(reply.Ref), nil
}

func (f *hostFSClient) List(ctx context.Context, p ext.Principal, dir ext.ObjRef, a ext.ListArgs) ([]ext.Obj, error) {
	reply, err := f.svc.FSList(ctx, &pb.FSListRequest{Principal: string(p), Dir: pb.RefToProto(dir), Args: pb.ListArgsToProto(a)})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return pb.ObjsFromProto(reply.Objs), nil
}

func (f *hostFSClient) Get(ctx context.Context, p ext.Principal, ref ext.ObjRef) (ext.Obj, error) {
	reply, err := f.svc.FSGet(ctx, &pb.FSGetRequest{Principal: string(p), Ref: pb.RefToProto(ref)})
	if err != nil {
		return ext.Obj{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.Obj{}, e
	}
	return pb.ObjFromProto(reply.Obj), nil
}

func (f *hostFSClient) Link(ctx context.Context, p ext.Principal, ref ext.ObjRef, a ext.LinkArgs) (*ext.Link, error) {
	reply, err := f.svc.FSLink(ctx, &pb.FSLinkRequest{Principal: string(p), Ref: pb.RefToProto(ref), Args: pb.LinkArgsToProto(a)})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return pb.LinkFromProto(reply.Link), nil
}

func (f *hostFSClient) Mkdir(ctx context.Context, p ext.Principal, parent ext.ObjRef, name string) (ext.Obj, error) {
	reply, err := f.svc.FSMkdir(ctx, &pb.FSMkdirRequest{Principal: string(p), Parent: pb.RefToProto(parent), Name: name})
	if err != nil {
		return ext.Obj{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.Obj{}, e
	}
	return pb.ObjFromProto(reply.Obj), nil
}

func (f *hostFSClient) Move(ctx context.Context, p ext.Principal, src, dstDir ext.ObjRef) error {
	reply, err := f.svc.FSMove(ctx, &pb.FSMoveRequest{Principal: string(p), Src: pb.RefToProto(src), DstDir: pb.RefToProto(dstDir)})
	return errFromReply(reply, err)
}

func (f *hostFSClient) Rename(ctx context.Context, p ext.Principal, src ext.ObjRef, name string) error {
	reply, err := f.svc.FSRename(ctx, &pb.FSRenameRequest{Principal: string(p), Src: pb.RefToProto(src), NewName: name})
	return errFromReply(reply, err)
}

func (f *hostFSClient) Copy(ctx context.Context, p ext.Principal, src, dstDir ext.ObjRef) error {
	reply, err := f.svc.FSCopy(ctx, &pb.FSCopyRequest{Principal: string(p), Src: pb.RefToProto(src), DstDir: pb.RefToProto(dstDir)})
	return errFromReply(reply, err)
}

func (f *hostFSClient) Remove(ctx context.Context, p ext.Principal, ref ext.ObjRef) error {
	reply, err := f.svc.FSRemove(ctx, &pb.FSRemoveRequest{Principal: string(p), Ref: pb.RefToProto(ref)})
	return errFromReply(reply, err)
}

// Put streams up (which the plugin already holds — unlike the driver-side pull
// model, the plugin here is the one with the bytes) to the host over FSPut: an
// Init frame naming the principal/destination/metadata, then sequential Chunk
// frames read from up.ReadAt, mirroring how driverServer.OpenRange pushes
// Chunks (rather than the ReadRequest/ReadReply pull dance Put.go uses, which
// only makes sense when the RECEIVER lacks the bytes).
func (f *hostFSClient) Put(ctx context.Context, p ext.Principal, dstDir ext.ObjRef, up ext.UploadStream) (ext.Obj, error) {
	stream, err := f.svc.FSPut(ctx)
	if err != nil {
		return ext.Obj{}, err
	}
	info := up.Info()
	if err := stream.Send(&pb.FSPutClient{Msg: &pb.FSPutClient_Init{Init: &pb.FSPutInit{
		Principal: string(p),
		DstDir:    pb.RefToProto(dstDir),
		Info:      pb.UploadInfoToProto(info),
	}}}); err != nil {
		return ext.Obj{}, err
	}

	buf := make([]byte, pb.ChunkSize)
	var off int64
	for {
		n, rerr := up.ReadAt(buf, off)
		if n > 0 {
			chunk := &pb.Chunk{Data: append([]byte(nil), buf[:n]...)}
			if serr := stream.Send(&pb.FSPutClient{Msg: &pb.FSPutClient_Chunk{Chunk: chunk}}); serr != nil {
				return ext.Obj{}, serr
			}
			off += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_, _ = stream.CloseAndRecv()
			return ext.Obj{}, rerr
		}
	}

	reply, err := stream.CloseAndRecv()
	if err != nil {
		return ext.Obj{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.Obj{}, e
	}
	return pb.ObjFromProto(reply.Obj), nil
}

// --- Store ---

type hostStoreClient struct{ svc pb.HostServiceClient }

func (s *hostStoreClient) KV() ext.KV      { return &hostKVClient{svc: s.svc} }
func (s *hostStoreClient) Secrets() ext.KV { return &hostSecretsClient{svc: s.svc} }
func (s *hostStoreClient) Table(name string) ext.Table {
	return &hostTableClient{svc: s.svc, table: name}
}

type hostKVClient struct{ svc pb.HostServiceClient }

func (k *hostKVClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
	reply, err := k.svc.StoreGet(ctx, &pb.StoreGetRequest{Key: key})
	if err != nil {
		return nil, false, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, false, e
	}
	return reply.Value, reply.Found, nil
}

func (k *hostKVClient) Set(ctx context.Context, key string, value []byte) error {
	reply, err := k.svc.StoreSet(ctx, &pb.StoreSetRequest{Key: key, Value: value})
	return errFromReply(reply, err)
}

func (k *hostKVClient) Delete(ctx context.Context, key string) error {
	reply, err := k.svc.StoreDelete(ctx, &pb.StoreDeleteRequest{Key: key})
	return errFromReply(reply, err)
}

func (k *hostKVClient) List(ctx context.Context, prefix string) ([]string, error) {
	reply, err := k.svc.StoreList(ctx, &pb.StoreListRequest{Prefix: prefix})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return reply.Keys, nil
}

// hostSecretsClient mirrors hostKVClient exactly, calling the Secret* RPCs
// instead of Store* — the host encrypts/decrypts on its side, so the wire
// shape (and this client) is identical to plaintext KV.
type hostSecretsClient struct{ svc pb.HostServiceClient }

func (k *hostSecretsClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
	reply, err := k.svc.SecretGet(ctx, &pb.SecretGetRequest{Key: key})
	if err != nil {
		return nil, false, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, false, e
	}
	return reply.Value, reply.Found, nil
}

func (k *hostSecretsClient) Set(ctx context.Context, key string, value []byte) error {
	reply, err := k.svc.SecretSet(ctx, &pb.SecretSetRequest{Key: key, Value: value})
	return errFromReply(reply, err)
}

func (k *hostSecretsClient) Delete(ctx context.Context, key string) error {
	reply, err := k.svc.SecretDelete(ctx, &pb.SecretDeleteRequest{Key: key})
	return errFromReply(reply, err)
}

func (k *hostSecretsClient) List(ctx context.Context, prefix string) ([]string, error) {
	reply, err := k.svc.SecretList(ctx, &pb.SecretListRequest{Prefix: prefix})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return reply.Keys, nil
}

// hostTableClient mirrors hostKVClient's shape, calling the Table* RPCs
// instead of Store* — bound to one table name, JSON-encoding rows over the
// wire (the host decodes into map[string]any and re-encodes on the way back;
// this leg only ever moves bytes).
type hostTableClient struct {
	svc   pb.HostServiceClient
	table string
}

func (t *hostTableClient) Insert(ctx context.Context, row map[string]any) (string, error) {
	data, err := json.Marshal(row)
	if err != nil {
		return "", err
	}
	reply, err := t.svc.TableInsert(ctx, &pb.TableInsertRequest{Table: t.table, Row: data})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return "", e
	}
	return reply.Id, nil
}

func (t *hostTableClient) Get(ctx context.Context, id string) (map[string]any, bool, error) {
	reply, err := t.svc.TableGet(ctx, &pb.TableGetRequest{Table: t.table, Id: id})
	if err != nil {
		return nil, false, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, false, e
	}
	if !reply.Found {
		return nil, false, nil
	}
	row := map[string]any{}
	if err := json.Unmarshal(reply.Row, &row); err != nil {
		return nil, false, err
	}
	return row, true, nil
}

func (t *hostTableClient) Update(ctx context.Context, id string, row map[string]any) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	reply, err := t.svc.TableUpdate(ctx, &pb.TableUpdateRequest{Table: t.table, Id: id, Row: data})
	return errFromReply(reply, err)
}

func (t *hostTableClient) Delete(ctx context.Context, id string) error {
	reply, err := t.svc.TableDelete(ctx, &pb.TableDeleteRequest{Table: t.table, Id: id})
	return errFromReply(reply, err)
}

func (t *hostTableClient) List(ctx context.Context) ([]ext.TableRow, error) {
	reply, err := t.svc.TableList(ctx, &pb.TableListRequest{Table: t.table})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	rows := make([]ext.TableRow, 0, len(reply.Rows))
	for _, r := range reply.Rows {
		row := map[string]any{}
		if err := json.Unmarshal(r.Row, &row); err != nil {
			return nil, err
		}
		rows = append(rows, ext.TableRow{ID: r.Id, Row: row})
	}
	return rows, nil
}

// --- HTTP ---

type hostHTTPClient struct{ svc pb.HostServiceClient }

func (h *hostHTTPClient) Do(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	reply, err := h.svc.HTTPDo(ctx, &pb.HTTPDoRequest{
		Method: req.Method,
		Url:    req.URL,
		Header: pb.HeaderToProto(req.Header),
		Body:   req.Body,
	})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	return &ext.HTTPResponse{Status: int(reply.Status), Header: pb.HeaderFromProto(reply.Header), Body: reply.Body}, nil
}

// --- local Cache (plugin process) ---

type localCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}
type cacheEntry struct {
	value   []byte
	expires time.Time
}

func newLocalCache() *localCache { return &localCache{m: map[string]cacheEntry{}} }

func (c *localCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		delete(c.m, key)
		return nil, false
	}
	return e.value, true
}

func (c *localCache) Set(key string, value []byte, ttl time.Duration) {
	e := cacheEntry{value: value}
	if ttl > 0 {
		e.expires = time.Now().Add(ttl)
	}
	c.mu.Lock()
	c.m[key] = e
	c.mu.Unlock()
}

func (c *localCache) Delete(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// --- Auth ---

type hostAuthClient struct{ svc pb.HostServiceClient }

func (a *hostAuthClient) Login(ctx context.Context, username, password string) (ext.Principal, error) {
	reply, err := a.svc.AuthLogin(ctx, &pb.AuthLoginRequest{Username: username, Password: password})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return "", e
	}
	return ext.Principal(reply.Principal), nil
}

func (a *hostAuthClient) VerifyToken(ctx context.Context, token string) (ext.Principal, error) {
	reply, err := a.svc.AuthVerifyToken(ctx, &pb.AuthVerifyTokenRequest{Token: token})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return "", e
	}
	return ext.Principal(reply.Principal), nil
}

func (a *hostAuthClient) VerifyAppPassword(ctx context.Context, username, appPassword string) (ext.Principal, error) {
	reply, err := a.svc.AuthVerifyAppPassword(ctx, &pb.AuthVerifyAppPasswordRequest{Username: username, AppPassword: appPassword})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return "", e
	}
	return ext.Principal(reply.Principal), nil
}

// --- Sign ---

type hostSignClient struct{ svc pb.HostServiceClient }

func (s *hostSignClient) SignLink(ctx context.Context, p ext.Principal, ref ext.ObjRef, scope ext.SignScope, ttl time.Duration) (string, error) {
	var ttlMs int64
	if ttl > 0 {
		ttlMs = ttl.Milliseconds()
	}
	reply, err := s.svc.SignLink(ctx, &pb.SignLinkRequest{Principal: string(p), Ref: pb.RefToProto(ref), Scope: string(scope), TtlMs: ttlMs})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return "", e
	}
	return reply.Token, nil
}

func (s *hostSignClient) VerifyLink(ctx context.Context, token string) (ext.ObjRef, ext.SignScope, error) {
	reply, err := s.svc.SignVerifyLink(ctx, &pb.SignVerifyLinkRequest{Token: token})
	if err != nil {
		return ext.ObjRef{}, "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.ObjRef{}, "", e
	}
	return pb.RefFromProto(reply.Ref), ext.SignScope(reply.Scope), nil
}

type stderrLogger struct{}

func (stderrLogger) Debugf(f string, a ...any) { log.Printf("[plugin][debug] "+f, a...) }
func (stderrLogger) Infof(f string, a ...any)  { log.Printf("[plugin][info] "+f, a...) }
func (stderrLogger) Warnf(f string, a ...any)  { log.Printf("[plugin][warn] "+f, a...) }
func (stderrLogger) Errorf(f string, a ...any) { log.Printf("[plugin][error] "+f, a...) }

// errFromReply collapses a transport error + an ErrorReply into one error.
func errFromReply(reply *pb.ErrorReply, err error) error {
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}
