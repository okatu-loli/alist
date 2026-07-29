package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"google.golang.org/grpc"
)

// This file is the HOST side of the F0 reverse bridge: HostServiceServer
// forwards a plugin's borrowed-capability calls to an ext.Host. The ext.Host it
// wraps is constructed per plugin (bound to that plugin's id + Grant +
// restricted principal — see internal/extension/hostcap), so identity is a
// server-side fact and never a wire input. A plugin subprocess Dials this server
// over the go-plugin GRPCBroker; ServeHostCapability stands one up on a broker
// id.
//
// The host is the go-plugin CLIENT, so it obtains its GRPCBroker from the
// dispensed plugin client's GRPCClient(ctx, broker, conn) hook. See the probe
// wiring (ext/grpcplugin) and the integration test for how the broker id is
// exchanged (host NextId() + AcceptAndServe, plugin Dial).

// HostServiceServer implements the host-served HostService by delegating to an
// ext.Host. Every gate (Grant, Authorize, allowlist, namespace) lives inside
// that ext.Host, so this layer is a thin, faithful forwarder.
type HostServiceServer struct {
	pb.UnimplementedHostServiceServer
	host ext.Host
}

// NewServer builds a HostServiceServer delegating to host. Exposed so a caller
// (and the reverse-bridge receiver tests that drive a single RPC with a fake
// stream) can hold the server directly rather than only the broker factory.
func NewServer(host ext.Host) *HostServiceServer {
	return &HostServiceServer{host: host}
}

// NewHostServiceServer builds a broker server factory (the shape
// GRPCBroker.AcceptAndServe wants) that serves HostService backed by host.
func NewHostServiceServer(host ext.Host) func([]grpc.ServerOption) *grpc.Server {
	return func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(opts...)
		pb.RegisterHostServiceServer(s, NewServer(host))
		return s
	}
}

// --- FS ---

func (s *HostServiceServer) FSResolve(ctx context.Context, req *pb.FSResolveRequest) (*pb.FSResolveReply, error) {
	ref, err := s.host.FS().Resolve(ctx, ext.Principal(req.Principal), req.Path)
	if err != nil {
		return &pb.FSResolveReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.FSResolveReply{Ref: pb.RefToProto(ref)}, nil
}

func (s *HostServiceServer) FSList(ctx context.Context, req *pb.FSListRequest) (*pb.ListReply, error) {
	objs, err := s.host.FS().List(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Dir), pb.ListArgsFromProto(req.Args))
	if err != nil {
		return &pb.ListReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ListReply{Objs: pb.ObjsToProto(objs)}, nil
}

func (s *HostServiceServer) FSGet(ctx context.Context, req *pb.FSGetRequest) (*pb.ObjReply, error) {
	o, err := s.host.FS().Get(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Ref))
	if err != nil {
		return &pb.ObjReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ObjReply{Obj: pb.ObjToProto(o)}, nil
}

func (s *HostServiceServer) FSLink(ctx context.Context, req *pb.FSLinkRequest) (*pb.LinkReply, error) {
	l, err := s.host.FS().Link(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Ref), pb.LinkArgsFromProto(req.Args))
	if err != nil {
		return &pb.LinkReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.LinkReply{Link: pb.LinkToProto(l)}, nil
}

func (s *HostServiceServer) FSMkdir(ctx context.Context, req *pb.FSMkdirRequest) (*pb.ObjReply, error) {
	o, err := s.host.FS().Mkdir(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Parent), req.Name)
	if err != nil {
		return &pb.ObjReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ObjReply{Obj: pb.ObjToProto(o)}, nil
}

func (s *HostServiceServer) FSMove(ctx context.Context, req *pb.FSMoveRequest) (*pb.ErrorReply, error) {
	err := s.host.FS().Move(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Src), pb.RefFromProto(req.DstDir))
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) FSRename(ctx context.Context, req *pb.FSRenameRequest) (*pb.ErrorReply, error) {
	err := s.host.FS().Rename(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Src), req.NewName)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) FSCopy(ctx context.Context, req *pb.FSCopyRequest) (*pb.ErrorReply, error) {
	err := s.host.FS().Copy(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Src), pb.RefFromProto(req.DstDir))
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) FSRemove(ctx context.Context, req *pb.FSRemoveRequest) (*pb.ErrorReply, error) {
	err := s.host.FS().Remove(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Ref))
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

// --- Store ---

func (s *HostServiceServer) StoreGet(ctx context.Context, req *pb.StoreGetRequest) (*pb.StoreGetReply, error) {
	v, ok, err := s.host.Store().KV().Get(ctx, req.Key)
	if err != nil {
		return &pb.StoreGetReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.StoreGetReply{Value: v, Found: ok}, nil
}

func (s *HostServiceServer) StoreSet(ctx context.Context, req *pb.StoreSetRequest) (*pb.ErrorReply, error) {
	err := s.host.Store().KV().Set(ctx, req.Key, req.Value)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) StoreDelete(ctx context.Context, req *pb.StoreDeleteRequest) (*pb.ErrorReply, error) {
	err := s.host.Store().KV().Delete(ctx, req.Key)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) StoreList(ctx context.Context, req *pb.StoreListRequest) (*pb.StoreListReply, error) {
	keys, err := s.host.Store().KV().List(ctx, req.Prefix)
	if err != nil {
		return &pb.StoreListReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.StoreListReply{Keys: keys}, nil
}

// --- Secrets (encrypted-at-rest KV, same shape/gate as Store, disjoint storage) ---

func (s *HostServiceServer) SecretGet(ctx context.Context, req *pb.SecretGetRequest) (*pb.SecretGetReply, error) {
	v, ok, err := s.host.Store().Secrets().Get(ctx, req.Key)
	if err != nil {
		return &pb.SecretGetReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.SecretGetReply{Value: v, Found: ok}, nil
}

func (s *HostServiceServer) SecretSet(ctx context.Context, req *pb.SecretSetRequest) (*pb.ErrorReply, error) {
	err := s.host.Store().Secrets().Set(ctx, req.Key, req.Value)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) SecretDelete(ctx context.Context, req *pb.SecretDeleteRequest) (*pb.ErrorReply, error) {
	err := s.host.Store().Secrets().Delete(ctx, req.Key)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) SecretList(ctx context.Context, req *pb.SecretListRequest) (*pb.SecretListReply, error) {
	keys, err := s.host.Store().Secrets().List(ctx, req.Prefix)
	if err != nil {
		return &pb.SecretListReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.SecretListReply{Keys: keys}, nil
}

// --- Table (schemaless document tables, same gate/backend as Store) ---

func (s *HostServiceServer) TableInsert(ctx context.Context, req *pb.TableInsertRequest) (*pb.TableInsertReply, error) {
	row, err := tableRowFromProto(req.Row)
	if err != nil {
		return &pb.TableInsertReply{Error: pb.ErrorToProto(err)}, nil
	}
	id, err := s.host.Store().Table(req.Table).Insert(ctx, row)
	if err != nil {
		return &pb.TableInsertReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.TableInsertReply{Id: id}, nil
}

func (s *HostServiceServer) TableGet(ctx context.Context, req *pb.TableGetRequest) (*pb.TableGetReply, error) {
	row, ok, err := s.host.Store().Table(req.Table).Get(ctx, req.Id)
	if err != nil {
		return &pb.TableGetReply{Error: pb.ErrorToProto(err)}, nil
	}
	if !ok {
		return &pb.TableGetReply{Found: false}, nil
	}
	data, merr := tableRowToProto(row)
	if merr != nil {
		return &pb.TableGetReply{Error: pb.ErrorToProto(merr)}, nil
	}
	return &pb.TableGetReply{Row: data, Found: true}, nil
}

func (s *HostServiceServer) TableUpdate(ctx context.Context, req *pb.TableUpdateRequest) (*pb.ErrorReply, error) {
	row, err := tableRowFromProto(req.Row)
	if err != nil {
		return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
	}
	err = s.host.Store().Table(req.Table).Update(ctx, req.Id, row)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) TableDelete(ctx context.Context, req *pb.TableDeleteRequest) (*pb.ErrorReply, error) {
	err := s.host.Store().Table(req.Table).Delete(ctx, req.Id)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) TableList(ctx context.Context, req *pb.TableListRequest) (*pb.TableListReply, error) {
	rows, err := s.host.Store().Table(req.Table).List(ctx)
	if err != nil {
		return &pb.TableListReply{Error: pb.ErrorToProto(err)}, nil
	}
	out := make([]*pb.TableRowProto, 0, len(rows))
	for _, r := range rows {
		data, merr := tableRowToProto(r.Row)
		if merr != nil {
			return &pb.TableListReply{Error: pb.ErrorToProto(merr)}, nil
		}
		out = append(out, &pb.TableRowProto{Id: r.ID, Row: data})
	}
	return &pb.TableListReply{Rows: out}, nil
}

// tableRowFromProto decodes the wire JSON bytes of a Table row into the
// map[string]any ext.Table.Insert/Update expect. Empty bytes decode to an
// empty (non-nil) map, mirroring an empty JSON object.
func tableRowFromProto(data []byte) (map[string]any, error) {
	row := map[string]any{}
	if len(data) == 0 {
		return row, nil
	}
	if err := json.Unmarshal(data, &row); err != nil {
		return nil, fmt.Errorf("hostserver: invalid table row JSON: %w", err)
	}
	return row, nil
}

// tableRowToProto encodes a Table row back to wire JSON bytes.
func tableRowToProto(row map[string]any) ([]byte, error) {
	data, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("hostserver: cannot encode table row: %w", err)
	}
	return data, nil
}

// --- Config ---

func (s *HostServiceServer) LoadConfig(ctx context.Context, _ *pb.LoadConfigRequest) (*pb.LoadConfigReply, error) {
	cfg, err := s.host.LoadConfig(ctx)
	if err != nil {
		return &pb.LoadConfigReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.LoadConfigReply{Config: cfg}, nil
}

func (s *HostServiceServer) SaveConfig(ctx context.Context, req *pb.SaveConfigRequest) (*pb.SaveConfigReply, error) {
	err := s.host.SaveConfig(ctx, req.Config)
	return &pb.SaveConfigReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *HostServiceServer) Capabilities(ctx context.Context, _ *pb.CapabilitiesRequest) (*pb.CapabilitiesReply, error) {
	apis, err := s.host.Capabilities(ctx)
	if err != nil {
		return &pb.CapabilitiesReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.CapabilitiesReply{Apis: apis}, nil
}

// --- Auth ---

func (s *HostServiceServer) AuthLogin(ctx context.Context, req *pb.AuthLoginRequest) (*pb.PrincipalReply, error) {
	p, err := s.host.Auth().Login(ctx, req.Username, req.Password)
	if err != nil {
		return &pb.PrincipalReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.PrincipalReply{Principal: string(p)}, nil
}

func (s *HostServiceServer) AuthVerifyToken(ctx context.Context, req *pb.AuthVerifyTokenRequest) (*pb.PrincipalReply, error) {
	p, err := s.host.Auth().VerifyToken(ctx, req.Token)
	if err != nil {
		return &pb.PrincipalReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.PrincipalReply{Principal: string(p)}, nil
}

func (s *HostServiceServer) AuthVerifyAppPassword(ctx context.Context, req *pb.AuthVerifyAppPasswordRequest) (*pb.PrincipalReply, error) {
	p, err := s.host.Auth().VerifyAppPassword(ctx, req.Username, req.AppPassword)
	if err != nil {
		return &pb.PrincipalReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.PrincipalReply{Principal: string(p)}, nil
}

// --- Sign ---

// maxSignTTL caps a reverse-bridge SignLink lifetime. ttl_ms is attacker-
// supplied over the wire; time.Duration(ttlMs)*time.Millisecond overflows int64
// nanoseconds for a ttl_ms above ~292 years, wrapping NEGATIVE — and hostSign
// treats ttl<=0 as "never expires", so a huge positive ttl_ms would silently
// mint a NON-EXPIRING token (LOW-1). signTTL clamps it.
const maxSignTTL = 10 * 365 * 24 * time.Hour

// signTTL converts a wire ttl_ms into a safe duration: ttl_ms<=0 keeps the
// explicit non-expiring semantics (hostSign uses sign.NotExpired), while any
// positive value is capped at maxSignTTL so it can never overflow into
// never-expire.
func signTTL(ttlMs int64) time.Duration {
	if ttlMs <= 0 {
		return 0
	}
	if ttlMs > int64(maxSignTTL/time.Millisecond) {
		return maxSignTTL
	}
	return time.Duration(ttlMs) * time.Millisecond
}

func (s *HostServiceServer) SignLink(ctx context.Context, req *pb.SignLinkRequest) (*pb.SignLinkReply, error) {
	token, err := s.host.Sign().SignLink(ctx, ext.Principal(req.Principal), pb.RefFromProto(req.Ref), ext.SignScope(req.Scope), signTTL(req.TtlMs))
	if err != nil {
		return &pb.SignLinkReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.SignLinkReply{Token: token}, nil
}

func (s *HostServiceServer) SignVerifyLink(ctx context.Context, req *pb.SignVerifyLinkRequest) (*pb.SignVerifyLinkReply, error) {
	ref, scope, err := s.host.Sign().VerifyLink(ctx, req.Token)
	if err != nil {
		return &pb.SignVerifyLinkReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.SignVerifyLinkReply{Ref: pb.RefToProto(ref), Scope: string(scope)}, nil
}

// --- HTTP ---

func (s *HostServiceServer) HTTPDo(ctx context.Context, req *pb.HTTPDoRequest) (*pb.HTTPDoReply, error) {
	resp, err := s.host.HTTP().Do(ctx, &ext.HTTPRequest{
		Method: req.Method,
		URL:    req.Url,
		Header: pb.HeaderFromProto(req.Header),
		Body:   req.Body,
	})
	if err != nil {
		return &pb.HTTPDoReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.HTTPDoReply{
		Status: int32(resp.Status),
		Header: pb.HeaderToProto(resp.Header),
		Body:   resp.Body,
	}, nil
}
