// Package grpcplugin is the plugin-side half of the Step 9.2 gRPC executor. An
// out-of-tree storage driver binary implements ext.StorageDriver and calls
// grpcplugin.Serve(driver); the AList host launches that binary and forwards ext
// calls over gRPC. This package lives in the ext module so external driver
// authors depend only on ext.
package grpcplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// driverServer adapts an ext.StorageDriver to the generated DriverServiceServer.
// Optional capabilities are dispatched by type-asserting the driver; a missing
// capability returns NOT_IMPLEMENT so the host's declared-capability gate and the
// runtime agree.
type driverServer struct {
	pb.UnimplementedDriverServiceServer
	drv    ext.StorageDriver
	broker *plugin.GRPCBroker
}

// InitHost is called once by the host right after dispense: it carries the
// broker id the host is serving its HostService on. The plugin dials it,
// builds a host client, and delivers it to the driver via Init — this is the
// only point impl.Init is called (there is no host to give before this).
//
// initOnce (initonce.go) is what actually enforces that "only": if this
// impl is also dispensed under another Kind name sharing this process, or
// InitHost races another dispense's InitHost for the same impl, initOnce
// still runs Init exactly once and this call reuses that single outcome.
//
// impl.Init runs on this RPC's ctx (init-scoped): it is canceled once InitHost
// returns, so a driver must not retain ctx for background work started here.
func (s *driverServer) InitHost(ctx context.Context, req *pb.InitHostRequest) (*pb.ErrorReply, error) {
	err := initOnce(ctx, s.drv, func() (ext.Host, error) {
		conn, err := s.broker.Dial(req.BrokerId)
		if err != nil {
			return nil, err
		}
		return NewHostClient(conn), nil
	})
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *driverServer) GetManifest(ctx context.Context, _ *pb.Empty) (*pb.ManifestReply, error) {
	mj, err := json.Marshal(s.drv.Manifest())
	if err != nil {
		return nil, err
	}
	cj, err := json.Marshal(s.drv.DriverConfig())
	if err != nil {
		return nil, err
	}
	return &pb.ManifestReply{ManifestJson: mj, DriverConfigJson: cj}, nil
}

func (s *driverServer) NewInstance(ctx context.Context, req *pb.NewInstanceRequest) (*pb.ErrorReply, error) {
	err := s.drv.NewInstance(ctx, req.StorageId, json.RawMessage(req.Config))
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *driverServer) DropInstance(ctx context.Context, req *pb.DropInstanceRequest) (*pb.ErrorReply, error) {
	err := s.drv.DropInstance(ctx, req.StorageId)
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *driverServer) List(ctx context.Context, req *pb.ListRequest) (*pb.ListReply, error) {
	objs, err := s.drv.List(ctx, pb.RefFromProto(req.Dir), pb.ListArgsFromProto(req.Args))
	if err != nil {
		return &pb.ListReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ListReply{Objs: pb.ObjsToProto(objs)}, nil
}

func (s *driverServer) Link(ctx context.Context, req *pb.LinkRequest) (*pb.LinkReply, error) {
	link, err := s.drv.Link(ctx, pb.RefFromProto(req.File), pb.LinkArgsFromProto(req.Args))
	if err != nil {
		return &pb.LinkReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.LinkReply{Link: pb.LinkToProto(link)}, nil
}

func (s *driverServer) Get(ctx context.Context, req *pb.GetRequest) (*pb.ObjReply, error) {
	g, ok := s.drv.(ext.Getter)
	if !ok {
		return notImplObj("Get"), nil
	}
	o, err := g.Get(ctx, req.StorageId, req.Path)
	if err != nil {
		return &pb.ObjReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.ObjReply{Obj: pb.ObjToProto(o)}, nil
}

func (s *driverServer) MakeDir(ctx context.Context, req *pb.MakeDirRequest) (*pb.ObjReply, error) {
	m, ok := s.drv.(ext.Mkdirer)
	if !ok {
		return notImplObj("Mkdir"), nil
	}
	o, err := m.Mkdir(ctx, pb.RefFromProto(req.Parent), req.Name)
	return objReply(o, err), nil
}

func (s *driverServer) Rename(ctx context.Context, req *pb.RenameRequest) (*pb.ObjReply, error) {
	r, ok := s.drv.(ext.Renamer)
	if !ok {
		return notImplObj("Rename"), nil
	}
	o, err := r.Rename(ctx, pb.RefFromProto(req.Src), req.NewName)
	return objReply(o, err), nil
}

func (s *driverServer) Move(ctx context.Context, req *pb.MoveRequest) (*pb.ObjReply, error) {
	m, ok := s.drv.(ext.Mover)
	if !ok {
		return notImplObj("Move"), nil
	}
	o, err := m.Move(ctx, pb.RefFromProto(req.Src), pb.RefFromProto(req.DstDir))
	return objReply(o, err), nil
}

func (s *driverServer) Copy(ctx context.Context, req *pb.CopyRequest) (*pb.ObjReply, error) {
	c, ok := s.drv.(ext.Copier)
	if !ok {
		return notImplObj("Copy"), nil
	}
	o, err := c.Copy(ctx, pb.RefFromProto(req.Src), pb.RefFromProto(req.DstDir))
	return objReply(o, err), nil
}

func (s *driverServer) TryRapidUpload(ctx context.Context, req *pb.TryRapidUploadRequest) (*pb.TryRapidUploadReply, error) {
	rp, ok := s.drv.(ext.RapidPutter)
	if !ok {
		return &pb.TryRapidUploadReply{Error: pb.ErrorToProto(ext.NewError(ext.ErrCodeNotImplement, "TryRapidUpload"))}, nil
	}
	o, hit, err := rp.TryRapidUpload(ctx, pb.RefFromProto(req.DstDir), req.Name, req.Size, req.Hashes)
	if err != nil {
		return &pb.TryRapidUploadReply{Error: pb.ErrorToProto(err)}, nil
	}
	if !hit {
		return &pb.TryRapidUploadReply{Hit: false}, nil
	}
	return &pb.TryRapidUploadReply{Obj: pb.ObjToProto(o), Hit: true}, nil
}

func (s *driverServer) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.ErrorReply, error) {
	r, ok := s.drv.(ext.Remover)
	if !ok {
		return &pb.ErrorReply{Error: pb.ErrorToProto(ext.NewError(ext.ErrCodeNotImplement, "Remove"))}, nil
	}
	err := r.Remove(ctx, pb.RefFromProto(req.Obj))
	return &pb.ErrorReply{Error: pb.ErrorToProto(err)}, nil
}

func (s *driverServer) Other(ctx context.Context, req *pb.OtherRequest) (*pb.OtherReply, error) {
	o, ok := s.drv.(ext.Other)
	if !ok {
		return &pb.OtherReply{Error: pb.ErrorToProto(ext.NewError(ext.ErrCodeNotImplement, "Other"))}, nil
	}
	data, err := o.Other(ctx, pb.RefFromProto(req.Obj), req.Method, json.RawMessage(req.Data))
	if err != nil {
		return &pb.OtherReply{Error: pb.ErrorToProto(err)}, nil
	}
	return &pb.OtherReply{Data: data}, nil
}

func (s *driverServer) OpenRange(req *pb.OpenRangeRequest, stream pb.DriverService_OpenRangeServer) error {
	opener, ok := s.drv.(ext.RangeOpener)
	if !ok {
		return stream.Send(&pb.Chunk{Error: pb.ErrorToProto(ext.NewError(ext.ErrCodeNotImplement, "OpenRange"))})
	}
	rc, err := opener.OpenRange(stream.Context(), pb.RefFromProto(req.Ref), req.StreamId, pb.RangeFromProto(req.Range))
	if err != nil {
		return stream.Send(&pb.Chunk{Error: pb.ErrorToProto(err)})
	}
	defer rc.Close()
	buf := make([]byte, pb.ChunkSize)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.Chunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return stream.Send(&pb.Chunk{Error: pb.ErrorToProto(err)})
		}
	}
}

func (s *driverServer) Put(stream pb.DriverService_PutServer) error {
	p, ok := s.drv.(ext.Putter)
	if !ok {
		return stream.Send(&pb.PutServer{Msg: &pb.PutServer_Done{
			Done: &pb.ObjReply{Error: pb.ErrorToProto(ext.NewError(ext.ErrCodeNotImplement, "Put"))},
		}})
	}
	init, err := stream.Recv()
	if err != nil {
		return err
	}
	pi := init.GetInit()
	if pi == nil {
		return errors.New("grpcplugin: first Put message must be Init")
	}
	up := &remoteUploadStream{stream: stream, info: pb.UploadInfoFromProto(pi.Info)}
	obj, perr := p.Put(stream.Context(), pb.RefFromProto(pi.DstDir), up)
	if up.readErr != nil {
		// A transport failure while serving ReadAt already broke the stream.
		return up.readErr
	}
	if perr != nil {
		return stream.Send(&pb.PutServer{Msg: &pb.PutServer_Done{Done: &pb.ObjReply{Error: pb.ErrorToProto(perr)}}})
	}
	return stream.Send(&pb.PutServer{Msg: &pb.PutServer_Done{Done: &pb.ObjReply{Obj: pb.ObjToProto(obj)}}})
}

// remoteUploadStream is the plugin-side ext.UploadStream: ReadAt sends a
// ReadRequest to the host and blocks for the matching ReadReply (9.0 §2 pull
// model). It is single-goroutine-per-Put by construction (a driver's Put reads
// sequentially or seeks), so the request/reply pairing needs no queue.
type remoteUploadStream struct {
	stream  pb.DriverService_PutServer
	info    ext.UploadInfo
	readErr error
}

func (u *remoteUploadStream) Info() ext.UploadInfo { return u.info }

func (u *remoteUploadStream) ReadAt(p []byte, off int64) (int, error) {
	if err := u.stream.Send(&pb.PutServer{Msg: &pb.PutServer_ReadRequest{
		ReadRequest: &pb.ReadRequest{Offset: off, Length: int64(len(p))},
	}}); err != nil {
		u.readErr = err
		return 0, err
	}
	msg, err := u.stream.Recv()
	if err != nil {
		u.readErr = err
		return 0, err
	}
	rr := msg.GetReadReply()
	if rr == nil {
		u.readErr = errors.New("grpcplugin: expected ReadReply")
		return 0, u.readErr
	}
	n := copy(p, rr.Data)
	if e := pb.ErrorFromProto(rr.Error); e != nil {
		return n, e
	}
	if rr.Eof {
		return n, io.EOF
	}
	return n, nil
}

// ReportProgress lets a driver report upload progress; it rides the same Put
// stream. Drivers that don't report simply never call it. (9.2 folds progress
// into the Put bidi stream rather than a separate Host callback service.)
func (u *remoteUploadStream) ReportProgress(fraction float64) error {
	return u.stream.Send(&pb.PutServer{Msg: &pb.PutServer_Progress{Progress: &pb.Progress{Fraction: fraction}}})
}

func notImplObj(op string) *pb.ObjReply {
	return &pb.ObjReply{Error: pb.ErrorToProto(ext.NewError(ext.ErrCodeNotImplement, op))}
}

func objReply(o ext.Obj, err error) *pb.ObjReply {
	if err != nil {
		return &pb.ObjReply{Error: pb.ErrorToProto(err)}
	}
	return &pb.ObjReply{Obj: pb.ObjToProto(o)}
}
